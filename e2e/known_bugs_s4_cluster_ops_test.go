//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// TestKnownBugClusterPeersMetricNeverDrops: cluster_peers is documented
// as "Live gateway peers including this node". The gauge counts the
// peer tracker's watch set, which only grows: a peer that dies is
// removed from /api/admin/peers when its KV entry expires, but the
// expiry raises no delete event, so the gauge keeps counting it, and a
// restarted node (new node id) is counted twice.
func TestKnownBugClusterPeersMetricNeverDrops(t *testing.T) {
	knownBug(t, "cluster_peers does not drop when a peer dies and double counts a restarted peer")
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
	want("n1 counts 2 peers after n2 died", n1, 2)

	n2.Proc.Restart()
	n2.WaitReady()
	cluster.WaitPeers(3, 75*time.Second)
	want("n1 counts 3 peers after n2 returned", n1, 3)
}
