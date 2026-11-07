package controlclient

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// fakeCP is a control plane that knows the registrations it accepted.
type fakeCP struct {
	cpv1.UnimplementedControlPlaneServer
	name string
	mu   sync.Mutex
	regs map[string]bool
	n    int
}

func (f *fakeCP) Register(_ context.Context, _ *cpv1.RegisterRequest) (*cpv1.RegisterResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("%s-%d", f.name, f.n)
	f.regs[id] = true
	return &cpv1.RegisterResponse{RegistrationId: id, TtlSeconds: 30}, nil
}

func (f *fakeCP) Heartbeat(_ context.Context, req *cpv1.HeartbeatRequest) (*cpv1.HeartbeatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &cpv1.HeartbeatResponse{Ok: f.regs[req.GetRegistrationId()]}, nil
}

func (f *fakeCP) Deregister(_ context.Context, req *cpv1.DeregisterRequest) (*cpv1.DeregisterResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.regs, req.GetRegistrationId())
	return &cpv1.DeregisterResponse{}, nil
}

func startFakeCP(t *testing.T, name string) (*fakeCP, *grpc.Server, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCP{name: name, regs: map[string]bool{}}
	srv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return f, srv, lis.Addr().String()
}

func waitFor(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var failoverProto = []byte("syntax = \"proto3\"; package greeter.v1; message Probe { string name = 1; } service GreeterService { rpc Hello(Probe) returns (Probe); }")

// A heartbeat that cannot reach the gateway moves the registration to
// the next address; a gateway that does not know the registration
// gets a fresh Register.
func TestSelfRegister_FailsOverToNextGateway(t *testing.T) {
	_, srvA, addrA := startFakeCP(t, "a")
	b, _, addrB := startFakeCP(t, "b")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reg, err := SelfRegister(ctx, Options{
		GatewayAddr:       addrA,
		GatewayAddrs:      []string{addrB},
		ServiceAddr:       "127.0.0.1:1",
		HeartbeatInterval: 50 * time.Millisecond,
		Logger:            t.Logf,
		Services:          []Service{{Namespace: "greeter", ProtoSource: failoverProto}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close(context.Background())
	if reg.GatewayAddr() != addrA || reg.ID() != "a-1" {
		t.Fatalf("registered on %s as %s, want %s as a-1", reg.GatewayAddr(), reg.ID(), addrA)
	}

	srvA.Stop()
	waitFor(t, 3*time.Second, "registration moves to b", func() bool { return reg.GatewayAddr() == addrB && reg.ID() == "b-1" })

	// b forgets it (restart): the next heartbeat registers again on b.
	b.mu.Lock()
	b.regs = map[string]bool{}
	b.mu.Unlock()
	waitFor(t, 3*time.Second, "re-registered on b", func() bool { return reg.ID() == "b-2" })
	if reg.GatewayAddr() != addrB {
		t.Fatalf("on %s, want %s", reg.GatewayAddr(), addrB)
	}
}

// SelfRegister skips an unreachable first address.
func TestSelfRegister_SkipsUnreachableGateway(t *testing.T) {
	_, _, addrB := startFakeCP(t, "b")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reg, err := SelfRegister(ctx, Options{
		GatewayAddrs: []string{"127.0.0.1:1", addrB},
		ServiceAddr:  "127.0.0.1:1",
		Logger:       t.Logf,
		Services:     []Service{{Namespace: "greeter", ProtoSource: failoverProto}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close(context.Background())
	if reg.GatewayAddr() != addrB {
		t.Fatalf("on %s, want %s", reg.GatewayAddr(), addrB)
	}
}
