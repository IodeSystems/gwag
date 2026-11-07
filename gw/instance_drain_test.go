package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Per-instance drain (instance_drain.go).

func registerOn(t *testing.T, g *Gateway, req *cpv1.RegisterRequest) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var lastErr error
	for ctx.Err() == nil {
		resp, err := g.ControlPlane().Register(ctx, req)
		if err == nil {
			return resp.GetRegistrationId()
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("Register: %v", lastErr)
	return ""
}

func greeterReq(t *testing.T, addr, instance string, lines ...string) *cpv1.RegisterRequest {
	t.Helper()
	req := &cpv1.RegisterRequest{Addr: addr, InstanceId: instance}
	for _, l := range lines {
		req.Services = append(req.Services, greeterBinding(t, l))
	}
	return req
}

// libraryConsumerReq registers a "library" service that depends on
// greeter with the given accepted lines. Never dispatched to, so the
// addr is not dialled.
func libraryConsumerReq(t *testing.T, greeterLines ...string) *cpv1.RegisterRequest {
	t.Helper()
	return &cpv1.RegisterRequest{
		Addr:       "127.0.0.1:1",
		InstanceId: "library-0",
		Services: []*cpv1.ServiceBinding{{
			Namespace:   "library",
			Version:     "v1",
			ProtoSource: testProtoBytes(t, "library.proto"),
			Depends:     []*cpv1.Dependency{{Service: "greeter", Lines: greeterLines}},
		}},
	}
}

func drainRPC(t *testing.T, g *Gateway, instance string, cancel bool) *cpv1.DrainInstanceResponse {
	t.Helper()
	resp, err := g.ControlPlane().DrainInstance(context.Background(), &cpv1.DrainInstanceRequest{Instance: instance, Cancel: cancel})
	if err != nil {
		t.Fatalf("DrainInstance(%s): %v", instance, err)
	}
	return resp
}

// waitDrainState polls DrainInstance until it reports want.
func waitDrainState(t *testing.T, g *Gateway, instance string, want cpv1.DrainState, timeout time.Duration) *cpv1.DrainInstanceResponse {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *cpv1.DrainInstanceResponse
	for time.Now().Before(deadline) {
		last = drainRPC(t, g, instance, false)
		if last.GetState() == want {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("drain %s: state=%v reasons=%v inflight=%d, want %v", instance, last.GetState(), last.GetReasons(), last.GetInflight(), want)
	return nil
}

func drainHelloVia(t *testing.T, base string) error {
	t.Helper()
	resp, err := http.Post(base+"/graphql", "application/json",
		strings.NewReader(`{"query":"{ greeter { hello(name:\"x\") { greeting } } }"}`))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Errors []any `json:"errors"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Errors) > 0 {
		return &drainTestErr{string(body)}
	}
	return nil
}

type drainTestErr struct{ s string }

func (e *drainTestErr) Error() string { return e.s }

func newStandalone(t *testing.T) (*Gateway, *httptest.Server) {
	t.Helper()
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return g, srv
}

func TestInstanceDrain_StopsRoutingToThatInstanceOnly(t *testing.T) {
	ga, addrA := startGreeterServer(t)
	gb, addrB := startGreeterServer(t)
	g, srv := newStandalone(t)
	regA := registerOn(t, g, greeterReq(t, addrA, "greeter-a", "v1"))
	registerOn(t, g, greeterReq(t, addrB, "greeter-b", "v1"))

	// By instance_id; no dependents, nothing in flight.
	resp := waitDrainState(t, g, "greeter-a", cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP, 5*time.Second)
	if resp.GetRegistrationId() != regA || resp.GetInstanceId() != "greeter-a" {
		t.Fatalf("resolved %s/%s, want %s/greeter-a", resp.GetRegistrationId(), resp.GetInstanceId(), regA)
	}
	if got := resp.GetLines(); len(got) != 1 || got[0] != "greeter/v1" {
		t.Fatalf("lines=%v", got)
	}

	beforeA := ga.helloCalls.Load()
	for i := 0; i < 10; i++ {
		if err := drainHelloVia(t, srv.URL); err != nil {
			t.Fatalf("hello %d: %v", i, err)
		}
	}
	if d := ga.helloCalls.Load() - beforeA; d != 0 {
		t.Fatalf("draining instance got %d calls", d)
	}
	if gb.helloCalls.Load() != 10 {
		t.Fatalf("other instance got %d calls, want 10", gb.helloCalls.Load())
	}

	// Cancel: routing resumes.
	if st := drainRPC(t, g, regA, true).GetState(); st != cpv1.DrainState_DRAIN_STATE_SERVING {
		t.Fatalf("after cancel state=%v", st)
	}
	for i := 0; i < 10; i++ {
		if err := drainHelloVia(t, srv.URL); err != nil {
			t.Fatalf("hello after cancel: %v", err)
		}
	}
	if ga.helloCalls.Load() == beforeA {
		t.Fatalf("cancelled drain: instance A still gets no traffic")
	}
}

func TestInstanceDrain_LastInstanceStopsRouting(t *testing.T) {
	_, addr := startGreeterServer(t)
	g, srv := newStandalone(t)
	registerOn(t, g, greeterReq(t, addr, "greeter-a", "v1"))
	waitDrainState(t, g, "greeter-a", cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP, 5*time.Second)
	if err := drainHelloVia(t, srv.URL); err == nil {
		t.Fatalf("dispatch reached the only (draining) instance")
	}
}

func TestInstanceDrain_WaitsForInflight(t *testing.T) {
	ga, addrA := startGreeterServer(t)
	_, addrB := startGreeterServer(t)
	release := make(chan struct{})
	ga.helloFn = func(ctx context.Context, req *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
		<-release
		return &greeterv1.HelloResponse{Greeting: "late"}, nil
	}
	g, srv := newStandalone(t)
	registerOn(t, g, greeterReq(t, addrA, "greeter-a", "v1"))

	done := make(chan error, 1)
	go func() { done <- drainHelloVia(t, srv.URL) }()
	deadline := time.Now().Add(5 * time.Second)
	for ga.helloCalls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("call never reached instance A")
		}
		time.Sleep(10 * time.Millisecond)
	}
	registerOn(t, g, greeterReq(t, addrB, "greeter-b", "v1"))

	resp := drainRPC(t, g, "greeter-a", false)
	if resp.GetState() != cpv1.DrainState_DRAIN_STATE_DRAINING || resp.GetInflight() != 1 {
		t.Fatalf("state=%v inflight=%d, want DRAINING with 1 in flight", resp.GetState(), resp.GetInflight())
	}
	time.Sleep(3 * drainTick)
	if st := drainRPC(t, g, "greeter-a", false).GetState(); st != cpv1.DrainState_DRAIN_STATE_DRAINING {
		t.Fatalf("state=%v while a call is in flight", st)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("in-flight call: %v", err)
	}
	waitDrainState(t, g, "greeter-a", cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP, 5*time.Second)
}

func TestInstanceDrain_BlockedWhenLastCoveringInstance(t *testing.T) {
	ga, addrA := startGreeterServer(t)
	_, addrC := startGreeterServer(t)
	g, srv := newStandalone(t)
	registerOn(t, g, greeterReq(t, addrA, "greeter-a", "v1"))
	registerOn(t, g, libraryConsumerReq(t, "v1"))

	resp := drainRPC(t, g, "greeter-a", false)
	if resp.GetState() != cpv1.DrainState_DRAIN_STATE_BLOCKED {
		t.Fatalf("state=%v, want BLOCKED", resp.GetState())
	}
	if r := resp.GetReasons(); len(r) != 1 || !strings.Contains(r[0], "library/v1") || !strings.Contains(r[0], "greeter [v1]") {
		t.Fatalf("reasons=%v", r)
	}
	// Blocked holds the drain; routing continues.
	if err := drainHelloVia(t, srv.URL); err != nil {
		t.Fatalf("blocked drain stopped routing: %v", err)
	}
	if ga.helloCalls.Load() != 1 {
		t.Fatalf("helloCalls=%d", ga.helloCalls.Load())
	}
	time.Sleep(3 * drainTick)
	if st := drainRPC(t, g, "greeter-a", false).GetState(); st != cpv1.DrainState_DRAIN_STATE_BLOCKED {
		t.Fatalf("state=%v, want still BLOCKED", st)
	}

	// A second v1 instance covers library: the drain proceeds.
	registerOn(t, g, greeterReq(t, addrC, "greeter-c", "v1"))
	waitDrainState(t, g, "greeter-a", cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP, 5*time.Second)

	// greeter-c is now the only v1 instance not draining.
	if st := drainRPC(t, g, "greeter-c", false).GetState(); st != cpv1.DrainState_DRAIN_STATE_BLOCKED {
		t.Fatalf("greeter-c state=%v, want BLOCKED (greeter-a drained first)", st)
	}
}

// Consumer accepts [v1, v2]; v2 has a ready instance; draining the
// only v1 instance is safe.
func TestInstanceDrain_SafeWhenAnotherLineCovers(t *testing.T) {
	_, addrA := startGreeterServer(t)
	_, addrB := startGreeterServer(t)
	g, _ := newStandalone(t)
	registerOn(t, g, greeterReq(t, addrA, "greeter-v1", "v1"))
	registerOn(t, g, greeterReq(t, addrB, "greeter-v2", "v2"))
	registerOn(t, g, libraryConsumerReq(t, "v1", "v2"))

	waitDrainState(t, g, "greeter-v1", cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP, 5*time.Second)
	// The v2 instance is now the only cover.
	resp := drainRPC(t, g, "greeter-v2", false)
	if resp.GetState() != cpv1.DrainState_DRAIN_STATE_BLOCKED {
		t.Fatalf("greeter-v2 state=%v, want BLOCKED", resp.GetState())
	}
	if r := resp.GetReasons(); len(r) != 1 || !strings.Contains(r[0], "greeter [v1, v2]") {
		t.Fatalf("reasons=%v", r)
	}
}

// One instance serving v1 and v2 is drained as a unit.
func TestInstanceDrain_MultiLineInstance(t *testing.T) {
	_, addrA := startGreeterServer(t)
	g, _ := newStandalone(t)
	registerOn(t, g, greeterReq(t, addrA, "greeter-ab", "v1", "v2"))
	registerOn(t, g, libraryConsumerReq(t, "v1", "v2"))
	resp := drainRPC(t, g, "greeter-ab", false)
	if resp.GetState() != cpv1.DrainState_DRAIN_STATE_BLOCKED {
		t.Fatalf("state=%v, want BLOCKED", resp.GetState())
	}
	if got := resp.GetLines(); len(got) != 2 {
		t.Fatalf("lines=%v", got)
	}
}

func TestInstanceDrain_UnknownAndAmbiguous(t *testing.T) {
	_, addrA := startGreeterServer(t)
	_, addrB := startGreeterServer(t)
	g, _ := newStandalone(t)
	registerOn(t, g, greeterReq(t, addrA, "dup", "v1"))
	registerOn(t, g, greeterReq(t, addrB, "dup", "v1"))
	if _, err := g.ControlPlane().DrainInstance(context.Background(), &cpv1.DrainInstanceRequest{Instance: "nope"}); err == nil {
		t.Fatal("unknown instance: no error")
	}
	_, err := g.ControlPlane().DrainInstance(context.Background(), &cpv1.DrainInstanceRequest{Instance: "dup"})
	if err == nil || !strings.Contains(err.Error(), "matches 2 registrations") {
		t.Fatalf("ambiguous: err=%v", err)
	}
}

func TestInstanceDrain_DeregisterForgets(t *testing.T) {
	_, addrA := startGreeterServer(t)
	g, _ := newStandalone(t)
	regA := registerOn(t, g, greeterReq(t, addrA, "greeter-a", "v1"))
	waitDrainState(t, g, regA, cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP, 5*time.Second)
	if _, err := g.ControlPlane().Deregister(context.Background(), &cpv1.DeregisterRequest{RegistrationId: regA}); err != nil {
		t.Fatal(err)
	}
	if got := g.listInstanceDrains(""); len(got) != 0 {
		t.Fatalf("instances after deregister: %+v", got)
	}
}

// Admin routes: POST drain, GET instances, and the drains field on
// the services listing.
func TestInstanceDrain_AdminRoutes(t *testing.T) {
	_, addrA := startGreeterServer(t)
	g, _ := newStandalone(t)
	regA := registerOn(t, g, greeterReq(t, addrA, "greeter-a", "v1"))
	registerOn(t, g, libraryConsumerReq(t, "v1"))
	mux, _, err := g.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/admin/instances/greeter-a/drain", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var drained instanceDrainInfo
	_ = json.NewDecoder(resp.Body).Decode(&drained)
	resp.Body.Close()
	if resp.StatusCode != 200 || drained.State != drainStateBlocked || drained.RegistrationID != regA || len(drained.Reasons) != 1 {
		t.Fatalf("drain: code=%d %+v", resp.StatusCode, drained)
	}

	resp, err = http.Post(srv.URL+"/admin/instances/nope/drain", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown instance code=%d", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/admin/instances")
	if err != nil {
		t.Fatal(err)
	}
	var list instancesOut
	_ = json.NewDecoder(resp.Body).Decode(&list.Body)
	resp.Body.Close()
	if len(list.Body.Instances) != 2 {
		t.Fatalf("instances=%+v", list.Body.Instances)
	}

	resp, err = http.Get(srv.URL + "/admin/services")
	if err != nil {
		t.Fatal(err)
	}
	var svcs servicesOut
	_ = json.NewDecoder(resp.Body).Decode(&svcs.Body)
	resp.Body.Close()
	for _, s := range svcs.Body.Services {
		switch s.Namespace {
		case "greeter":
			if len(s.Drains) != 1 || s.Drains[0].State != drainStateBlocked {
				t.Errorf("greeter drains=%+v", s.Drains)
			}
		case "library":
			if s.Drains == nil || len(s.Drains) != 0 {
				t.Errorf("library drains=%+v, want []", s.Drains)
			}
		}
	}
}

// Cluster: a drain requested on node B stops routing on node A, and
// SAFE_TO_STOP waits for the call in flight on node A.
func TestInstanceDrain_ClusterCrossNode(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	waitForPeers(t, a.gw, b.gw)

	ga, addrA := startGreeterServer(t)
	gb, addrB := startGreeterServer(t)
	release := make(chan struct{})
	var releaseOnce bool
	t.Cleanup(func() {
		if !releaseOnce {
			close(release)
		}
	})
	ga.helloFn = func(ctx context.Context, req *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
		if req.GetName() == "hold" {
			<-release
		}
		return &greeterv1.HelloResponse{Greeting: "a"}, nil
	}
	regA := registerOn(t, a.gw, greeterReq(t, addrA, "greeter-a", "v1"))
	// Hold a call on A via node A while A is the only replica.
	waitForPool(t, a.gw, "greeter", "v1", 30*time.Second)
	held := make(chan error, 1)
	go func() {
		resp, err := http.Post(a.httpSrv.URL+"/graphql", "application/json",
			strings.NewReader(`{"query":"{ greeter { hello(name:\"hold\") { greeting } } }"}`))
		if err == nil {
			resp.Body.Close()
		}
		held <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for ga.helloCalls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("held call never reached A")
		}
		time.Sleep(10 * time.Millisecond)
	}
	registerOn(t, a.gw, greeterReq(t, addrB, "greeter-b", "v1"))
	for _, n := range []*clusterNode{a, b} {
		deadline := time.Now().Add(30 * time.Second)
		for {
			p := waitForPool(t, n.gw, "greeter", "v1", 30*time.Second)
			if p.replicaCount() == 2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("pool never reached 2 replicas")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// Node B must know the instance before it can resolve it.
	deadline = time.Now().Add(10 * time.Second)
	for {
		if _, err := b.gw.drains.resolveInstance(regA); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node B never indexed instance A")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if st := drainRPC(t, b.gw, regA, false).GetState(); st == cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP {
		t.Fatal("SAFE_TO_STOP while a call is in flight on node A")
	}

	// Routing stops on node A (the drain was requested on B).
	deadline = time.Now().Add(10 * time.Second)
	for {
		ok := false
		a.gw.mu.Lock()
		if p := a.gw.protoSlot(poolKey{namespace: "greeter", version: "v1"}); p != nil {
			for _, r := range *p.replicas.Load() {
				if r.owner == regA && r.health.draining.Load() {
					ok = true
				}
			}
		}
		a.gw.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node A never stopped routing to instance A")
		}
		time.Sleep(20 * time.Millisecond)
	}
	beforeA, beforeB := ga.helloCalls.Load(), gb.helloCalls.Load()
	for i := 0; i < 5; i++ {
		if err := drainHelloVia(t, a.httpSrv.URL); err != nil {
			t.Fatalf("hello via A: %v", err)
		}
	}
	if ga.helloCalls.Load() != beforeA || gb.helloCalls.Load() != beforeB+5 {
		t.Fatalf("calls A=%d B=%d after drain, want A unchanged, B+5", ga.helloCalls.Load()-beforeA, gb.helloCalls.Load()-beforeB)
	}

	time.Sleep(3 * drainTick)
	resp := drainRPC(t, b.gw, regA, false)
	if resp.GetState() != cpv1.DrainState_DRAIN_STATE_DRAINING || resp.GetInflight() != 1 {
		t.Fatalf("node B: state=%v inflight=%d, want DRAINING with node A's call", resp.GetState(), resp.GetInflight())
	}

	releaseOnce = true
	close(release)
	if err := <-held; err != nil {
		t.Fatalf("held call: %v", err)
	}
	waitDrainState(t, b.gw, regA, cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP, 10*time.Second)
	waitDrainState(t, a.gw, regA, cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP, 10*time.Second)
}
