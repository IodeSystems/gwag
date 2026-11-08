package controlclient

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// authCP records the authorization metadata of every call.
type authCP struct {
	cpv1.UnimplementedControlPlaneServer
	mu   sync.Mutex
	seen map[string][]string // method -> authorization values
}

func (a *authCP) note(ctx context.Context, method string) {
	md, _ := metadata.FromIncomingContext(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen[method] = append(a.seen[method], md.Get("authorization")...)
}

func (a *authCP) Register(ctx context.Context, _ *cpv1.RegisterRequest) (*cpv1.RegisterResponse, error) {
	a.note(ctx, "Register")
	return &cpv1.RegisterResponse{RegistrationId: "r1", TtlSeconds: 30}, nil
}

func (a *authCP) Heartbeat(ctx context.Context, _ *cpv1.HeartbeatRequest) (*cpv1.HeartbeatResponse, error) {
	a.note(ctx, "Heartbeat")
	return &cpv1.HeartbeatResponse{Ok: true}, nil
}

func (a *authCP) Deregister(ctx context.Context, _ *cpv1.DeregisterRequest) (*cpv1.DeregisterResponse, error) {
	a.note(ctx, "Deregister")
	return &cpv1.DeregisterResponse{}, nil
}

func (a *authCP) DrainInstance(ctx context.Context, _ *cpv1.DrainInstanceRequest) (*cpv1.DrainInstanceResponse, error) {
	a.note(ctx, "DrainInstance")
	return &cpv1.DrainInstanceResponse{State: cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP}, nil
}

func TestAppKeySentOnEveryCall(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &authCP{seen: map[string][]string{}}
	srv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(srv, a)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	var mu sync.Mutex
	key := "gwk_one"
	reg, err := SelfRegister(context.Background(), Options{
		GatewayAddr:       lis.Addr().String(),
		ServiceAddr:       "127.0.0.1:1",
		HeartbeatInterval: 20 * time.Millisecond,
		Logger:            func(string, ...any) {},
		Services:          []Service{{Namespace: "greeter", ProtoSource: []byte(`syntax = "proto3";`)}},
		AppKeyFunc: func(context.Context) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			return key, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A rotated key is picked up by the next call.
	mu.Lock()
	key = "gwk_two"
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	if err := reg.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if got := a.seen["Register"]; len(got) != 1 || got[0] != "Bearer gwk_one" {
		t.Errorf("Register authorization %v", got)
	}
	for _, m := range []string{"Heartbeat", "DrainInstance", "Deregister"} {
		got := a.seen[m]
		if len(got) == 0 || got[len(got)-1] != "Bearer gwk_two" {
			t.Errorf("%s authorization %v", m, got)
		}
	}
}
