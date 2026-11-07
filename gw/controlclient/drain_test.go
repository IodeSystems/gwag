package controlclient

import (
	"context"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// drainCP reports BLOCKED until unblock is called, then DRAINING once,
// then SAFE_TO_STOP. It records the drain calls and the Deregister.
type drainCP struct {
	cpv1.UnimplementedControlPlaneServer
	mu           sync.Mutex
	unblocked    bool
	afterUnblock int
	drainIDs     []string
	deregistered string
	deregAt      time.Time
	safeAt       time.Time
}

func (c *drainCP) Register(context.Context, *cpv1.RegisterRequest) (*cpv1.RegisterResponse, error) {
	return &cpv1.RegisterResponse{RegistrationId: "reg-1", TtlSeconds: 30}, nil
}

func (c *drainCP) Heartbeat(context.Context, *cpv1.HeartbeatRequest) (*cpv1.HeartbeatResponse, error) {
	return &cpv1.HeartbeatResponse{Ok: true}, nil
}

func (c *drainCP) Deregister(_ context.Context, req *cpv1.DeregisterRequest) (*cpv1.DeregisterResponse, error) {
	c.mu.Lock()
	c.deregistered = req.GetRegistrationId()
	c.deregAt = time.Now()
	c.mu.Unlock()
	return &cpv1.DeregisterResponse{}, nil
}

func (c *drainCP) DrainInstance(_ context.Context, req *cpv1.DrainInstanceRequest) (*cpv1.DrainInstanceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drainIDs = append(c.drainIDs, req.GetInstance())
	resp := &cpv1.DrainInstanceResponse{RegistrationId: req.GetInstance()}
	switch {
	case !c.unblocked:
		resp.State = cpv1.DrainState_DRAIN_STATE_BLOCKED
		resp.Reasons = []string{"orders/v1 accepts greeter [v1]: no other ready instance"}
	case c.afterUnblock == 0:
		c.afterUnblock++
		resp.State = cpv1.DrainState_DRAIN_STATE_DRAINING
		resp.Inflight = 1
	default:
		resp.State = cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP
		if c.safeAt.IsZero() {
			c.safeAt = time.Now()
		}
	}
	return resp, nil
}

func startDrainCP(t *testing.T) (*drainCP, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cp := &drainCP{}
	srv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(srv, cp)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return cp, lis.Addr().String()
}

var greeterProto = []byte(`syntax = "proto3"; package greeter.v1; message P { string n = 1; } service S { rpc M(P) returns (P); }`)

// SIGTERM with DrainOnSIGTERM: drain, hold while BLOCKED, deregister
// only after SAFE_TO_STOP, then OnDrained.
func TestDrainOnSIGTERM(t *testing.T) {
	cp, addr := startDrainCP(t)
	// Keep the test process alive if the library's handler is not
	// installed (the test then fails on the OnDrained wait).
	guard := make(chan os.Signal, 4)
	signal.Notify(guard, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(guard) })

	drained := make(chan struct{})
	reg, err := SelfRegister(context.Background(), Options{
		GatewayAddr:       addr,
		ServiceAddr:       "greeter:50051",
		Services:          []Service{{Namespace: "greeter", ProtoSource: greeterProto}},
		DrainOnSIGTERM:    true,
		DrainPollInterval: 20 * time.Millisecond,
		OnDrained:         func() { close(drained) },
		Logger:            t.Logf,
	})
	if err != nil {
		t.Fatalf("SelfRegister: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// BLOCKED holds: no deregister, no OnDrained.
	time.Sleep(200 * time.Millisecond)
	cp.mu.Lock()
	n, dereg := len(cp.drainIDs), cp.deregistered
	cp.unblocked = true
	cp.mu.Unlock()
	if n < 2 || dereg != "" {
		t.Fatalf("while blocked: drain calls=%d deregistered=%q", n, dereg)
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("OnDrained never ran")
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.drainIDs[0] != "reg-1" {
		t.Fatalf("drained %q, want reg-1", cp.drainIDs[0])
	}
	if cp.deregistered != "reg-1" || cp.deregAt.Before(cp.safeAt) {
		t.Fatalf("deregistered=%q at %v, safe at %v", cp.deregistered, cp.deregAt, cp.safeAt)
	}
}

// Without DrainOnSIGTERM, Drain can be called directly; it returns
// ctx's error while blocked.
func TestDrain_ContextEndsWhileBlocked(t *testing.T) {
	_, addr := startDrainCP(t)
	reg, err := SelfRegister(context.Background(), Options{
		GatewayAddr:       addr,
		ServiceAddr:       "greeter:50051",
		Services:          []Service{{Namespace: "greeter", ProtoSource: greeterProto}},
		DrainPollInterval: 20 * time.Millisecond,
		Logger:            t.Logf,
	})
	if err != nil {
		t.Fatalf("SelfRegister: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := reg.Drain(ctx); err == nil {
		t.Fatal("Drain returned nil while blocked")
	}
}
