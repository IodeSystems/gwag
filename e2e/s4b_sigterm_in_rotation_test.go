//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// shutdownDelayArgs is what an operator sets for a load balancer that
// probes every 100ms: keep serving past one probe so the LB sees the 503
// before the listener closes.
var shutdownDelayArgs = []string{"--shutdown-delay", "500ms"}

// TestS4bSigtermInRotation sends SIGTERM to a gateway node that is
// still in the LB's rotation, with unary requests in flight on it
// (upstream latency 1s) and more arriving until the LB's next probe
// fails. The LB only pulls the node when /api/health stops answering
// 200. Every request must succeed: those in flight finish, those that
// arrive before the probe fails are served, and the node exits 0.
func TestS4bSigtermInRotation(t *testing.T) {
	env := harness.New(t)
	cluster := env.Cluster(3, harness.GatewayOpts{Environment: "e2e", Args: shutdownDelayArgs})
	n1, n2, n3 := cluster.Nodes[0], cluster.Nodes[1], cluster.Nodes[2]

	const latency = time.Second
	orders := env.Fixture(n1, harness.FixtureOpts{Namespace: "orders", Owner: "commerce", Latency: latency})
	billing := env.Fixture(n3, harness.FixtureOpts{Kind: harness.KindOpenAPI, Namespace: "billing", Owner: "finance", Latency: latency})
	fixtures := []*harness.Fixture{orders, billing}

	ctx := harness.Ctx(t, 3*time.Minute)
	for _, g := range cluster.Nodes {
		g := g
		harness.Eventually(t, 30*time.Second, g.Name+" routes both services", func() error {
			for _, f := range fixtures {
				if _, err := f.EchoVia(ctx, g, "v1", "warm"); err != nil {
					return err
				}
			}
			return nil
		})
	}

	lb := cluster.LB(t, harness.LBOpts{ProbeInterval: 100 * time.Millisecond})
	var errMu sync.Mutex
	var firstErrs []string
	load := harness.StartLoad(t, harness.LoadOpts{Workers: 12, CallTimeout: 10 * time.Second},
		func(_ context.Context, w int) error {
			f := fixtures[w%len(fixtures)]
			// Not the load's context: Stop would cancel calls in flight.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return lb.Do(ctx, func(ctx context.Context, gw *harness.Gateway) error {
				e, err := f.EchoVia(ctx, gw, "v1", "load")
				if err == nil && e.Msg != "load" {
					err = fmt.Errorf("wrong answer %+v", e)
				}
				if err != nil {
					errMu.Lock()
					if len(firstErrs) < 10 {
						firstErrs = append(firstErrs, fmt.Sprintf("%s: %v", gw.Name, err))
					}
					errMu.Unlock()
					return fmt.Errorf("%s: %w", gw.Name, err)
				}
				return nil
			})
		})
	load.WaitOK(t, 100, 30*time.Second)
	harness.Eventually(t, 10*time.Second, "n2 gets traffic", func() error {
		if st := lb.Stats()[n2.Name]; st.OK < 20 {
			return fmt.Errorf("%+v", st)
		}
		return nil
	})

	// Still in rotation: no Pull, no WaitIdle. Calls are in flight on n2.
	if !lb.InRotation(n2) {
		t.Fatal("n2 is not in rotation before SIGTERM")
	}
	if st := lb.Stats()[n2.Name]; st.Sent-st.OK-st.Failed == 0 {
		t.Fatalf("no call in flight on n2 at SIGTERM: %+v", st)
	}
	n2.Proc.Signal(syscall.SIGTERM)
	termAt := time.Now()

	lb.WaitOutOfRotation(t, n2, 10*time.Second)
	t.Logf("n2 left the LB rotation %s after SIGTERM", time.Since(termAt).Round(time.Millisecond))
	if code := n2.Proc.WaitExit(40 * time.Second); code != 0 {
		t.Errorf("n2 exit code %d, want 0", code)
	}
	t.Logf("n2 exited %s after SIGTERM", time.Since(termAt).Round(time.Millisecond))

	// The rest of the cluster keeps serving.
	ok0, _ := load.Counts()
	load.WaitOK(t, ok0+100, 30*time.Second)

	res := load.Stop()
	t.Logf("%s; %s", res, lb.StatsString())
	st := lb.Stats()
	for _, g := range []*harness.Gateway{n1, n2, n3} {
		if s := st[g.Name]; s.Failed != 0 {
			errMu.Lock()
			t.Errorf("%s: sent=%d ok=%d failed=%d, want zero failed\nfirst errors: %v", g.Name, s.Sent, s.OK, s.Failed, firstErrs)
			errMu.Unlock()
		}
	}
	res.RequireNoFailures(t)
}
