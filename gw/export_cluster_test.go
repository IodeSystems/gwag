package gateway

import "testing"

// SharedClusterPair starts one gateway on each node of the shared
// 2-node test cluster, with cluster tracking running and both seeing
// each other. For tests in package gateway_test (which may import
// packages that import this one, such as gw/oidc).
func SharedClusterPair(t *testing.T, aOpts, bOpts []Option) (a, b *Gateway) {
	t.Helper()
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	na := newSharedClusterNode(t, testCluster.a, aOpts...)
	nb := newSharedClusterNode(t, testCluster.b, bOpts...)
	waitForPeers(t, na.gw, nb.gw)
	return na.gw, nb.gw
}
