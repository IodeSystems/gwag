package gateway

import (
	"testing"
	"time"
)

func startRouteNode(t *testing.T, name, clusterAddr string, seeds []string) (*Cluster, *Gateway) {
	t.Helper()
	cl, err := StartCluster(ClusterOptions{
		NodeName:      name,
		ClientListen:  freeAddr(t),
		ClusterListen: clusterAddr,
		Peers:         seeds,
		DataDir:       t.TempDir(),
		StartTimeout:  10 * time.Second,
		LogLevel:      "silent",
	})
	if err != nil {
		t.Fatalf("StartCluster %s: %v", name, err)
	}
	t.Cleanup(cl.Close)
	g := New(WithCluster(cl), WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	return cl, g
}

func waitPeerCount(t *testing.T, g *Gateway, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := g.livePeerCount()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("livePeerCount = %d, want %d", got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// cluster_peers must follow NATS route state: a dead peer drops well
// inside peerTTL, and a restarted node (new server ID) replaces its
// predecessor instead of adding to it.
func TestClusterPeersMetric_FollowsRoutes(t *testing.T) {
	addrA, addrB, addrC := freeAddr(t), freeAddr(t), freeAddr(t)
	_, ga := startRouteNode(t, "peers-a", addrA, []string{addrB})
	clB, _ := startRouteNode(t, "peers-b", addrB, []string{addrA})
	_, _ = startRouteNode(t, "peers-c", addrC, []string{addrA})
	waitPeerCount(t, ga, 3, 10*time.Second)

	clB.Close()
	waitPeerCount(t, ga, 2, peerTTL/2)

	_, _ = startRouteNode(t, "peers-b2", freeAddr(t), []string{addrA})
	waitPeerCount(t, ga, 3, 10*time.Second)
}

func TestClusterPeersMetric_NotClusteredIsZero(t *testing.T) {
	g := New(WithoutBackpressure())
	t.Cleanup(g.Close)
	if n := g.livePeerCount(); n != 0 {
		t.Fatalf("livePeerCount = %d, want 0", n)
	}
}
