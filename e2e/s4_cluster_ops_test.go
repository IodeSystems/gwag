//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// TestS4ClusterOps runs a 3-node cluster under load that an LB with
// health checks spreads over the nodes. Services register through two
// different nodes' control planes and know every node's control plane
// (controlclient GatewayAddrs), so a registration moves when its node
// goes. The story kills a node (SIGKILL), brings it back, then rolls
// every node one at a time (SIGTERM, start, wait for ready), and
// requires that no call sent to a live node fails, and that no call
// fails at all during the roll. Last, it kills the only node a
// service can register through: every node drops that service.
func TestS4ClusterOps(t *testing.T) {
	env := harness.New(t)
	cluster := env.Cluster(3, harness.GatewayOpts{Environment: "e2e"})
	n1, n2, n3 := cluster.Nodes[0], cluster.Nodes[1], cluster.Nodes[2]

	// orders registers through n1, billing through n2; each fails
	// over to the other nodes.
	orders := env.Fixture(n1, harness.FixtureOpts{Namespace: "orders", Owner: "commerce",
		FailoverGateways: []*harness.Gateway{n2, n3}})
	billing := env.Fixture(n2, harness.FixtureOpts{Kind: harness.KindOpenAPI, Namespace: "billing", Owner: "finance",
		FailoverGateways: []*harness.Gateway{n3, n1}})
	fixtures := []*harness.Fixture{orders, billing}

	ctx := harness.Ctx(t, 8*time.Minute)

	// Registrations replicate: every node lists both services with a
	// ready instance before the story starts.
	for _, g := range cluster.Nodes {
		for _, f := range fixtures {
			g.Admin().WaitInstance(f.Opts.Namespace, "v1", f.Opts.Instance, "ready", 30*time.Second)
		}
	}
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

	lb := cluster.LB(t, harness.LBOpts{})
	// The first errors per node; Load keeps only 20 distinct messages
	// and the killed node's resets fill that.
	var errMu sync.Mutex
	nodeErrs := map[string][]string{}
	noteErr := func(node string, err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if len(nodeErrs[node]) < 5 {
			nodeErrs[node] = append(nodeErrs[node], fmt.Sprintf("%s: %v", time.Now().Format("15:04:05.000"), err))
		}
	}
	startLoad := func() *harness.Load {
		return harness.StartLoad(t, harness.LoadOpts{Workers: 6, Interval: 5 * time.Millisecond, CallTimeout: 10 * time.Second},
			func(_ context.Context, w int) error {
				f := fixtures[w%len(fixtures)]
				// Not the load's context: Stop would cancel calls in
				// flight, and a cancelled call is neither a gateway
				// failure nor a counted dispatch. Stop waits for them.
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				return lb.Do(ctx, func(ctx context.Context, gw *harness.Gateway) error {
					e, err := f.EchoVia(ctx, gw, "v1", "load")
					if err != nil {
						noteErr(gw.Name, err)
						return fmt.Errorf("%s: %w", gw.Name, err)
					}
					if e.Msg != "load" || e.Instance != f.Opts.Instance {
						return fmt.Errorf("%s: wrong answer %+v", gw.Name, e)
					}
					return nil
				})
			})
	}

	t.Run("node loss", func(t *testing.T) {
		survivors := []*harness.Gateway{n1, n3}
		// Converged: every node's watch has seen all 3 peers, which is
		// when the registry buckets are raised to 3 replicas. A kill
		// before that was seen once to leave a survivor listing 0
		// peers for over a minute (not reproduced on demand).
		harness.Eventually(t, 75*time.Second, "every node's cluster_peers is 3", func() error {
			for _, g := range cluster.Nodes {
				s, err := g.Scrape(ctx)
				if err != nil {
					return err
				}
				if v := s.Counter("cluster_peers", nil); v != 3 {
					return fmt.Errorf("%s cluster_peers = %v", g.Name, v)
				}
			}
			return nil
		})
		// And the registry and peers streams finished moving to 3
		// replicas. Still required with the default `odd` policy,
		// which skips R2 but not the R1 -> R3 move: a kill during it
		// leaves the stream without a quorum until the node returns.
		// Without this wait, 2 of 3 runs failed as before (8984
		// failed calls, "orders" missing from n1's schema, peers
		// notice timeout). A 3-node cluster before the move is R1:
		// the stream lives on one node, and killing that node loses
		// the registry and peers state.
		cluster.WaitStreamsReplicated(t, 3, 75*time.Second)
		before := map[string]*harness.Scrape{}
		for _, g := range survivors {
			before[g.Name] = g.MustScrape(t)
		}
		sdlBefore := mustSDL(t, ctx, n1)
		hashesBefore := serviceHashes(t, ctx, n1)

		load := startLoad()
		load.WaitOK(t, 200, 30*time.Second)
		for _, g := range cluster.Nodes {
			g := g
			harness.Eventually(t, 10*time.Second, g.Name+" gets traffic", func() error {
				if st := lb.Stats()[g.Name]; st.OK < 20 {
					return fmt.Errorf("%+v", st)
				}
				return nil
			})
		}

		// billing's registration, held by n2's control plane. It dies
		// with n2; the fixture registers again through n3 or n1.
		billingReg := func(g *harness.Gateway) []string {
			ss, err := g.Admin().Services(ctx)
			if err != nil {
				return nil
			}
			var ids []string
			if svc := ss.Service(billing.Opts.Namespace, "v1"); svc != nil {
				for _, in := range svc.Instances {
					ids = append(ids, in.RegistrationID)
				}
			}
			return ids
		}
		oldBillingReg := billingReg(n1)
		if len(oldBillingReg) != 1 {
			t.Fatalf("n1 lists billing registrations %v, want one", oldBillingReg)
		}

		n2.Proc.Kill()
		killedAt := time.Now()
		lb.WaitOutOfRotation(t, n2, 5*time.Second)
		t.Logf("n2 left the LB rotation %s after SIGKILL", time.Since(killedAt).Round(time.Millisecond))

		t.Run("peers notice", func(t *testing.T) {
			// A dead node leaves the peer list when its peer-KV entry
			// expires (30s TTL), not when its sockets close.
			harness.Eventually(t, 75*time.Second, "survivors list 2 peers", func() error {
				for _, g := range survivors {
					n, err := g.PeerCount(ctx)
					if err != nil {
						return err
					}
					if n != 2 {
						return fmt.Errorf("%s lists %d peers", g.Name, n)
					}
				}
				return nil
			})
			t.Logf("peers dropped on both survivors %s after SIGKILL", time.Since(killedAt).Round(time.Millisecond))
		})

		t.Run("services stay reachable", func(t *testing.T) {
			// Registered through the dead node: billing. Through a live
			// node: orders. Both answer on both survivors, with the
			// load still running.
			for _, g := range survivors {
				for _, f := range fixtures {
					e, err := f.EchoVia(ctx, g, "v1", "after-kill")
					if err != nil || e.Instance != f.Opts.Instance {
						t.Errorf("%s via %s: %+v, %v", f.Opts.Namespace, g.Name, e, err)
					}
				}
			}
			ok0, _ := load.Counts()
			load.WaitOK(t, ok0+200, 30*time.Second)
		})

		// Let the cluster run on two nodes for a while under load so a
		// slow failure (registration expiry, reconciler error) shows.
		okMid, _ := load.Counts()
		harness.Eventually(t, 30*time.Second, "load keeps flowing on 2 nodes", func() error {
			if ok, failed := load.Counts(); ok < okMid+500 {
				return fmt.Errorf("ok=%d failed=%d", ok, failed)
			}
			return nil
		})

		t.Run("the dead node's registration expires on every survivor", func(t *testing.T) {
			// billing moved to a survivor at once; its entries from n2
			// expire 30s after n2's last refresh and leave a delete
			// marker, so both survivors drop the old registration.
			harness.Eventually(t, 75*time.Second, "survivors list only billing's new registration", func() error {
				for _, g := range survivors {
					ids := billingReg(g)
					if len(ids) != 1 || ids[0] == oldBillingReg[0] {
						return fmt.Errorf("%s lists billing registrations %v, old %s", g.Name, ids, oldBillingReg[0])
					}
				}
				return nil
			})
			t.Logf("old billing registration gone on both survivors %s after SIGKILL", time.Since(killedAt).Round(time.Millisecond))
		})

		t.Run("services registered through the dead node are listed", func(t *testing.T) {
			for _, g := range survivors {
				ss, err := g.Admin().Services(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range fixtures {
					svc := ss.Service(f.Opts.Namespace, "v1")
					if svc == nil || svc.Instance(f.Opts.Instance) == nil || svc.Instance(f.Opts.Instance).State != "ready" {
						t.Errorf("%s: %s/v1 not listed with a ready %s instance", g.Name, f.Opts.Namespace, f.Opts.Instance)
					}
				}
			}
		})

		// The LB's health probe alone decides when n2 takes calls
		// again: /api/health turns 200 once the registry is replayed,
		// and every node holds the same registry.
		n2FailedDown := lb.Stats()[n2.Name].Failed
		n2.Proc.Restart()
		restartedAt := time.Now()
		lb.WaitInRotation(t, n2, 60*time.Second)
		t.Logf("n2 back in LB rotation %s after restart", time.Since(restartedAt).Round(time.Millisecond))
		n2.WaitReady()
		cluster.WaitPeers(3, 60*time.Second)
		sent0 := lb.Stats()[n2.Name].OK
		harness.Eventually(t, 30*time.Second, "n2 serves traffic again", func() error {
			if d := lb.Stats()[n2.Name].OK - sent0; d < 20 {
				return fmt.Errorf("%d calls", d)
			}
			return nil
		})

		t.Run("rejoined node serves the same schema", func(t *testing.T) {
			harness.Eventually(t, 60*time.Second, "n2 serves the pre-kill SDL and hashes", func() error {
				sdl, err := n2.SchemaSDL(ctx, "")
				if err != nil {
					return err
				}
				if sdl != sdlBefore {
					return fmt.Errorf("n2 SDL differs from pre-kill SDL (%d vs %d bytes)", len(sdl), len(sdlBefore))
				}
				if h := serviceHashes(t, ctx, n2); !reflect.DeepEqual(h, hashesBefore) {
					return fmt.Errorf("service hashes %v, want %v", h, hashesBefore)
				}
				return nil
			})
			for _, g := range []*harness.Gateway{n1, n3} {
				if sdl := mustSDL(t, ctx, g); sdl != sdlBefore {
					t.Errorf("%s SDL changed across the kill/restart", g.Name)
				}
			}
			for _, f := range fixtures {
				harness.Eventually(t, 30*time.Second, "n2 routes "+f.Opts.Namespace, func() error {
					e, err := f.EchoVia(ctx, n2, "v1", "rejoined")
					if err == nil && e.Instance != f.Opts.Instance {
						err = fmt.Errorf("answered by %s", e.Instance)
					}
					return err
				})
			}
		})

		res := load.Stop()
		t.Logf("node loss: %s; %s", res, lb.StatsString())

		t.Run("no failures on live nodes", func(t *testing.T) {
			st := lb.Stats()
			for _, g := range survivors {
				if s := st[g.Name]; s.Failed != 0 || s.OK == 0 {
					errMu.Lock()
					t.Errorf("%s: sent=%d ok=%d failed=%d, want zero failed\nfirst errors: %v", g.Name, s.Sent, s.OK, s.Failed, nodeErrs[g.Name])
					errMu.Unlock()
				}
			}
			// n2 after its restart, from the LB's first call to it on.
			if d := st[n2.Name].Failed - n2FailedDown; d != 0 {
				errMu.Lock()
				t.Errorf("n2 failed %d calls after its restart, want 0; first errors: %v", d, nodeErrs[n2.Name])
				errMu.Unlock()
			}
			t.Logf("calls that failed on the killed node (in flight at SIGKILL or sent before the LB noticed): %d", n2FailedDown)
		})

		t.Run("metrics", func(t *testing.T) {
			st := lb.Stats()
			for _, g := range survivors {
				g := g
				// The LB's calls plus the one direct call per service made
				// in "services stay reachable".
				want := float64(st[g.Name].OK) + float64(len(fixtures))
				g.WaitDeltas(t, before[g.Name], dispatchCount, []harness.Want{
					{Labels: harness.Labels{"code": "ok"}, Delta: want},
					{Labels: nil, Delta: want}, // nothing but ok dispatches
				}, 10*time.Second)
				s := g.MustScrape(t)
				if d := harness.Delta(before[g.Name], s, "requests_rejected_total", nil); d != 0 {
					t.Errorf("%s requests_rejected_total moved by %v\n%s", g.Name, d, s.Describe("requests_rejected_total", nil))
				}
			}
		})
	})

	t.Run("rolling upgrade", func(t *testing.T) {
		load := startLoad()
		load.WaitOK(t, 200, 30*time.Second)
		base := lb.Stats()
		sdl := mustSDL(t, ctx, n1)

		type timing struct {
			node  string
			ready time.Duration
		}
		var timings []timing
		for _, g := range cluster.Nodes {
			g := g
			// LB deregisters the node and lets its calls finish; then
			// the operator signals it.
			lb.Pull(g)
			lb.WaitIdle(t, g, 10*time.Second)
			if code := g.Proc.Stop(30 * time.Second); code != 0 {
				t.Errorf("%s exited with code %d on SIGTERM, want 0", g.Name, code)
			}
			// The LB's own probe sees the node gone. Not
			// WaitOutOfRotation: a pulled node passes it at once,
			// and Restore before a failed probe sends calls to the
			// stopped node (seen: 6 connection-refused calls).
			lb.WaitProbeFails(t, g, 5*time.Second)
			// From here the LB's health probe alone decides when the
			// node takes calls again.
			lb.Restore(g)

			ok0, _ := load.Counts()
			load.WaitOK(t, ok0+100, 30*time.Second) // traffic keeps flowing on the other two

			g.Proc.Restart()
			t0 := time.Now()
			lb.WaitInRotation(t, g, 60*time.Second)
			timings = append(timings, timing{g.Name, time.Since(t0)})
			g.WaitReady() // and the control plane accepts
			// The restarted node has a new node id; its old peer entry
			// lingers until the 30s TTL, so match on names. Converged
			// before the next node goes down.
			cluster.WaitNodes(t, cluster.Names(), 60*time.Second)

			// The node takes its share of traffic again before the
			// next one goes down.
			sent0 := lb.Stats()[g.Name].OK
			harness.Eventually(t, 30*time.Second, g.Name+" serves traffic again", func() error {
				if d := lb.Stats()[g.Name].OK - sent0; d < 20 {
					return fmt.Errorf("%d calls", d)
				}
				return nil
			})
		}

		res := load.Stop()
		t.Logf("rolling upgrade: %s; %s", res, lb.StatsString())
		for _, tm := range timings {
			t.Logf("%s: start to /api/health 200 (in LB rotation): %s", tm.node, tm.ready.Round(time.Millisecond))
			if tm.ready > 20*time.Second {
				t.Errorf("%s took %s to become ready, want under 20s", tm.node, tm.ready)
			}
		}
		res.RequireNoFailures(t)
		end := lb.Stats()
		for _, g := range cluster.Nodes {
			if d := end[g.Name].Failed - base[g.Name].Failed; d != 0 {
				errMu.Lock()
				t.Errorf("%s failed %d calls during the roll; first errors: %v", g.Name, d, nodeErrs[g.Name])
				errMu.Unlock()
			}
			if d := end[g.Name].OK - base[g.Name].OK; d == 0 {
				t.Errorf("%s served no calls during the roll", g.Name)
			}
		}

		t.Run("cluster after the roll", func(t *testing.T) {
			// Stale peer entries of the old incarnations expire.
			cluster.WaitPeers(3, 75*time.Second)
			for _, g := range cluster.Nodes {
				harness.Eventually(t, 60*time.Second, g.Name+" serves the same schema and both services", func() error {
					if s, err := g.SchemaSDL(ctx, ""); err != nil || s != sdl {
						return fmt.Errorf("SDL differs (err %v)", err)
					}
					for _, f := range fixtures {
						if _, err := f.EchoVia(ctx, g, "v1", "after-roll"); err != nil {
							return err
						}
					}
					return nil
				})
				if body, status, _ := g.Health(ctx); status != http.StatusOK || body["status"] != "serving" {
					t.Errorf("%s health = %d %v, want 200 serving", g.Name, status, body)
				}
			}
		})
	})

	t.Run("only owner node lost", func(t *testing.T) {
		// ledger knows only n3's control plane, so nothing refreshes
		// its registry entry while n3 is down.
		ledger := env.Fixture(n3, harness.FixtureOpts{Namespace: "ledger"})
		for _, g := range cluster.Nodes {
			g := g
			harness.Eventually(t, 30*time.Second, g.Name+" routes ledger", func() error {
				_, err := ledger.EchoVia(ctx, g, "v1", "before")
				return err
			})
		}
		survivors := []*harness.Gateway{n1, n2}
		listsLedger := func(g *harness.Gateway) (bool, error) {
			ss, err := g.Admin().Services(ctx)
			if err != nil {
				return false, err
			}
			return ss.Service(ledger.Opts.Namespace, "v1") != nil, nil
		}

		n3.Proc.Kill()
		killedAt := time.Now()
		// The entry expires 30s (registry TTL) after n3's last
		// refresh; its delete marker reaches every node.
		harness.Eventually(t, 75*time.Second, "both survivors drop ledger", func() error {
			for _, g := range survivors {
				listed, err := listsLedger(g)
				if err != nil {
					return err
				}
				if listed {
					return fmt.Errorf("%s still lists ledger", g.Name)
				}
				if _, err := ledger.EchoVia(ctx, g, "v1", "after-expiry"); err == nil {
					return fmt.Errorf("%s still routes ledger", g.Name)
				}
			}
			return nil
		})
		t.Logf("ledger dropped on both survivors %s after SIGKILL", time.Since(killedAt).Round(time.Millisecond))
		// Dropped for good, on both, with the same schema.
		harness.Consistently(t, 5*time.Second, "survivors agree ledger is gone", func() error {
			for _, g := range survivors {
				if listed, err := listsLedger(g); err != nil || listed {
					return fmt.Errorf("%s lists ledger (err %v)", g.Name, err)
				}
			}
			return nil
		})
		if a, b := mustSDL(t, ctx, n1), mustSDL(t, ctx, n2); a != b {
			t.Errorf("n1 and n2 serve different SDL after ledger expired")
		}
		// orders and billing know n1 and n2 too.
		for _, g := range survivors {
			for _, f := range fixtures {
				f, g := f, g
				harness.Eventually(t, 30*time.Second, g.Name+" routes "+f.Opts.Namespace, func() error {
					_, err := f.EchoVia(ctx, g, "v1", "after-n3-loss")
					return err
				})
			}
		}

		// n3 comes back; ledger's next heartbeat reaches it, finds
		// the registration unknown and registers again.
		n3.Proc.Restart()
		n3.WaitReady()
		cluster.WaitPeers(3, 75*time.Second)
		for _, g := range cluster.Nodes {
			g := g
			harness.Eventually(t, 60*time.Second, g.Name+" routes ledger again", func() error {
				_, err := ledger.EchoVia(ctx, g, "v1", "back")
				return err
			})
		}
	})
}

// TestS4HealthAfterRegistryReplay: a restarted cluster node answers
// /api/health 200 only after it has replayed the registry, so the
// first call a load balancer sends on that 200 routes. Before the fix
// it failed with `Cannot query field "orders" on type "Query"`. The
// window is the registry replay, so the test restarts the node several
// times and polls tightly.
func TestS4HealthAfterRegistryReplay(t *testing.T) {
	env := harness.New(t)
	cluster := env.Cluster(3, harness.GatewayOpts{})
	orders := env.Fixture(cluster.Nodes[0], harness.FixtureOpts{Namespace: "orders"})
	n2 := cluster.Nodes[1]
	probe := &http.Client{Timeout: time.Second}
	ctx := harness.Ctx(t, 5*time.Minute)

	for attempt := 1; attempt <= 6; attempt++ {
		n2.Proc.Restart()
		var firstErr error
		harness.Eventually(t, 30*time.Second, "n2 health 200", func() error {
			resp, err := probe.Get(n2.BaseURL() + "/api/health")
			if err != nil {
				return err
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d", resp.StatusCode)
			}
			return nil
		})
		// The first call after the first 200, exactly what an LB does.
		_, firstErr = orders.EchoVia(ctx, n2, "v1", "right-after-health")
		if firstErr != nil {
			t.Fatalf("attempt %d: /api/health was 200 but the first call failed: %v", attempt, firstErr)
		}
		n2.WaitReady()
		cluster.WaitNodes(t, cluster.Names(), 60*time.Second)
	}
}

func mustSDL(t *testing.T, ctx context.Context, g *harness.Gateway) string {
	t.Helper()
	s, err := g.SchemaSDL(ctx, "")
	if err != nil {
		t.Fatalf("%s SDL: %v", g.Name, err)
	}
	return s
}

// serviceHashes is "<ns>/<ver>=<hash>" for every service the node lists.
func serviceHashes(t *testing.T, ctx context.Context, g *harness.Gateway) []string {
	t.Helper()
	ss, err := g.Admin().Services(ctx)
	if err != nil {
		t.Fatalf("%s services: %v", g.Name, err)
	}
	var out []string
	for _, s := range ss.Services {
		out = append(out, s.Namespace+"/"+s.Version+"="+s.HashHex)
	}
	sort.Strings(out)
	return out
}
