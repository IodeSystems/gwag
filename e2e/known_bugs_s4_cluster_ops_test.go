//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

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
	// A kill while the peers stream still moves to 3 replicas can
	// leave it without a quorum until n2 is back: /api/admin/peers
	// then answers 500 (seen here and in TestS4ClusterOps).
	cluster.WaitStreamsReplicated(t, 3, 75*time.Second)

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
