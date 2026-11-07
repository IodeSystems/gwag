package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Start gate: a binding with depends is WAITING (not routable) until
// each dependency has a ready instance on some accepted line.

const (
	stReady    = cpv1.InstanceState_INSTANCE_STATE_READY
	stWaiting  = cpv1.InstanceState_INSTANCE_STATE_WAITING
	stDegraded = cpv1.InstanceState_INSTANCE_STATE_DEGRADED
)

func dependsOn(b *cpv1.ServiceBinding, svc string, lines ...string) *cpv1.ServiceBinding {
	b.Depends = append(b.Depends, &cpv1.Dependency{Service: svc, Lines: lines})
	return b
}

func mustRegister(t *testing.T, g *Gateway, addr string, bs ...*cpv1.ServiceBinding) *cpv1.RegisterResponse {
	t.Helper()
	resp, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{Addr: addr, Services: bs})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return resp
}

func heartbeatStates(t *testing.T, g *Gateway, regID string) []*cpv1.BindingStatus {
	t.Helper()
	resp, err := g.ControlPlane().Heartbeat(context.Background(), &cpv1.HeartbeatRequest{RegistrationId: regID})
	if err != nil || !resp.GetOk() {
		t.Fatalf("Heartbeat: ok=%v err=%v", resp.GetOk(), err)
	}
	return resp.GetBindings()
}

func wantStates(t *testing.T, got []*cpv1.BindingStatus, want ...cpv1.InstanceState) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("bindings=%v, want %d", got, len(want))
	}
	for i, w := range want {
		if got[i].GetState() != w {
			t.Errorf("binding %d (%s/%s) state=%v, want %v", i, got[i].GetNamespace(), got[i].GetVersion(), got[i].GetState(), w)
		}
	}
}

// helloVia queries greeter.hello; returns the greeting or "" plus the
// raw body when the query fails.
func helloVia(t *testing.T, base string) (string, string) {
	t.Helper()
	q := `{"query":"{ greeter { hello(name:\"x\") { greeting } } }"}`
	resp, err := http.Post(base+"/graphql", "application/json", strings.NewReader(q))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Data struct {
			Greeter struct {
				Hello struct {
					Greeting string `json:"greeting"`
				} `json:"hello"`
			} `json:"greeter"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Errors) > 0 {
		return "", string(body)
	}
	return out.Data.Greeter.Hello.Greeting, string(body)
}

func TestStartGate_NoDependsReadyImmediately(t *testing.T) {
	greeter, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	resp := mustRegister(t, g, addr, greeterBinding(t, "v1"))
	wantStates(t, resp.GetBindings(), stReady)
	if d := resp.GetBindings()[0].GetDepends(); len(d) != 0 {
		t.Errorf("depends=%v, want none", d)
	}
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	if got, body := helloVia(t, srv.URL); got != "hello x" {
		t.Fatalf("hello: %s", body)
	}
	if greeter.helloCalls.Load() != 1 {
		t.Fatalf("helloCalls=%d", greeter.helloCalls.Load())
	}
}

func TestStartGate_WaitingUnroutableThenReady(t *testing.T) {
	greeter, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	resp := mustRegister(t, g, addr, dependsOn(greeterBinding(t, "v1"), "library", "v1"))
	wantStates(t, resp.GetBindings(), stWaiting)
	dep := resp.GetBindings()[0].GetDepends()
	if len(dep) != 1 || dep[0].GetService() != "library" || len(dep[0].GetPresentLines()) != 0 {
		t.Fatalf("depends=%v, want library missing", dep)
	}

	// Not routable: no schema field, no backend call.
	if got, _ := helloVia(t, srv.URL); got != "" {
		t.Fatalf("waiting greeter answered %q", got)
	}
	if n := greeter.helloCalls.Load(); n != 0 {
		t.Fatalf("helloCalls=%d while waiting", n)
	}
	_, info := listServiceInfo(t, g, "greeter", "v1")
	if info == nil || info.GetReplicaCount() != 0 || len(info.GetInstances()) != 1 ||
		info.GetInstances()[0].GetState() != stWaiting {
		t.Fatalf("listing=%v, want 0 replicas + 1 waiting instance", info)
	}
	if len(info.GetDepends()) != 1 {
		t.Errorf("waiting row depends=%v", info.GetDepends())
	}

	// Dependency arrives: gate opens without any action from greeter.
	lib := mustRegister(t, g, addr, libraryBinding(t, "v1"))
	wantStates(t, lib.GetBindings(), stReady)
	st := heartbeatStates(t, g, resp.GetRegistrationId())
	wantStates(t, st, stReady)
	if pl := st[0].GetDepends()[0].GetPresentLines(); !reflect.DeepEqual(pl, []string{"v1"}) {
		t.Errorf("presentLines=%v, want [v1]", pl)
	}
	if got, body := helloVia(t, srv.URL); got != "hello x" {
		t.Fatalf("hello after ready: %s", body)
	}
	_, info = listServiceInfo(t, g, "greeter", "v1")
	if info.GetReplicaCount() != 1 || info.GetInstances()[0].GetState() != stReady {
		t.Fatalf("listing after ready=%v", info)
	}
}

// A chain (greeter → library → users) opens in order once the leaf
// arrives; a binding in the same Register can satisfy another.
func TestStartGate_ChainAndSameRequest(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	gr := mustRegister(t, g, addr, dependsOn(greeterBinding(t, "v1"), "library", "v1"))
	lib := mustRegister(t, g, addr, dependsOn(libraryBinding(t, "v1"), "users", "v1"))
	wantStates(t, gr.GetBindings(), stWaiting)
	wantStates(t, lib.GetBindings(), stWaiting)

	users := libraryBinding(t, "v1")
	users.Namespace = "users"
	mustRegister(t, g, addr, users)
	wantStates(t, heartbeatStates(t, g, lib.GetRegistrationId()), stReady)
	wantStates(t, heartbeatStates(t, g, gr.GetRegistrationId()), stReady)

	// v2 depends on v1 of the same Register (listed first on purpose).
	both := mustRegister(t, g, addr, dependsOn(greeterBinding(t, "v3"), "greeter", "v2"), greeterBinding(t, "v2"))
	wantStates(t, both.GetBindings(), stReady, stReady)
}

func TestStartGate_AnyAcceptedLine(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	gr := mustRegister(t, g, addr, dependsOn(greeterBinding(t, "v1"), "library", "v1", "v2"))
	wantStates(t, gr.GetBindings(), stWaiting)
	mustRegister(t, g, addr, libraryBinding(t, "v2"))
	st := heartbeatStates(t, g, gr.GetRegistrationId())
	wantStates(t, st, stReady)
	d := st[0].GetDepends()[0]
	if !reflect.DeepEqual(d.GetLines(), []string{"v1", "v2"}) || !reflect.DeepEqual(d.GetPresentLines(), []string{"v2"}) {
		t.Errorf("dep=%v, want lines [v1 v2] present [v2]", d)
	}
}

func TestStartGate_DegradedOnLossNoCascade(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(g.Close)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	lib := mustRegister(t, g, addr, libraryBinding(t, "v1"))
	gr := mustRegister(t, g, addr, dependsOn(greeterBinding(t, "v1"), "library", "v1"))
	wantStates(t, gr.GetBindings(), stReady)

	if _, err := g.ControlPlane().Deregister(context.Background(), &cpv1.DeregisterRequest{RegistrationId: lib.GetRegistrationId()}); err != nil {
		t.Fatal(err)
	}
	wantStates(t, heartbeatStates(t, g, gr.GetRegistrationId()), stDegraded)
	if got, body := helloVia(t, srv.URL); got != "hello x" {
		t.Fatalf("degraded greeter stopped serving: %s", body)
	}

	// Admin listing carries the state string for the UI.
	rr := httptest.NewRecorder()
	newAdminRouter(t, g).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/services", nil))
	var out struct {
		Services []struct {
			Namespace string `json:"namespace"`
			Instances []struct {
				State   string `json:"state"`
				Addr    string `json:"addr"`
				Depends []struct {
					Service      string   `json:"service"`
					PresentLines []string `json:"presentLines"`
				} `json:"depends"`
			} `json:"instances"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, rr.Body.String())
	}
	var state string
	for _, s := range out.Services {
		if s.Namespace == "greeter" && len(s.Instances) == 1 {
			state = s.Instances[0].State
			if d := s.Instances[0].Depends; len(d) != 1 || d[0].Service != "library" || d[0].PresentLines == nil || len(d[0].PresentLines) != 0 {
				t.Errorf("instance depends=%v", d)
			}
		}
	}
	if state != "degraded" {
		t.Fatalf("admin state=%q, want degraded: %s", state, rr.Body.String())
	}

	// Dependency back: READY again.
	mustRegister(t, g, addr, libraryBinding(t, "v1"))
	wantStates(t, heartbeatStates(t, g, gr.GetRegistrationId()), stReady)
}

func TestStartGate_CycleRefused(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	mustRegister(t, g, addr, dependsOn(libraryBinding(t, "v1"), "greeter", "v2", "v1"))

	_, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr:     addr,
		Services: []*cpv1.ServiceBinding{dependsOn(greeterBinding(t, "v1"), "library", "v1")},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err=%v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), "greeter/v1 -> library/v1 -> greeter/v1") {
		t.Errorf("err=%v, want the cycle named", err)
	}
	if _, info := listServiceInfo(t, g, "greeter", "v1"); info != nil {
		t.Errorf("refused Register left %v", info)
	}

	// Self-cycle.
	_, err = g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr:     addr,
		Services: []*cpv1.ServiceBinding{dependsOn(greeterBinding(t, "v3"), "greeter", "v3")},
	})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "greeter/v3 -> greeter/v3") {
		t.Fatalf("self-cycle err=%v", err)
	}
}

// Cluster: greeter waits on node A; library registers on node B; A's
// gate opens through the replicated registry and both nodes route.
func TestStartGate_ClusterCrossNode(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	waitForPeers(t, a.gw, b.gw)
	greeter, addr := startGreeterServer(t)

	registerRetry := func(n *clusterNode, bs ...*cpv1.ServiceBinding) *cpv1.RegisterResponse {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var lastErr error
		for ctx.Err() == nil {
			resp, err := n.gw.ControlPlane().Register(ctx, &cpv1.RegisterRequest{Addr: addr, Services: bs})
			if err == nil {
				return resp
			}
			lastErr = err
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("Register: %v", lastErr)
		return nil
	}

	gr := registerRetry(a, dependsOn(greeterBinding(t, "v1"), "library", "v1"))
	wantStates(t, gr.GetBindings(), stWaiting)

	// Node B learns of the waiter through the registry but does not
	// route it.
	waitGate(t, 15*time.Second, "waiting instance listed on B", func() bool {
		_, info := listServiceInfo(t, b.gw, "greeter", "v1")
		return info != nil && len(info.GetInstances()) == 1 && info.GetInstances()[0].GetState() == stWaiting
	})
	for _, n := range []*clusterNode{a, b} {
		if got, _ := helloVia(t, n.httpSrv.URL); got != "" {
			t.Fatalf("waiting greeter routable: %q", got)
		}
	}

	registerRetry(b, libraryBinding(t, "v1"))
	waitGate(t, 30*time.Second, "greeter READY on A", func() bool {
		st := heartbeatStates(t, a.gw, gr.GetRegistrationId())
		return len(st) == 1 && st[0].GetState() == stReady
	})
	waitForPool(t, a.gw, "greeter", "v1", 30*time.Second)
	waitForPool(t, b.gw, "greeter", "v1", 30*time.Second)
	waitGate(t, 15*time.Second, "B lists greeter ready", func() bool {
		_, info := listServiceInfo(t, b.gw, "greeter", "v1")
		return info != nil && len(info.GetInstances()) == 1 && info.GetInstances()[0].GetState() == stReady
	})
	if got, body := helloVia(t, b.httpSrv.URL); got != "hello x" {
		t.Fatalf("hello via B: %s", body)
	}
	if greeter.helloCalls.Load() != 1 {
		t.Fatalf("helloCalls=%d", greeter.helloCalls.Load())
	}
}

func waitGate(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
