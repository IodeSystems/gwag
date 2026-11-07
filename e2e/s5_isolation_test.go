//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// TestS5Isolation gives service A a small concurrency cap, a short
// wait budget and an upstream timeout, then makes A slow and then
// hung while service B takes its own load. Per-pool isolation says A
// backs up A's pool only: B sees no failures and about its baseline
// latency, and A's failures are timeouts and backpressure rejections,
// not ingress rejections.
func TestS5Isolation(t *testing.T) {
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{Args: []string{
		"--upstream-timeout", "1s",
		"--max-wait", "300ms",
	}})
	a := env.Fixture(gw, harness.FixtureOpts{Namespace: "slow", MaxConcurrency: 2})
	b := env.Fixture(gw, harness.FixtureOpts{Namespace: "fast"})

	aLbl := harness.Labels{"namespace": "slow", "version": "v1"}
	bLbl := harness.Labels{"namespace": "fast", "version": "v1"}

	var bLat, aLat harness.Latencies
	bLoad := harness.StartLoad(t, harness.LoadOpts{Workers: 4, Interval: 5 * time.Millisecond, CallTimeout: 5 * time.Second},
		func(ctx context.Context, _ int) error {
			return bLat.Time(func() error { _, err := b.Echo(ctx, "v1", "b", harness.Caller("s5/b")); return err })
		})
	bLoad.WaitOK(t, 500, 30*time.Second)

	var baseP99 time.Duration
	t.Run("baseline: B alone", func(t *testing.T) {
		bLat.Reset()
		bLoad.WaitOK(t, bLoadOK(bLoad)+1000, 30*time.Second)
		baseP99 = bLat.Percentile(99)
		t.Logf("B baseline: n=%d p50=%s p99=%s", bLat.Len(), bLat.Percentile(50), baseP99)
		if baseP99 > 100*time.Millisecond {
			t.Fatalf("baseline p99 %s is too high to compare against", baseP99)
		}
		bLat.Reset()
	})
	// Bound: 3x baseline plus 15ms, floor of 25ms.
	bound := 3*baseP99 + 15*time.Millisecond
	if bound < 25*time.Millisecond {
		bound = 25 * time.Millisecond
	}

	before := gw.MustScrape(t)
	_, bFailed0 := bLoad.Counts()

	// A's callers: more workers than A's cap of 2, each call bounded
	// by the client only (5s), so the gateway's limits decide.
	var aErrs errCount
	aLoad := harness.StartLoad(t, harness.LoadOpts{Workers: 8, CallTimeout: 5 * time.Second},
		func(ctx context.Context, _ int) error {
			return aLat.Time(func() error {
				_, err := a.Echo(ctx, "v1", "a", harness.Caller("s5/a"))
				aErrs.add(err)
				return err
			})
		})

	checkB := func(t *testing.T, phase string) {
		t.Helper()
		if _, f := bLoad.Counts(); f != bFailed0 {
			t.Errorf("%s: B had %d failed calls, want 0", phase, f-bFailed0)
		}
		p99 := bLat.Percentile(99)
		t.Logf("%s: B n=%d p50=%s p99=%s (baseline p99 %s, bound %s)", phase, bLat.Len(), bLat.Percentile(50), p99, baseP99, bound)
		if p99 > bound {
			t.Errorf("%s: B p99 %s exceeds bound %s (baseline %s): cross-pool interference", phase, p99, bound, baseP99)
		}
	}
	queueDepth := func(s *harness.Scrape, ns string) float64 {
		return s.Counter("pool_queue_depth", harness.Labels{"namespace": ns, "version": "v1"})
	}

	t.Run("A slow: backpressure on A only", func(t *testing.T) {
		a.SetFaults(harness.Faults{LatencyMs: 600})
		bLat.Reset()
		harness.Eventually(t, 20*time.Second, "A's queue fills and its pool refuses", func() error {
			s, err := gw.Scrape(context.Background())
			if err != nil {
				return err
			}
			if queueDepth(s, "slow") < 1 {
				return fmt.Errorf("pool_queue_depth{slow} = %v", queueDepth(s, "slow"))
			}
			if got := harness.Delta(before, s, "pool_backoff_total", aLbl); got < 3 {
				return fmt.Errorf("pool_backoff_total{slow} delta = %v", got)
			}
			return nil
		})
		bLoad.WaitOK(t, bLoadOK(bLoad)+1000, 30*time.Second)
		checkB(t, "A slow")
		if n := aErrs.matching("could not acquire"); n == 0 {
			t.Errorf("no A call failed with a slot-wait rejection; A errors: %s", &aErrs)
		}
		if got := harness.Delta(before, gw.MustScrape(t), "upstream_timeouts_total", aLbl); got != 0 {
			t.Errorf("upstream_timeouts_total{slow} moved by %v while A answers in 600ms (< 1s)", got)
		}
	})

	t.Run("A hung: timeouts on A only", func(t *testing.T) {
		a.SetFaults(harness.Faults{Hang: true})
		bLat.Reset()
		harness.Eventually(t, 20*time.Second, "A's calls time out", func() error {
			s, err := gw.Scrape(context.Background())
			if err != nil {
				return err
			}
			if got := harness.Delta(before, s, "upstream_timeouts_total", aLbl); got < 3 {
				return fmt.Errorf("upstream_timeouts_total{slow} delta = %v", got)
			}
			return nil
		})
		bLoad.WaitOK(t, bLoadOK(bLoad)+1000, 30*time.Second)
		checkB(t, "A hung")
		if n := aErrs.matching("upstream timeout"); n == 0 {
			t.Errorf("no A call failed with 'upstream timeout'; A errors: %s", &aErrs)
		}
		s := gw.MustScrape(t)
		if got := queueDepth(s, "slow"); got < 1 {
			t.Errorf("pool_queue_depth{slow} = %v while A hangs under 8 workers, want >= 1", got)
		}
	})

	t.Run("isolation holds in metrics", func(t *testing.T) {
		s := gw.MustScrape(t)
		for _, name := range []string{"pool_backoff_total", "upstream_timeouts_total", "upstream_retries_total", "replica_ejections_total"} {
			if got := harness.Delta(before, s, name, bLbl); got != 0 {
				t.Errorf("%s{fast} moved by %v\n%s", name, got, s.Describe(name, nil))
			}
		}
		if got := queueDepth(s, "fast"); got != 0 {
			t.Errorf("pool_queue_depth{fast} = %v", got)
		}
		// A's trouble is upstream, not ingress: nothing was refused
		// at the front door.
		if got := harness.Delta(before, s, "requests_rejected_total", nil); got != 0 {
			t.Errorf("requests_rejected_total moved by %v, want 0 (upstream backpressure is not ingress)\n%s", got, s.Describe("requests_rejected_total", nil))
		}
	})

	t.Run("A recovers: gauges return to 0", func(t *testing.T) {
		a.SetFaults(harness.Faults{})
		aOK, _ := aLoad.Counts()
		aLoad.WaitOK(t, aOK+50, 30*time.Second)
		aLoad.Stop()
		harness.Eventually(t, 30*time.Second, "pool_queue_depth and replicas_ejected return to 0", func() error {
			s, err := gw.Scrape(context.Background())
			if err != nil {
				return err
			}
			for _, ns := range []string{"slow", "fast"} {
				if got := queueDepth(s, ns); got != 0 {
					return fmt.Errorf("pool_queue_depth{%s} = %v", ns, got)
				}
			}
			if got := s.Counter("replicas_ejected", nil); got != 0 {
				return fmt.Errorf("replicas_ejected = %v\n%s", got, s.Describe("replicas_ejected", nil))
			}
			return nil
		})
		if _, err := a.Echo(harness.Ctx(t, 10*time.Second), "v1", "after", harness.Caller("s5/a")); err != nil {
			t.Errorf("A call after recovery: %v", err)
		}
		bLoad.Stop().RequireNoFailures(t)
	})
}

func bLoadOK(l *harness.Load) int64 { ok, _ := l.Counts(); return ok }

// errCount tallies error messages (first 200 chars).
type errCount struct {
	mu sync.Mutex
	m  map[string]int
}

func (e *errCount) add(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.m == nil {
		e.m = map[string]int{}
	}
	e.m[msg]++
}

func (e *errCount) matching(sub string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for k, v := range e.m {
		if strings.Contains(k, sub) {
			n += v
		}
	}
	return n
}

func (e *errCount) String() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return fmt.Sprint(e.m)
}
