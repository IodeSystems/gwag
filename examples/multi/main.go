// Multi-proto example: two unrelated services exposed under their own
// GraphQL namespaces in a single gateway. No middleware — just the
// thirty-second tour from the README, made runnable.
//
//	go run .
//	curl -d '{"query":"{ greeter { hello(name: \"world\") { greeting } } }"}' \
//	     http://localhost:8080/graphql
//	curl -d '{"query":"{ library { listBooks(author: \"\") { books { title author year } } } }"}' \
//	     http://localhost:8080/graphql
package main

import (
	"context"
	"log"
	"net"
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	gateway "github.com/iodesystems/go-api-gateway"
	greeterv1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/greeter/v1"
	libraryv1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/library/v1"
)

func main() {
	greeterConn := startServer("greeter", func(s *grpc.Server) {
		greeterv1.RegisterGreeterServiceServer(s, &greeterImpl{})
	})
	libraryConn := startServer("library", func(s *grpc.Server) {
		libraryv1.RegisterLibraryServiceServer(s, &libraryImpl{})
	})

	gw := gateway.New()
	mustRegister(gw, "./protos/greeter.proto", greeterConn)
	mustRegister(gw, "./protos/library.proto", libraryConn)

	addr := ":8080"
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, gw.Handler()); err != nil {
		log.Fatal(err)
	}
}

func mustRegister(gw *gateway.Gateway, path string, conn grpc.ClientConnInterface) {
	if err := gw.AddProto(path, gateway.To(conn)); err != nil {
		log.Fatalf("register %s: %v", path, err)
	}
}

func startServer(name string, register func(*grpc.Server)) *grpc.ClientConn {
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
		log.Fatal(err)
	}
	return conn
}

// ---------------------------------------------------------------------
// Toy service implementations.
// ---------------------------------------------------------------------

type greeterImpl struct {
	greeterv1.UnimplementedGreeterServiceServer
}

func (*greeterImpl) Hello(_ context.Context, req *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
	name := req.GetName()
	if name == "" {
		name = "stranger"
	}
	return &greeterv1.HelloResponse{Greeting: "Hello, " + name + "!"}, nil
}

type libraryImpl struct {
	libraryv1.UnimplementedLibraryServiceServer
}

var books = []*libraryv1.Book{
	{Id: "1", Title: "Go Programming", Author: "Alan Donovan", Year: 2015},
	{Id: "2", Title: "The Go Programming Language", Author: "Brian Kernighan", Year: 2015},
	{Id: "3", Title: "Designing Data-Intensive Applications", Author: "Martin Kleppmann", Year: 2017},
}

func (*libraryImpl) ListBooks(_ context.Context, req *libraryv1.ListBooksRequest) (*libraryv1.ListBooksResponse, error) {
	out := &libraryv1.ListBooksResponse{}
	for _, b := range books {
		if req.GetAuthor() != "" && b.Author != req.GetAuthor() {
			continue
		}
		out.Books = append(out.Books, b)
	}
	return out, nil
}

func (*libraryImpl) GetBook(_ context.Context, req *libraryv1.GetBookRequest) (*libraryv1.Book, error) {
	for _, b := range books {
		if b.Id == req.GetId() {
			return b, nil
		}
	}
	return nil, nil
}
