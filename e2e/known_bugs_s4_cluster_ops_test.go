//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// TestKnownBugHealthBeforeRegistryReady: a restarted cluster node
// answers /api/health 200 ("serving") before it has replayed the
// registry, so a load balancer that trusts the health check sends it
// traffic that fails with `Cannot query field "orders" on type
// "Query"`. /api/health is the only readiness signal the gateway
// exposes. Timing dependent: the window is the registry replay, so the
// test restarts the node several times and polls tightly.
func TestKnownBugHealthBeforeRegistryReady(t *testing.T) {
	knownBug(t, "/api/health is 200 before a restarted cluster node routes registered services")
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

// TestClusterPeersMetricTracksLiveNodes: cluster_peers counts this node
// plus the remote nodes it holds a NATS route to. A killed peer drops
// out within seconds (not after the peers KV entry's TTL), and a
// restarted node (new node id) replaces its predecessor.
func TestClusterPeersMetricTracksLiveNodes(t *testing.T) {
	env := harness.New(t)
	cluster := env.Cluster(3, harness.GatewayOpts{})
	n1, n2 := cluster.Nodes[0], cluster.Nodes[1]
	ctx := harness.Ctx(t, 5*time.Minute)

	peersMetric := func(g *harness.Gateway) (float64, error) {
		s, err := g.Scrape(ctx)
		if err != nil {
			return 0, err
		}
		return s.Counter("cluster_peers", nil), nil
	}
	want := func(desc string, g *harness.Gateway, n float64) {
		t.Helper()
		harness.Eventually(t, 45*time.Second, desc, func() error {
			v, err := peersMetric(g)
			if err != nil {
				return err
			}
			if v != n {
				return fmt.Errorf("cluster_peers = %v, want %v", v, n)
			}
			return nil
		})
	}
	want("n1 counts 3 peers", n1, 3)

	n2.Proc.Kill()
	// The gauge follows NATS routes, so it drops long before the peers
	// KV entry expires (peerTTL) and /api/admin/peers drops n2.
	want("n1 counts 2 peers after n2 died", n1, 2)
	harness.Eventually(t, 75*time.Second, "n1's /api/admin/peers drops n2", func() error {
		n, err := n1.PeerCount(ctx)
		if err != nil {
			return err
		}
		if n != 2 {
			return fmt.Errorf("%d peers", n)
		}
		return nil
	})

	n2.Proc.Restart()
	n2.WaitReady()
	cluster.WaitPeers(3, 75*time.Second)
	want("n1 counts 3 peers after n2 returned", n1, 3)
}
