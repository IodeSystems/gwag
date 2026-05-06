// Package controlclient is the convenience layer subservices use to
// self-register with a go-api-gateway control plane: one Register call,
// a background heartbeat goroutine, and graceful Deregister on Close.
//
//	reg, err := controlclient.SelfRegister(ctx, controlclient.Options{
//	    GatewayAddr: "gateway:50090",
//	    ServiceAddr: "greeter:50051",
//	    Services: []controlclient.Service{
//	        {Namespace: "greeter", FileDescriptor: greeterv1.File_greeter_proto},
//	    },
//	})
//	if err != nil { log.Fatal(err) }
//	defer reg.Close(context.Background())
//
// Holds an open *grpc.ClientConn to the gateway for the lifetime of
// the registration. Re-Register is automatic on heartbeat-not-ok
// (gateway evicted us, e.g. after a restart) — the goroutine fires a
// fresh Register and resumes.
package controlclient

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	cpv1 "github.com/iodesystems/go-api-gateway/controlplane/v1"
)

type Service struct {
	// Namespace under which to mount this service in the gateway's
	// GraphQL surface. Empty falls back to the proto's filename stem
	// (proto bindings) or the OpenAPI spec's Info.Title (OpenAPI
	// bindings).
	Namespace string

	// Version, e.g. "v1", "v2". Multiple versions of the same namespace
	// coexist on the gateway; latest surfaces flat under the namespace,
	// older versions appear as `vN` sub-objects with @deprecated.
	// Empty defaults to "v1". Ignored for OpenAPI bindings (single
	// version per ns in v1).
	Version string

	// FileDescriptor for the .proto file containing the service. The
	// generated bindings expose this as `pb.File_<name>_proto`.
	// Transitively-imported descriptors are walked automatically.
	//
	// Mutually exclusive with OpenAPISpec: a Service registers either
	// a proto-described gRPC service OR an OpenAPI-described HTTP
	// service.
	FileDescriptor protoreflect.FileDescriptor

	// OpenAPISpec is the raw bytes of an OpenAPI 3.x document (JSON
	// or YAML; kin-openapi parses either). When set, ServiceAddr in
	// the parent Options is the HTTP base URL the gateway dispatches
	// to (e.g. "https://billing.internal").
	OpenAPISpec []byte
}

type Options struct {
	// GatewayAddr is the gRPC host:port where the gateway's control
	// plane is listening (the WithControlPlane addr).
	GatewayAddr string

	// ServiceAddr is the host:port where this service's gRPC server
	// is reachable. Passed to the gateway verbatim.
	ServiceAddr string

	// Services to register. All bind to ServiceAddr. Empty rejected.
	Services []Service

	// InstanceID is echoed in ListRegistrations for operator debugging.
	// Free-form ("greeter@pod-abc"). Optional.
	InstanceID string

	// TTL is how long the gateway should retain the registration
	// without seeing a heartbeat. 0 → server default (30s).
	TTL time.Duration

	// HeartbeatInterval defaults to TTL/3 (or 10s if TTL is 0).
	HeartbeatInterval time.Duration

	// DialOptions are appended to the gateway-conn dial. Insecure is
	// the default if none are supplied.
	DialOptions []grpc.DialOption

	// Logger is called with non-fatal events (eviction recovery,
	// heartbeat errors). Defaults to log.Printf-equivalent.
	Logger func(format string, args ...any)
}

// Registration is the live handle returned by SelfRegister. Close
// stops the heartbeat goroutine and gracefully deregisters.
type Registration struct {
	conn   *grpc.ClientConn
	client cpv1.ControlPlaneClient

	mu     sync.Mutex
	id     string
	ttl    time.Duration
	stop   chan struct{}
	done   chan struct{}
	logger func(string, ...any)
	opts   Options
}

func SelfRegister(ctx context.Context, opts Options) (*Registration, error) {
	if opts.GatewayAddr == "" {
		return nil, errors.New("controlclient: GatewayAddr required")
	}
	if opts.ServiceAddr == "" {
		return nil, errors.New("controlclient: ServiceAddr required")
	}
	if len(opts.Services) == 0 {
		return nil, errors.New("controlclient: at least one Service required")
	}
	if opts.Logger == nil {
		opts.Logger = log.Printf
	}

	dialOpts := opts.DialOptions
	if len(dialOpts) == 0 {
		dialOpts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	conn, err := grpc.NewClient(opts.GatewayAddr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial gateway %s: %w", opts.GatewayAddr, err)
	}

	r := &Registration{
		conn:   conn,
		client: cpv1.NewControlPlaneClient(conn),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		logger: opts.Logger,
		opts:   opts,
	}

	if err := r.register(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}

	hi := opts.HeartbeatInterval
	if hi == 0 {
		if r.ttl > 0 {
			hi = r.ttl / 3
		} else {
			hi = 10 * time.Second
		}
	}
	go r.heartbeatLoop(hi)

	return r, nil
}

func (r *Registration) register(ctx context.Context) error {
	req := &cpv1.RegisterRequest{
		Addr:       r.opts.ServiceAddr,
		InstanceId: r.opts.InstanceID,
		TtlSeconds: uint32(r.opts.TTL / time.Second),
	}
	for _, s := range r.opts.Services {
		hasProto := s.FileDescriptor != nil
		hasOpenAPI := len(s.OpenAPISpec) > 0
		if hasProto && hasOpenAPI {
			return fmt.Errorf("service %q: cannot set both FileDescriptor and OpenAPISpec", s.Namespace)
		}
		if !hasProto && !hasOpenAPI {
			return fmt.Errorf("service %q: must set FileDescriptor or OpenAPISpec", s.Namespace)
		}
		if hasOpenAPI {
			req.Services = append(req.Services, &cpv1.ServiceBinding{
				Namespace:   s.Namespace,
				OpenapiSpec: s.OpenAPISpec,
			})
			continue
		}
		fdsBytes, fileName, err := descriptorSetBytes(s.FileDescriptor)
		if err != nil {
			return fmt.Errorf("descriptor for %s: %w", s.FileDescriptor.Path(), err)
		}
		req.Services = append(req.Services, &cpv1.ServiceBinding{
			Namespace:         s.Namespace,
			Version:           s.Version,
			FileDescriptorSet: fdsBytes,
			FileName:          fileName,
		})
	}

	resp, err := r.client.Register(ctx, req)
	if err != nil {
		return fmt.Errorf("Register: %w", err)
	}
	r.mu.Lock()
	r.id = resp.GetRegistrationId()
	r.ttl = time.Duration(resp.GetTtlSeconds()) * time.Second
	r.mu.Unlock()
	return nil
}

func (r *Registration) heartbeatLoop(interval time.Duration) {
	defer close(r.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.mu.Lock()
			id := r.id
			r.mu.Unlock()
			if id == "" {
				continue
			}
			resp, err := r.client.Heartbeat(context.Background(), &cpv1.HeartbeatRequest{RegistrationId: id})
			if err != nil {
				r.logger("controlclient: heartbeat error: %v", err)
				continue
			}
			if !resp.GetOk() {
				r.logger("controlclient: gateway evicted us, re-registering")
				if err := r.register(context.Background()); err != nil {
					r.logger("controlclient: re-register failed: %v", err)
				}
			}
		}
	}
}

// Close stops the heartbeat and gracefully deregisters. Safe to call
// once. ctx is used only for the Deregister call.
func (r *Registration) Close(ctx context.Context) error {
	select {
	case <-r.stop:
		return nil
	default:
	}
	close(r.stop)
	<-r.done

	r.mu.Lock()
	id := r.id
	r.mu.Unlock()
	if id != "" {
		_, _ = r.client.Deregister(ctx, &cpv1.DeregisterRequest{RegistrationId: id})
	}
	return r.conn.Close()
}

// descriptorSetBytes serialises a FileDescriptor and all its transitive
// imports into a FileDescriptorSet. Returns the bytes and the name of
// the primary file (so the gateway knows which one to mount).
func descriptorSetBytes(fd protoreflect.FileDescriptor) ([]byte, string, error) {
	fds := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var walk func(f protoreflect.FileDescriptor)
	walk = func(f protoreflect.FileDescriptor) {
		if seen[string(f.Path())] {
			return
		}
		seen[string(f.Path())] = true
		for i := 0; i < f.Imports().Len(); i++ {
			walk(f.Imports().Get(i).FileDescriptor)
		}
		fds.File = append(fds.File, protodesc.ToFileDescriptorProto(f))
	}
	walk(fd)
	b, err := proto.Marshal(fds)
	if err != nil {
		return nil, "", err
	}
	return b, string(fd.Path()), nil
}
