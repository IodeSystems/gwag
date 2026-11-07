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
// different nodes' control planes. The story kills a node (SIGKILL),
// brings it back, then rolls every node one at a time (SIGTERM, start,
// wait for ready), and requires that no call sent to a node that stayed
// up fails, and that no call fails at all during the roll.
func TestS4ClusterOps(t *testing.T) {
	env := harness.New(t)
	cluster := env.Cluster(3, harness.GatewayOpts{Environment: "e2e"})
	n1, n2, n3 := cluster.Nodes[0], cluster.Nodes[1], cluster.Nodes[2]

	// orders registers through n1, billing through n2.
	orders := env.Fixture(n1, harness.FixtureOpts{Namespace: "orders", Owner: "commerce"})
	billing := env.Fixture(n2, harness.FixtureOpts{Kind: harness.KindOpenAPI, Namespace: "billing", Owner: "finance"})
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

		// /api/health answers 200 before a restarted node has replayed
		// the registry (see TestKnownBugHealthBeforeRegistryReady), so
		// the operator keeps the node out of rotation until it routes.
		lb.Pull(n2)
		n2.Proc.Restart()
		restartedAt := time.Now()
		n2.WaitReady()
		t.Logf("n2 healthy %s after restart (start to /api/health 200 and control plane)", time.Since(restartedAt).Round(time.Millisecond))
		cluster.WaitPeers(3, 60*time.Second)
		for _, f := range fixtures {
			f := f
			harness.Eventually(t, 60*time.Second, "n2 routes "+f.Opts.Namespace, func() error {
				_, err := f.EchoVia(ctx, n2, "v1", "probe")
				return err
			})
		}
		lb.Restore(n2)
		lb.WaitInRotation(t, n2, 10*time.Second)
		t.Logf("n2 rejoined with 3 peers and routing %s after restart", time.Since(restartedAt).Round(time.Millisecond))

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

		t.Run("no failures on nodes that stayed up", func(t *testing.T) {
			st := lb.Stats()
			for _, g := range survivors {
				if s := st[g.Name]; s.Failed != 0 || s.OK == 0 {
					errMu.Lock()
					t.Errorf("%s: sent=%d ok=%d failed=%d, want zero failed\nfirst errors: %v", g.Name, s.Sent, s.OK, s.Failed, nodeErrs[g.Name])
					errMu.Unlock()
				}
			}
			t.Logf("calls that failed on the killed node (in flight at SIGKILL or sent before the LB noticed): %d", st[n2.Name].Failed)
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
			node          string
			healthy, full time.Duration
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
			// The LB's own probe sees the node gone.
			lb.WaitOutOfRotation(t, g, 5*time.Second)

			ok0, _ := load.Counts()
			load.WaitOK(t, ok0+100, 30*time.Second) // traffic keeps flowing on the other two

			g.Proc.Restart()
			t0 := time.Now()
			g.WaitReady() // /api/health 200 and the control plane accepts
			healthy := time.Since(t0)
			// The restarted node has a new node id; its old peer entry
			// lingers until the 30s TTL, so match on names.
			cluster.WaitNodes(t, cluster.Names(), 60*time.Second)
			// Healthy is not ready: wait until the node routes both
			// services before the LB may send to it.
			for _, f := range fixtures {
				f := f
				harness.Eventually(t, 60*time.Second, g.Name+" routes "+f.Opts.Namespace, func() error {
					_, err := f.EchoVia(ctx, g, "v1", "probe")
					return err
				})
			}
			full := time.Since(t0)
			lb.Restore(g)
			lb.WaitInRotation(t, g, 10*time.Second)
			timings = append(timings, timing{g.Name, healthy, full})

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
			t.Logf("%s: start to /api/health 200: %s; start to routing both services: %s", tm.node, tm.healthy.Round(time.Millisecond), tm.full.Round(time.Millisecond))
			if tm.healthy > 20*time.Second {
				t.Errorf("%s took %s to become healthy, want under 20s", tm.node, tm.healthy)
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
