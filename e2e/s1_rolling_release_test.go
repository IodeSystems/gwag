//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// TestS1RollingRelease moves a provider (library) from v1 to v2 and
// its consumer (orders) from [v1] to [v2] while a steady load keeps
// calling the provider as the consumer. The story is
// examples/multi/demo.sh run by a corporate team:
//
//  1. library v1 is up, orders depends on library [v1]
//  2. a v1+v2 instance joins
//  3. the v1-only instance is drained to safe_to_stop and exits
//  4. orders moves to [v1,v2], then to [v2]
//  5. v1 becomes sunset-able; it is deprecated, then retired
//
// No call may fail at any step. Each step checks the admin graph and
// the lifecycle gauges.
func TestS1RollingRelease(t *testing.T) {
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{Environment: "e2e"})
	admin := gw.Admin()
	ctx := harness.Ctx(t, 8*time.Minute)

	libA := env.Fixture(gw, harness.FixtureOpts{Namespace: "library", Instance: "library-a", Versions: []string{"v1"}, Owner: "books", DrainOnSIGTERM: true})
	orders1 := env.Fixture(gw, harness.FixtureOpts{Namespace: "orders", Instance: "orders-1", Depends: []string{"library:v1"}, Owner: "commerce"})

	const consumer = "orders/v1" // the X-Caller-Id the orders service sends
	lib := func(ver string) harness.Labels { return harness.Labels{"namespace": "library", "version": ver} }
	with := func(l harness.Labels, kv ...string) harness.Labels {
		out := harness.Labels{}
		for k, v := range l {
			out[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	const (
		instances = "service_instances"
		drains    = "service_drain_instances"
		unmet     = "dependency_unmet"
		sunset    = "line_sunsetable"
		wait      = 20 * time.Second
	)
	ordersUnmet := harness.Labels{"consumer_namespace": "orders", "consumer_version": "v1", "provider": "library"}

	// Load: orders calls library on whichever line it currently uses,
	// and the test records which instance answered.
	var line atomic.Pointer[string]
	setLine := func(l string) { line.Store(&l) }
	setLine("v1")
	var servedMu sync.Mutex
	served := map[string]int{}
	load := harness.StartLoad(t, harness.LoadOpts{Workers: 6, Interval: 10 * time.Millisecond},
		func(ctx context.Context, _ int) error {
			e, err := libA.Echo(ctx, *line.Load(), "load", harness.Caller(consumer))
			if err != nil {
				return err
			}
			servedMu.Lock()
			served[e.Instance+"/"+e.Version]++
			servedMu.Unlock()
			return nil
		})
	servedBy := func(key string) int {
		servedMu.Lock()
		defer servedMu.Unlock()
		return served[key]
	}
	// okSince waits for n more successful calls from now.
	okSince := func(n int64) {
		t.Helper()
		ok, _ := load.Counts()
		load.WaitOK(t, ok+n, 30*time.Second)
	}

	t.Run("1 provider v1, consumer depends on [v1]", func(t *testing.T) {
		load.WaitOK(t, 100, 30*time.Second)
		gw.WaitGauge(t, instances, with(lib("v1"), "state", "ready"), 1, wait)
		gw.WaitGauge(t, instances, harness.Labels{"namespace": "orders", "version": "v1", "state": "ready"}, 1, wait)
		gw.WaitGauge(t, instances, harness.Labels{"state": "waiting"}, 0, wait)
		gw.WaitGauge(t, unmet, ordersUnmet, 0, wait)
		gw.WaitGauge(t, drains, harness.Labels{}, 0, wait)
		// The only line of a namespace is never sunset-able.
		gw.WaitGauge(t, sunset, lib("v1"), 0, wait)

		g, err := admin.Graph(ctx, "1m")
		if err != nil {
			t.Fatal(err)
		}
		if n := g.Node("library/v1"); n == nil || n.ReadyCount != 1 || n.Sunsetable {
			t.Fatalf("library/v1 node: %+v", n)
		}
		e := onlyEdge(t, g, "library")
		if e.ConsumerNamespace != "orders" || fmt.Sprint(e.Lines) != "[v1]" || !e.Met {
			t.Fatalf("desired edge: %+v, want orders -> library [v1] met", e)
		}
		if a := g.Edge(consumer, "library", "v1"); a == nil || a.OkCount == 0 {
			t.Fatalf("no active edge %s -> library/v1: %+v", consumer, g.ActiveEdges)
		}
	})

	var libB *harness.Fixture
	t.Run("2 a v1+v2 instance joins", func(t *testing.T) {
		libB = env.Fixture(gw, harness.FixtureOpts{Namespace: "library", Instance: "library-b", Versions: []string{"v1", "v2"}, Owner: "books", DrainOnSIGTERM: true})
		gw.WaitGauge(t, instances, with(lib("v1"), "state", "ready"), 2, wait)
		gw.WaitGauge(t, instances, with(lib("v2"), "state", "ready"), 1, wait)
		gw.WaitGauge(t, unmet, ordersUnmet, 0, wait)
		// v1 still has a dependent with no other line; v2 does not exist
		// to it yet.
		gw.WaitGauge(t, sunset, lib("v1"), 0, wait)

		// Both v1 replicas take load.
		harness.Eventually(t, 30*time.Second, "library-b answers v1 calls", func() error {
			if n := servedBy("library-b/v1"); n == 0 {
				return fmt.Errorf("library-b served no v1 call yet")
			}
			return nil
		})
		g, err := admin.Graph(ctx, "1m")
		if err != nil {
			t.Fatal(err)
		}
		if n := g.Node("library/v1"); n.ReadyCount != 2 || n.ReplicaCount != 2 {
			t.Fatalf("library/v1: ready %d replicas %d, want 2 2", n.ReadyCount, n.ReplicaCount)
		}
		if g.Node("library/v2") == nil {
			t.Fatal("no library/v2 node")
		}
		// A line nobody depends on and nobody calls reads as sunset-able
		// under the documented rule ("with no dependents the rule holds
		// trivially"), including the newest one. Logged, not asserted.
		t.Logf("finding: library/v2 right after it appears: sunsetable=%v reason=%q", g.Node("library/v2").Sunsetable, g.Node("library/v2").SunsetReason)
	})

	t.Run("3 drain the v1-only instance to safe_to_stop and exit", func(t *testing.T) {
		before := gw.MustScrape(t)
		resp, err := admin.DrainInstance(ctx, "library-a", false)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("drain library-a: %s %v", harness.DrainStateName(resp.GetState()), resp.GetReasons())
		admin.WaitDrainState("library-a", "safe_to_stop", 30*time.Second)
		gw.WaitGauge(t, drains, with(lib("v1"), "drain_state", "safe_to_stop"), 1, wait)
		gw.WaitGauge(t, drains, with(lib("v1"), "drain_state", "blocked"), 0, wait)
		gw.WaitGauge(t, instances, with(lib("v1"), "state", "ready"), 2, wait) // drained, not gone
		gw.WaitDeltas(t, before, "drain_requests_total", []harness.Want{{Labels: harness.Labels{"outcome": "requested"}, Delta: 1}}, wait)

		// A drained instance takes no new calls while load continues.
		calls := libA.Stats().Calls
		okSince(100)
		if got := libA.Stats().Calls; got != calls {
			t.Fatalf("library-a served %d calls after safe_to_stop", got-calls)
		}

		// SIGTERM on a drained instance: deregister and exit 0.
		if code := libA.Proc.Stop(30 * time.Second); code != 0 {
			t.Fatalf("library-a exit code %d, want 0", code)
		}
		if !strings.Contains(libA.Proc.Logs(), "drained and deregistered") {
			t.Error("library-a did not log a clean drain")
		}
		admin.WaitInstance("library", "v1", "library-a", "", wait)
		gw.WaitGauge(t, instances, with(lib("v1"), "state", "ready"), 1, wait)
		gw.WaitGauge(t, drains, with(lib("v1"), "drain_state", "safe_to_stop"), 0, wait)
		gw.WaitGauge(t, instances, harness.Labels{"namespace": "orders", "version": "v1", "state": "ready"}, 1, wait)
		gw.WaitGauge(t, unmet, ordersUnmet, 0, wait)
		gw.WaitCounterAtLeast(t, before, "start_gate_transitions_total",
			harness.Labels{"namespace": "library", "version": "v1", "from": "ready", "to": "removed"}, 1, wait)
		okSince(50)
	})

	// restartOrders runs a new orders instance with the given
	// dependency next to the old one, then retires the old one.
	restartOrders := func(t *testing.T, old *harness.Fixture, id string, lines string) *harness.Fixture {
		t.Helper()
		next := env.Fixture(gw, harness.FixtureOpts{Namespace: "orders", Instance: id, Depends: []string{"library:" + lines}, Owner: "commerce"})
		if code := old.Proc.Stop(30 * time.Second); code != 0 {
			t.Fatalf("%s exit code %d, want 0", old.Opts.Instance, code)
		}
		admin.WaitInstance("orders", "v1", old.Opts.Instance, "", wait)
		return next
	}

	var orders2 *harness.Fixture
	var lastV1Call time.Time
	t.Run("4a consumer moves to [v1,v2] and calls v2", func(t *testing.T) {
		orders2 = restartOrders(t, orders1, "orders-2", "v1,v2")
		gw.WaitGauge(t, unmet, ordersUnmet, 0, wait)
		gw.WaitGauge(t, instances, harness.Labels{"namespace": "orders", "version": "v1", "state": "ready"}, 1, wait)
		harness.Eventually(t, wait, "edge orders -> library reads [v1,v2], both present", func() error {
			g, err := admin.Graph(ctx, "1m")
			if err != nil {
				return err
			}
			e := onlyEdge(t, g, "library")
			if fmt.Sprint(e.Lines) != "[v1 v2]" || fmt.Sprint(e.PresentLines) != "[v1 v2]" || !e.Met {
				return fmt.Errorf("edge: %+v", e)
			}
			return nil
		})
		// orders now sends its calls to v2.
		setLine("v2")
		lastV1Call = time.Now() // calls already in flight may land a moment later
		okSince(100)
		harness.Eventually(t, wait, "active edge orders/v1 -> library/v2 carries ok calls", func() error {
			g, err := admin.Graph(ctx, "1m")
			if err != nil {
				return err
			}
			if e := g.Edge(consumer, "library", "v2"); e == nil || e.OkCount == 0 {
				return fmt.Errorf("no ok traffic on library/v2")
			}
			return nil
		})
		// v1 has a dependent that can use v2, but it still saw traffic
		// within the window.
		g, _ := admin.Graph(ctx, "1m")
		if n := g.Node("library/v1"); n.Sunsetable || n.TrafficCount == 0 {
			t.Fatalf("library/v1 sunsetable=%v traffic=%d (%s), want not sunset-able: calls inside the window", n.Sunsetable, n.TrafficCount, n.SunsetReason)
		}
		gw.WaitGauge(t, sunset, lib("v1"), 0, wait)
		gw.WaitGauge(t, sunset, lib("v2"), 0, wait) // v2 has traffic now
	})

	t.Run("4b consumer moves to [v2]", func(t *testing.T) {
		restartOrders(t, orders2, "orders-3", "v2")
		gw.WaitGauge(t, unmet, ordersUnmet, 0, wait)
		harness.Eventually(t, wait, "edge orders -> library reads [v2] and is met", func() error {
			g, err := admin.Graph(ctx, "1m")
			if err != nil {
				return err
			}
			e := onlyEdge(t, g, "library")
			if fmt.Sprint(e.Lines) != "[v2]" || !e.Met {
				return fmt.Errorf("edge: %+v", e)
			}
			return nil
		})
		gw.WaitGauge(t, sunset, lib("v2"), 0, wait) // the dependent has no other line
	})

	t.Run("5a v1 becomes sunset-able once its calls age out of 1m", func(t *testing.T) {
		// Admin graph, window 1m: sunset-able once no call reached v1
		// for a minute.
		start := time.Now()
		harness.Eventually(t, 150*time.Second, "library/v1 sunset-able in the 1m graph", func() error {
			g, err := admin.Graph(ctx, "1m")
			if err != nil {
				return err
			}
			n := g.Node("library/v1")
			if !n.Sunsetable || n.TrafficCount != 0 {
				return fmt.Errorf("sunsetable=%v traffic=%d: %s", n.Sunsetable, n.TrafficCount, n.SunsetReason)
			}
			return nil
		})
		t.Logf("library/v1 sunset-able in the 1m graph %s after the consumer left v1 (%s after the last v1 call)",
			time.Since(start).Round(time.Second), time.Since(lastV1Call).Round(time.Second))

		// The gauge uses the graph's default window, 1h, so the same
		// line is still not sunset-able there. Finding: a line cannot
		// read 1 on line_sunsetable within an hour of its last call.
		g1h, err := admin.Graph(ctx, "1h")
		if err != nil {
			t.Fatal(err)
		}
		if n := g1h.Node("library/v1"); n.Sunsetable {
			t.Errorf("library/v1 sunset-able in the 1h graph right after traffic: %s", n.SunsetReason)
		}
		gw.WaitGauge(t, sunset, lib("v1"), 0, wait)
		gw.WaitGauge(t, sunset, lib("v2"), 0, wait)
		t.Log("finding: line_sunsetable{library,v1} stays 0 for an hour (metric window 1h, fixed); the 1m admin graph says sunset-able")
	})

	t.Run("5b v1 deprecated with a reason, v2 not", func(t *testing.T) {
		const reason = "retiring in the Q3 release, use v2"
		if err := admin.Deprecate(ctx, "library", "v1", reason); err != nil {
			t.Fatal(err)
		}
		ss, err := admin.Services(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := ss.Service("library", "v1").ManualDeprecationReason; got != reason {
			t.Fatalf("library/v1 manualDeprecationReason %q, want %q", got, reason)
		}
		if got := ss.Service("library", "v2").ManualDeprecationReason; got != "" {
			t.Fatalf("library/v2 manualDeprecationReason %q, want none", got)
		}
		sdl, err := gw.SchemaSDL(ctx, "library")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sdl, reason) {
			t.Fatalf("SDL does not carry the deprecation reason:\n%s", sdl)
		}
		okSince(50) // deprecation does not stop calls
	})

	t.Run("6 draining the only v2 instance is blocked, then cancelled", func(t *testing.T) {
		before := gw.MustScrape(t)
		resp, err := admin.DrainInstance(ctx, "library-b", false)
		if err != nil {
			t.Fatal(err)
		}
		if harness.DrainStateName(resp.GetState()) != "blocked" {
			t.Fatalf("drain library-b: %s %v, want blocked", harness.DrainStateName(resp.GetState()), resp.GetReasons())
		}
		if len(resp.GetReasons()) == 0 {
			t.Error("blocked drain has no reasons")
		}
		admin.WaitDrainState("library-b", "blocked", wait)
		gw.WaitGauge(t, drains, with(lib("v2"), "drain_state", "blocked"), 1, wait)
		gw.WaitGauge(t, drains, with(lib("v2"), "drain_state", "safe_to_stop"), 0, wait)
		// Blocked, so it keeps serving.
		okSince(100)

		if _, err := admin.DrainInstance(ctx, "library-b", true); err != nil {
			t.Fatal(err)
		}
		admin.WaitDrainState("library-b", "serving", wait)
		gw.WaitGauge(t, drains, with(lib("v2"), "drain_state", "blocked"), 0, wait)
		gw.WaitDeltas(t, before, "drain_requests_total", []harness.Want{
			{Labels: harness.Labels{"outcome": "requested"}, Delta: 1},
			{Labels: harness.Labels{"outcome": "cancelled"}, Delta: 1},
		}, wait)
		okSince(50)
	})

	t.Run("7 retire v1: a v2-only instance replaces library-b", func(t *testing.T) {
		libC := env.Fixture(gw, harness.FixtureOpts{Namespace: "library", Instance: "library-c", Versions: []string{"v2"}, Owner: "books", DrainOnSIGTERM: true})
		gw.WaitGauge(t, instances, with(lib("v2"), "state", "ready"), 2, wait)

		resp, err := admin.DrainInstance(ctx, "library-b", false)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("drain library-b: %s %v", harness.DrainStateName(resp.GetState()), resp.GetReasons())
		admin.WaitDrainState("library-b", "safe_to_stop", 30*time.Second)
		gw.WaitGauge(t, drains, with(lib("v2"), "drain_state", "safe_to_stop"), 1, wait)
		gw.WaitGauge(t, drains, with(lib("v1"), "drain_state", "safe_to_stop"), 1, wait)
		if code := libB.Proc.Stop(30 * time.Second); code != 0 {
			t.Fatalf("library-b exit code %d, want 0", code)
		}

		// v1 is gone from the registry, the schema and the gauges; v2 serves.
		admin.WaitInstance("library", "v1", "library-b", "", wait)
		harness.Eventually(t, wait, "library/v1 is gone", func() error {
			ss, err := admin.Services(ctx)
			if err != nil {
				return err
			}
			if s := ss.Service("library", "v1"); s != nil {
				return fmt.Errorf("library/v1 still listed with %d instances", len(s.Instances))
			}
			return nil
		})
		for _, name := range []string{instances, drains, sunset} {
			gw.WaitNoSeries(t, name, lib("v1"), wait)
		}
		gw.WaitGauge(t, instances, with(lib("v2"), "state", "ready"), 1, wait)
		gw.WaitGauge(t, unmet, ordersUnmet, 0, wait)
		sdl, err := gw.SchemaSDL(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(sdl, "library_v1") || strings.Contains(sdl, "Library_v1") {
			t.Errorf("SDL still names library v1")
		}
		if _, err := libC.Echo(ctx, "v1", "gone", harness.Caller(consumer)); err == nil {
			t.Error("calling library v1 after retirement succeeded")
		}
		if _, err := libC.Echo(ctx, "v2", "still-here", harness.Caller(consumer)); err != nil {
			t.Errorf("calling library v2: %v", err)
		}
		okSince(100)
	})

	res := load.Stop()
	t.Logf("load: %s", res)
	t.Logf("served by: %v", served)
	res.RequireNoFailures(t)
}

// onlyEdge returns the one desired edge into provider.
func onlyEdge(t testing.TB, g *harness.Graph, provider string) harness.DesiredEdge {
	t.Helper()
	var out []harness.DesiredEdge
	for _, e := range g.DesiredEdges {
		if e.Provider == provider {
			out = append(out, e)
		}
	}
	if len(out) != 1 {
		t.Fatalf("desired edges into %s: %+v, want exactly one", provider, out)
	}
	return out[0]
}
