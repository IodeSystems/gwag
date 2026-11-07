package controlclient_test

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	gateway "github.com/iodesystems/gwag/v2/gw"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

func simpleProto(pkg string) []byte {
	return []byte(fmt.Sprintf(`syntax = "proto3"; package %s.v1; message P { string n = 1; } service S { rpc M(P) returns (P); }`, pkg))
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logSink) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.lines {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

func serveGateway(t *testing.T, g *gateway.Gateway, lis net.Listener) {
	t.Helper()
	srv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(srv, g.ControlPlane())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
}

type result struct {
	reg *controlclient.Registration
	err error
}

func TestSelfRegister_BlocksUntilDependsPresent(t *testing.T) {
	g := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure())
	t.Cleanup(g.Close)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveGateway(t, g, lis)

	logs := &logSink{}
	done := make(chan result, 1)
	go func() {
		reg, err := controlclient.SelfRegister(context.Background(), controlclient.Options{
			GatewayAddr:       lis.Addr().String(),
			ServiceAddr:       "127.0.0.1:1",
			ReadyPollInterval: 20 * time.Millisecond,
			Logger:            logs.logf,
			Services: []controlclient.Service{{
				Namespace:   "orders",
				Version:     "v1",
				ProtoSource: simpleProto("orders"),
				Depends:     []controlclient.Dependency{{Service: "library", Lines: []string{"v1", "v2"}}},
			}},
		})
		done <- result{reg, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("SelfRegister returned while library absent: err=%v", r.err)
	case <-time.After(300 * time.Millisecond):
	}
	if !logs.contains("waiting for dependencies: orders/v1 needs library(v1|v2)") {
		t.Errorf("no waiting log: %v", logs.lines)
	}

	if _, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr: "127.0.0.1:1",
		Services: []*cpv1.ServiceBinding{{
			Namespace: "library", Version: "v2", ProtoSource: simpleProto("library"),
		}},
	}); err != nil {
		t.Fatalf("Register library: %v", err)
	}

	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SelfRegister did not return after library registered")
	}
	if r.err != nil {
		t.Fatalf("SelfRegister: %v", r.err)
	}
	t.Cleanup(func() { _ = r.reg.Close(context.Background()) })
	b := r.reg.Bindings()
	if len(b) != 1 || b[0].State != controlclient.StateReady {
		t.Fatalf("bindings=%+v, want one ready", b)
	}
	if got := r.reg.PresentLines("library"); !reflect.DeepEqual(got, []string{"v2"}) {
		t.Errorf("PresentLines=%v, want [v2]", got)
	}
}

// Fail closed: the gateway is down at start; SelfRegister retries
// until it comes up instead of returning the first error.
func TestSelfRegister_RetriesUntilGatewayUp(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	logs := &logSink{}
	done := make(chan result, 1)
	go func() {
		reg, err := controlclient.SelfRegister(context.Background(), controlclient.Options{
			GatewayAddr: addr,
			ServiceAddr: "127.0.0.1:1",
			Logger:      logs.logf,
			Services:    []controlclient.Service{{Namespace: "orders", Version: "v1", ProtoSource: simpleProto("orders")}},
		})
		done <- result{reg, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("SelfRegister returned with gateway down: err=%v", r.err)
	case <-time.After(600 * time.Millisecond):
	}
	if !logs.contains("unreachable, retrying") {
		t.Errorf("no retry log: %v", logs.lines)
	}

	g := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure())
	t.Cleanup(g.Close)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("re-listen %s: %v", addr, err)
	}
	serveGateway(t, g, lis)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("SelfRegister: %v", r.err)
		}
		_ = r.reg.Close(context.Background())
	case <-time.After(10 * time.Second):
		t.Fatal("SelfRegister did not return after gateway came up")
	}
}

// A refused Register (dependency cycle) is returned, not retried.
func TestSelfRegister_CycleReturnsError(t *testing.T) {
	g := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure())
	t.Cleanup(g.Close)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveGateway(t, g, lis)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = controlclient.SelfRegister(ctx, controlclient.Options{
		GatewayAddr: lis.Addr().String(),
		ServiceAddr: "127.0.0.1:1",
		Logger:      func(string, ...any) {},
		Services: []controlclient.Service{{
			Namespace: "orders", Version: "v1", ProtoSource: simpleProto("orders"),
			Depends: []controlclient.Dependency{{Service: "orders", Lines: []string{"v1"}}},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "orders/v1 -> orders/v1") || ctx.Err() != nil {
		t.Fatalf("err=%v ctxErr=%v, want cycle error before ctx end", err, ctx.Err())
	}
}
