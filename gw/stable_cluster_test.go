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
	a, b := startTwoNodeCluster(t)

	// Register a v3 cut on A directly (in-process, no greeter backend
	// needed — we're testing stable propagation, not dispatch).
	a.gw.mu.Lock()
	a.gw.advanceStableLocked("svc", 3)
	a.gw.mu.Unlock()

	// B's stableWatchLoop should pull the value from KV. JetStream
	// stream-leader election in a fresh two-node cluster can take
	// a few seconds, so the convergence budget is generous.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		b.gw.mu.Lock()
		got := b.gw.stableVN["svc"]
		b.gw.mu.Unlock()
		if got == 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.gw.mu.Lock()
	got := b.gw.stableVN["svc"]
	b.gw.mu.Unlock()
	if got != 3 {
		t.Fatalf("B's stableVN[svc] = %d, want 3 (KV-driven convergence)", got)
	}
}

// TestClusterE2E_StableMonotonicAcrossPeers covers the CAS-monotonic
// write loop: two peers writing different values to the same
// namespace key converge on the maximum, not last-writer-wins.
func TestClusterE2E_StableMonotonicAcrossPeers(t *testing.T) {
	a, b := startTwoNodeCluster(t)

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
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		a.gw.mu.Lock()
		ga := a.gw.stableVN["svc"]
		a.gw.mu.Unlock()
		b.gw.mu.Lock()
		gb := b.gw.stableVN["svc"]
		b.gw.mu.Unlock()
		if ga == 7 && gb == 7 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.gw.mu.Lock()
	ga := a.gw.stableVN["svc"]
	a.gw.mu.Unlock()
	b.gw.mu.Lock()
	gb := b.gw.stableVN["svc"]
	b.gw.mu.Unlock()
	t.Fatalf("CAS-monotonic convergence timeout: A=%d B=%d, want both 7", ga, gb)
}

// TestClusterE2E_StableInitialReplay: a node joining after a value
// has already been written reads the historical value via the
// watcher's initial replay (jetstream.IncludeHistory).
func TestClusterE2E_StableInitialReplay(t *testing.T) {
	a, b := startTwoNodeCluster(t)

	// Write to A's stable KV directly (mirrors the writeback that
	// advanceStableLocked would have done from a real registration —
	// but bypasses the local in-memory map so we know B is reading
	// from KV, not via cluster gossip of any other kind).
	if a.gw.peers.stable == nil {
		t.Fatal("A.peers.stable nil")
	}
	// JetStream stream-leader election in a fresh two-node cluster
	// can take a couple of seconds; retry the seed until it lands.
	seedDeadline := time.Now().Add(15 * time.Second)
	var seedErr error
	for time.Now().Before(seedDeadline) {
		wctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		seedErr = tryWriteStable(wctx, a.gw.peers.stable, "svc", 9)
		cancel()
		if seedErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if seedErr != nil {
		t.Fatalf("seed stable: %v", seedErr)
	}

	// B's watcher must have surfaced the value.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.gw.mu.Lock()
		got := b.gw.stableVN["svc"]
		b.gw.mu.Unlock()
		if got == 9 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.gw.mu.Lock()
	got := b.gw.stableVN["svc"]
	b.gw.mu.Unlock()
	t.Fatalf("B's stableVN[svc] = %d, want 9 (initial KV replay)", got)
}
