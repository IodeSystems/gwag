//go:build e2e

package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// TestHarnessLifecycle exercises the harness and fixture features the
// stories build on: the start gate, load, SIGTERM drain, SIGKILL,
// restart, and each injected fault.
func TestHarnessLifecycle(t *testing.T) {
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{})
	admin := gw.Admin()

	// A consumer whose dependency is missing waits.
	shop := env.Fixture(gw, harness.FixtureOpts{Namespace: "shop", Depends: []string{"stock:v1"}, NoWait: true})
	admin.WaitInstance("shop", "v1", shop.Opts.Instance, "waiting", 20*time.Second)

	// The provider arrives; the consumer turns ready by itself.
	stock1 := env.Fixture(gw, harness.FixtureOpts{Namespace: "stock", DrainOnSIGTERM: true})
	shop.Proc.WaitLog("FIXTURE ready", 20*time.Second)
	admin.WaitInstance("shop", "v1", shop.Opts.Instance, "ready", 20*time.Second)

	// Load across a replica swap: stock-2 joins, stock-1 drains away.
	load := harness.StartLoad(t, harness.LoadOpts{Workers: 4, Interval: 5 * time.Millisecond},
		func(ctx context.Context, _ int) error {
			_, err := stock1.Echo(ctx, "v1", "load")
			return err
		})
	load.WaitOK(t, 50, 20*time.Second)
	stock2 := env.Fixture(gw, harness.FixtureOpts{Namespace: "stock", DrainOnSIGTERM: true})
	if code := stock1.Proc.Stop(30 * time.Second); code != 0 {
		t.Fatalf("stock-1 exit code %d after SIGTERM drain, want 0", code)
	}
	stock1.Proc.WaitLog("drained and deregistered", time.Second)
	admin.WaitInstance("stock", "v1", stock1.Opts.Instance, "", 10*time.Second)
	ok, _ := load.Counts()
	load.WaitOK(t, ok+50, 20*time.Second)
	res := load.Stop()
	t.Logf("load across the drain: %s", res)
	res.RequireNoFailures(t)

	// SIGKILL: no drain, no deregister. Restart reuses the address.
	stock2.Proc.Kill()
	if code, _ := stock2.Proc.ExitCode(); code != -1 {
		t.Fatalf("stock-2 exit code %d after SIGKILL, want -1", code)
	}
	stock2.Restart()
	if e := stock2.MustEcho("v1", "back"); e.Instance != stock2.Opts.Instance {
		t.Fatalf("after restart: served by %s, want %s", e.Instance, stock2.Opts.Instance)
	}

	// Faults, changed at runtime.
	stock2.SetFaults(harness.Faults{LatencyMs: 200})
	start := time.Now()
	stock2.MustEcho("v1", "slow")
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("latency fault: call took %s, want >= 200ms", d)
	}
	stock2.SetFaults(harness.Faults{Hang: true})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	_, err := stock2.Echo(ctx, "v1", "hang")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hang fault: want the caller's deadline, got %v", err)
	}
	stock2.SetFaults(harness.Faults{ErrorRate: 0.5})
	var failed int
	for i := 0; i < 4; i++ {
		if _, err := stock2.Echo(harness.Ctx(t, 10*time.Second), "v1", "rate"); err != nil {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("error rate 0.5: %d of 4 calls failed, want 2", failed)
	}
	if s := stock2.Stats(); s.Errors != 2 {
		t.Fatalf("stats: %+v, want 2 errors", s)
	}

	// Crash after N calls: exit 3 without deregistering.
	flaky := env.Fixture(gw, harness.FixtureOpts{Namespace: "flaky", CrashAfter: 2})
	flaky.MustEcho("v1", "1")
	flaky.MustEcho("v1", "2")
	if _, err := flaky.Echo(harness.Ctx(t, 10*time.Second), "v1", "3"); err == nil {
		t.Fatal("call 3 to a crash-after-2 fixture succeeded")
	}
	if code := flaky.Proc.WaitExit(5 * time.Second); code != 3 {
		t.Fatalf("crash exit code %d, want 3", code)
	}
}

// TestHarnessCluster starts a 3-node cluster and dispatches from every
// node to a fixture registered with one of them.
func TestHarnessCluster(t *testing.T) {
	env := harness.New(t)
	c := env.Cluster(3, harness.GatewayOpts{})
	f := env.Fixture(c.Nodes[0], harness.FixtureOpts{Namespace: "hello"})
	for _, n := range c.Nodes {
		harness.Eventually(t, 30*time.Second, "echo via "+n.Name, func() error {
			e, err := f.EchoVia(harness.Ctx(t, 5*time.Second), n, "v1", n.Name)
			if err == nil && e.Instance != f.Opts.Instance {
				return errors.New("served by " + e.Instance)
			}
			return err
		})
	}
}
