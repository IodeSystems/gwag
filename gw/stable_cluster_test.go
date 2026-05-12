package gateway

import (
	"context"
	"testing"
	"time"
)

// TestClusterE2E_StablePersistsAcrossNodes drives plan §4 Phase 2:
// a vN registered on node A persists in the dedicated stable KV
// bucket; node B's stableWatchLoop converges its local stableVN
// to the same value. The convergence holds even when B never sees
// the underlying replica (the proof that KV — not the reconciler-
// driven joinPoolLocked path — is now load-bearing for stable).
func TestClusterE2E_StablePersistsAcrossNodes(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	waitForPeers(t, a.gw, b.gw)

	// Register a v3 cut on A directly (in-process, no greeter backend
	// needed — we're testing stable propagation, not dispatch).
	a.gw.mu.Lock()
	a.gw.advanceStableLocked("svc", 3)
	a.gw.mu.Unlock()

	// B's stableWatchLoop should pull the value from KV.
	waitForStableConvergence(t, 15*time.Second, []*Gateway{b.gw}, func(snap map[string]int) bool {
		return snap["svc"] == 3
	})
}

// TestClusterE2E_StableMonotonicAcrossPeers covers the CAS-monotonic
// write loop: two peers writing different values to the same
// namespace key converge on the maximum, not last-writer-wins.
func TestClusterE2E_StableMonotonicAcrossPeers(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	waitForPeers(t, a.gw, b.gw)

	// A writes 4, B writes 7. Whichever lands first, the second's CAS
	// loop reads the current value, sees its own write doesn't beat it,
	// and either advances (if higher) or no-ops (if lower).
	a.gw.mu.Lock()
	a.gw.advanceStableLocked("svc", 4)
	a.gw.mu.Unlock()
	b.gw.mu.Lock()
	b.gw.advanceStableLocked("svc", 7)
	b.gw.mu.Unlock()

	// Wait for both nodes to converge on 7.
	waitForStableConvergence(t, 15*time.Second, []*Gateway{a.gw, b.gw}, func(snap map[string]int) bool {
		return snap["svc"] == 7
	})
}

// TestClusterE2E_StableInitialReplay: a node joining after a value
// has already been written reads the historical value via the
// watcher's initial replay (jetstream.IncludeHistory).
func TestClusterE2E_StableInitialReplay(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	waitForPeers(t, a.gw, b.gw)

	if a.gw.peers.stable == nil {
		t.Fatal("A.peers.stable nil")
	}

	// Write to A's stable KV directly (mirrors the writeback that
	// advanceStableLocked would have done from a real registration —
	// but bypasses the local in-memory map so we know B is reading
	// from KV, not via cluster gossip of any other kind).
	seedCtx, seedCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer seedCancel()
	var seedErr error
	for seedCtx.Err() == nil {
		wctx, cancel := context.WithTimeout(seedCtx, 2*time.Second)
		seedErr = tryWriteStable(wctx, a.gw.peers.stable, "svc", 9)
		cancel()
		if seedErr == nil {
			break
		}
		select {
		case <-seedCtx.Done():
			break
		case <-time.After(100 * time.Millisecond):
		}
	}
	if seedErr != nil {
		t.Fatalf("seed stable: %v", seedErr)
	}

	// B's watcher must have surfaced the value.
	waitForStableConvergence(t, 10*time.Second, []*Gateway{b.gw}, func(snap map[string]int) bool {
		return snap["svc"] == 9
	})
}
