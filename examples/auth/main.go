// Auth hide+inject example: the canonical case the library API is
// shaped around.
//
// Two gRPC services run in-process over bufconn:
//
//   - auth.v1.AuthService — resolves an opaque token into a Context blob.
//     Registered as INTERNAL: callable by hooks, not exposed externally.
//   - user.v1.UserService — public service whose RPCs embed an
//     auth.v1.Context input field.
//
// HideAndInject[*authv1.Context] strips the auth field from the
// external GraphQL surface and populates it at request time by calling
// AuthService.Resolve once per request, cached on the request context.
//
// Status: this file compiles against the API sketch in ../../gateway.go
// but the library impl that would actually serve traffic is pending.
// The example exits at the dispatch line, intentionally — see the TODO.
// It exists as the design pin: the API was built for this shape, and
// any change to the API has to keep this code compiling and readable.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	gateway "github.com/iodesystems/go-api-gateway"
	authv1 "github.com/iodesystems/go-api-gateway/examples/auth/gen/auth/v1"
	userv1 "github.com/iodesystems/go-api-gateway/examples/auth/gen/user/v1"
)

func main() {
	ctx := context.Background()

	authConn := startInProcessServer(ctx, "auth", func(s *grpc.Server) {
		authv1.RegisterAuthServiceServer(s, &authImpl{})
	})
	userConn := startInProcessServer(ctx, "user", func(s *grpc.Server) {
		userv1.RegisterUserServiceServer(s, &userImpl{})
	})

	authClient := authv1.NewAuthServiceClient(authConn)

	gw := gateway.New()

	// Internal: callable by hooks, not exposed in the public schema.
	if err := gw.AddProto("./protos/auth.proto",
		gateway.To(authConn),
		// gateway.AsInternal(),  // TODO: planned, see README
	); err != nil {
		log.Fatalf("register auth: %v", err)
	}

	// Public.
	if err := gw.AddProto("./protos/user.proto",
		gateway.To(userConn),
	); err != nil {
		log.Fatalf("register user: %v", err)
	}

	// One declaration; the schema half hides every input field of type
	// *authv1.Context, the runtime half fills them. The gateway calls
	// the resolver once per request and caches by type.
	gw.Use(gateway.HideAndInject[*authv1.Context](func(ctx context.Context) (*authv1.Context, error) {
		token := bearerFromContext(ctx)
		if token == "" {
			return nil, gateway.Reject(gateway.CodeUnauthenticated, "missing bearer token")
		}
		resp, err := authClient.Resolve(ctx, &authv1.ResolveRequest{Token: token})
		if err != nil {
			return nil, err
		}
		return resp.GetContext(), nil
	}))

	handler := gw.Handler()
	if handler == nil {
		fmt.Println(`go-api-gateway: example wired correctly against the API sketch.

The library impl that would dispatch traffic is pending — gw.Handler()
returns nil today. To make this serve real GraphQL, implement:
  - runtime .proto parsing → *graphql.Schema
  - Pair pipeline assembly (schema rewrites + per-request middleware)
  - dynamicpb dispatch through the registered grpc.ClientConn
  - HideAndInject[T]: schema field strip + per-request memoised resolver

See ../../gateway.go for the API surface this needs to satisfy.`)
		return
	}

	addr := ":8080"
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}

// ---------------------------------------------------------------------
// In-process gRPC server fixtures.
// ---------------------------------------------------------------------

func startInProcessServer(ctx context.Context, name string, register func(*grpc.Server)) *grpc.ClientConn {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	register(srv)
	go func() {
		if err := srv.Serve(lis); err != nil {
			log.Printf("%s server: %v", name, err)
		}
	}()
	conn, err := grpc.NewClient("passthrough:///"+name,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("dial %s: %v", name, err)
	}
	return conn
}

func bearerFromContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, v := range md.Get("authorization") {
		const prefix = "Bearer "
		if len(v) > len(prefix) && v[:len(prefix)] == prefix {
			return v[len(prefix):]
		}
	}
	return ""
}

// ---------------------------------------------------------------------
// Service stubs. Toy logic — the example is about the gateway wiring,
// not the auth model.
// ---------------------------------------------------------------------

type authImpl struct {
	authv1.UnimplementedAuthServiceServer
}

func (*authImpl) Resolve(_ context.Context, req *authv1.ResolveRequest) (*authv1.ResolveResponse, error) {
	if req.GetToken() == "deny" {
		return nil, errors.New("denied")
	}
	return &authv1.ResolveResponse{
		Context: &authv1.Context{
			UserId:   "u_" + req.GetToken(),
			TenantId: "t_demo",
		},
	}, nil
}

type userImpl struct {
	userv1.UnimplementedUserServiceServer
}

func (*userImpl) GetMe(_ context.Context, req *userv1.GetMeRequest) (*userv1.GetMeResponse, error) {
	a := req.GetAuth()
	if a == nil {
		return nil, errors.New("missing auth context — gateway should have injected it")
	}
	return &userv1.GetMeResponse{
		Id:       a.GetUserId(),
		Name:     "Demo User",
		TenantId: a.GetTenantId(),
	}, nil
}
