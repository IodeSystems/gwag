//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// TestS3InstanceCrash runs a two-replica service under load, SIGKILLs
// the replicas one at a time (no drain, no deregister), and checks
// what callers, the admin API and /metrics show: calls move to the
// survivor with a bounded number of failures, retries and ejections
// move, and with every replica dead the consumer is DEGRADED with
// dependency_unmet=1 until a restarted replica brings it back.
//
// The fixtures register with a 3s TTL, so a killed replica leaves the
// registry after 3s plus up to one 5s janitor tick.
func TestS3InstanceCrash(t *testing.T) {
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{})
	opts := harness.FixtureOpts{Namespace: "orders", Instance: "orders-a", TTL: 3 * time.Second}
	a := env.Fixture(gw, opts)
	opts.Instance = "orders-b"
	b := env.Fixture(gw, opts)
	env.Fixture(gw, harness.FixtureOpts{Namespace: "shop", Instance: "shop-1", Depends: []string{"orders:v1"}})

	admin := gw.Admin()
	const expiry = 20 * time.Second // TTL 3s + janitor tick 5s, with room

	ordersLabels := harness.Labels{"namespace": "orders", "version": "v1"}
	instances := func(s *harness.Scrape, ns, state string) float64 {
		return s.Counter("service_instances", harness.Labels{"namespace": ns, "version": "v1", "state": state})
	}
	unmet := func(s *harness.Scrape) float64 {
		return s.Counter("dependency_unmet", harness.Labels{"consumer_namespace": "shop", "provider": "orders"})
	}

	t.Run("steady state", func(t *testing.T) {
		s := gw.MustScrape(t)
		if got := instances(s, "orders", "ready"); got != 2 {
			t.Fatalf("orders ready instances = %v, want 2\n%s", got, s.Describe("service_instances", nil))
		}
		if got := unmet(s); got != 0 {
			t.Fatalf("dependency_unmet = %v, want 0", got)
		}
		admin.WaitInstance("shop", "v1", "shop-1", "ready", 10*time.Second)
	})

	callOrders := func(ctx context.Context, _ int) error {
		_, err := a.Echo(ctx, "v1", "x", harness.Caller("shop"))
		return err
	}
	load := harness.StartLoad(t, harness.LoadOpts{Workers: 4, Interval: 5 * time.Millisecond, CallTimeout: 5 * time.Second}, callOrders)
	load.WaitOK(t, 200, 20*time.Second)
	if _, failed := load.Counts(); failed != 0 {
		t.Fatalf("failures before any kill: %d", failed)
	}
	before := gw.MustScrape(t)

	t.Run("kill replica a: traffic moves to b", func(t *testing.T) {
		a.Proc.Kill()
		bCalls := b.Stats().Calls
		ok, _ := load.Counts()
		// While a is dead but still registered, 5 straight failures
		// eject it; the gauge counts it until it leaves the registry.
		harness.Eventually(t, 10*time.Second, "replicas_ejected = 1", func() error {
			s, err := gw.Scrape(context.Background())
			if err != nil {
				return err
			}
			if got := s.Counter("replicas_ejected", ordersLabels); got != 1 {
				return fmt.Errorf("replicas_ejected = %v", got)
			}
			return nil
		})
		load.WaitOK(t, ok+300, 20*time.Second)
		harness.Eventually(t, 10*time.Second, "b serves calls after the kill", func() error {
			if got := b.Stats().Calls; got <= bCalls+100 {
				return fmt.Errorf("b calls %d (was %d)", got, bCalls)
			}
			return nil
		})
		// a stays in the registry until its TTL expires; then it goes.
		admin.WaitInstance("orders", "v1", "orders-a", "", expiry)
		_, failed := load.Counts()
		t.Logf("failed calls from the kill until a left the registry: %d", failed)
		// Design: a call that never reached a replica is re-sent to
		// the other one, so a connect-refused kill costs no failures.
		// Allow a handful for calls in flight on a when it died.
		if failed > 5 {
			t.Errorf("failed calls = %d, want <= 5 (retry design: connect failures are re-sent)\n%s", failed, load.Stop())
		}
		s := gw.MustScrape(t)
		if got := instances(s, "orders", "ready"); got != 1 {
			t.Errorf("orders ready instances = %v, want 1", got)
		}
		if got := harness.Delta(before, s, "upstream_retries_total", harness.Labels{"namespace": "orders", "version": "v1", "reason": "unavailable"}); got < 1 {
			t.Errorf("upstream_retries_total{reason=unavailable} delta = %v, want >= 1\n%s", got, s.Describe("upstream_retries_total", nil))
		} else {
			t.Logf("retries: %v", got)
		}
		if got := harness.Delta(before, s, "replica_ejections_total", ordersLabels); got < 1 {
			t.Errorf("replica_ejections_total delta = %v, want >= 1 (5 consecutive failures to a dead replica)\n%s", got, s.Describe("replica_ejections_total", nil))
		} else {
			t.Logf("ejections: %v\n%s", got, s.Describe("replica_ejections_total", nil))
		}
		if got := harness.Delta(before, s, "replica_ejections_total", harness.Labels{"namespace": "orders", "version": "v1", "replica": a.Addr}); got < 1 {
			t.Errorf("replica_ejections_total for a (%s) delta = %v, want >= 1\n%s", a.Addr, got, s.Describe("replica_ejections_total", nil))
		}
		if got := harness.Delta(before, s, "replica_ejections_total", harness.Labels{"namespace": "orders", "version": "v1", "replica": b.Addr}); got != 0 {
			t.Errorf("surviving replica b was ejected %v times", got)
		}
		t.Logf("replicas_ejected after a left: %v", s.Counter("replicas_ejected", ordersLabels))
	})

	_, failedBeforeLast := load.Counts()

	t.Run("kill the last replica", func(t *testing.T) {
		harness.Consistently(t, 500*time.Millisecond, "load still passing", func() error {
			if _, f := load.Counts(); f != failedBeforeLast {
				return fmt.Errorf("failures rose to %d with b alive", f)
			}
			return nil
		})
		load.Stop()
		failLoad := harness.StartLoad(t, harness.LoadOpts{Workers: 2, Interval: 20 * time.Millisecond, CallTimeout: 5 * time.Second}, callOrders)
		failLoad.WaitOK(t, 50, 20*time.Second)
		b.Proc.Kill()

		harness.Eventually(t, 10*time.Second, "calls fail with no replica alive", func() error {
			if _, f := failLoad.Counts(); f < 5 {
				return fmt.Errorf("failed = %d", f)
			}
			return nil
		})
		// Both replicas dead, b still registered until its TTL
		// expires. After that the provider has no ready instance.
		admin.WaitInstance("orders", "v1", "orders-b", "", expiry)
		harness.Eventually(t, 10*time.Second, "dependency_unmet=1, orders instances 0", func() error {
			s, err := gw.Scrape(context.Background())
			if err != nil {
				return err
			}
			if got := unmet(s); got != 1 {
				return fmt.Errorf("dependency_unmet = %v", got)
			}
			for _, st := range []string{"ready", "waiting", "degraded"} {
				if got := instances(s, "orders", st); got != 0 {
					return fmt.Errorf("orders %s instances = %v", st, got)
				}
			}
			return nil
		})
		// Start gate design (gw/start_gate.go): a consumer that passed
		// the gate keeps serving and is reported DEGRADED, not WAITING.
		admin.WaitInstance("shop", "v1", "shop-1", "degraded", 10*time.Second)
		s := gw.MustScrape(t)
		if got := instances(s, "shop", "degraded"); got != 1 {
			t.Errorf("shop degraded instances = %v, want 1\n%s", got, s.Describe("service_instances", nil))
		}
		if got := instances(s, "shop", "waiting"); got != 0 {
			t.Errorf("shop waiting instances = %v, want 0", got)
		}
		failLoad.Stop()
	})

	t.Run("restart a replica: traffic recovers", func(t *testing.T) {
		a.Restart()
		admin.WaitInstance("orders", "v1", "orders-a", "ready", 20*time.Second)
		rec := harness.StartLoad(t, harness.LoadOpts{Workers: 4, Interval: 5 * time.Millisecond, CallTimeout: 5 * time.Second}, callOrders)
		rec.WaitOK(t, 200, 20*time.Second)
		harness.Eventually(t, 10*time.Second, "dependency_unmet back to 0", func() error {
			s, err := gw.Scrape(context.Background())
			if err != nil {
				return err
			}
			if got := unmet(s); got != 0 {
				return fmt.Errorf("dependency_unmet = %v", got)
			}
			if got := instances(s, "orders", "ready"); got != 1 {
				return fmt.Errorf("orders ready = %v", got)
			}
			return nil
		})
		admin.WaitInstance("shop", "v1", "shop-1", "ready", 10*time.Second)
		rec.Stop().RequireNoFailures(t)
	})
}
