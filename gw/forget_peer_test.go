package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

type forgetPeerFixture struct {
	gw      *Gateway
	cluster *Cluster
	cp      cpv1.ControlPlaneServer
}

// newForgetPeerFixture spins a single-node cluster and brings the
// peer tracker up synchronously (vs the async ControlPlane() path).
// Tests can manipulate the peers KV directly via cluster.JS.
func newForgetPeerFixture(t *testing.T) *forgetPeerFixture {
	t.Helper()
	dir := t.TempDir()
	cluster, err := StartCluster(ClusterOptions{
		NodeName:      "test",
		ClientListen:  "127.0.0.1:0",
		ClusterListen: "127.0.0.1:0",
		DataDir:       dir,
		StartTimeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cluster.Close)

	gw := New(WithCluster(cluster), WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)

	jsCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := cluster.WaitForJetStream(jsCtx); err != nil {
		cancel()
		t.Fatalf("WaitForJetStream: %v", err)
	}
	cancel()
	// startClusterTracking captures its ctx as the parent of the long-
	// running watch + reconciler goroutines. Use Background so they
	// don't die when this helper returns. Cleanup runs through
	// gw.Close → tracker.stop.
	if _, err := gw.startClusterTracking(context.Background()); err != nil {
		t.Fatalf("startClusterTracking: %v", err)
	}

	return &forgetPeerFixture{
		gw:      gw,
		cluster: cluster,
		cp:      gw.ControlPlane(),
	}
}

// putPeer writes a peerEntry under nodeID into the peers KV.
func (f *forgetPeerFixture) putPeer(t *testing.T, nodeID, name string) {
	t.Helper()
	b, err := json.Marshal(peerEntry{
		NodeID:  nodeID,
		Name:    name,
		JoinedM: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("marshal peer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.gw.peers.peers.Put(ctx, nodeID, b); err != nil {
		t.Fatalf("put peer: %v", err)
	}
	// Mirror the local live-set update the watch loop would do.
	f.gw.peers.mu.Lock()
	f.gw.peers.live[nodeID] = struct{}{}
	f.gw.peers.mu.Unlock()
}

func TestForgetPeer_AlivePeerRejected(t *testing.T) {
	f := newForgetPeerFixture(t)
	f.putPeer(t, "PEER_ALIVE_NODE_ID", "alive")

	resp, err := f.cp.ForgetPeer(context.Background(), &cpv1.ForgetPeerRequest{
		NodeId: "PEER_ALIVE_NODE_ID",
	})
	if err == nil {
		t.Fatalf("expected error, got resp=%+v", resp)
	}
	if !strings.Contains(err.Error(), "still alive") {
		t.Fatalf("error should mention 'still alive', got: %v", err)
	}
}

func TestForgetPeer_HappyPath(t *testing.T) {
	f := newForgetPeerFixture(t)
	// The peer's KV entry has expired but this node's watch has not
	// dropped it from the live set yet. Writing and deleting the KV
	// entry here raced the watch loop (a late Put event re-added the
	// peer after ForgetPeer), so the state is set directly: absent
	// from KV, present in the live set.
	f.gw.peers.mu.Lock()
	f.gw.peers.live["PEER_DEAD_NODE_ID"] = struct{}{}
	f.gw.peers.mu.Unlock()

	resp, err := f.cp.ForgetPeer(context.Background(), &cpv1.ForgetPeerRequest{
		NodeId: "PEER_DEAD_NODE_ID",
	})
	if err != nil {
		t.Fatalf("ForgetPeer: %v", err)
	}
	if !resp.GetRemoved() {
		t.Fatalf("Removed=false; expected true since peer was in local live set")
	}

	// Live set should no longer contain the forgotten peer.
	f.gw.peers.mu.Lock()
	_, stillLive := f.gw.peers.live["PEER_DEAD_NODE_ID"]
	f.gw.peers.mu.Unlock()
	if stillLive {
		t.Fatal("forgotten peer still in live set")
	}
}

func TestForgetPeer_NotInLiveSet(t *testing.T) {
	// Peer never registered (never in KV, never in live set). ForgetPeer
	// returns successfully but Removed=false — the call is a no-op.
	f := newForgetPeerFixture(t)
	resp, err := f.cp.ForgetPeer(context.Background(), &cpv1.ForgetPeerRequest{
		NodeId: "NEVER_EXISTED_NODE_ID",
	})
	if err != nil {
		t.Fatalf("ForgetPeer: %v", err)
	}
	if resp.GetRemoved() {
		t.Fatal("Removed=true for a peer that never registered")
	}
}

func TestForgetPeer_RefuseSelf(t *testing.T) {
	f := newForgetPeerFixture(t)
	resp, err := f.cp.ForgetPeer(context.Background(), &cpv1.ForgetPeerRequest{
		NodeId: f.cluster.NodeID,
	})
	if err == nil {
		t.Fatalf("expected error forgetting self, got resp=%+v", resp)
	}
	if !strings.Contains(err.Error(), "self") {
		t.Fatalf("error should mention 'self', got: %v", err)
	}
}

func TestForgetPeer_EmptyNodeID(t *testing.T) {
	f := newForgetPeerFixture(t)
	if _, err := f.cp.ForgetPeer(context.Background(), &cpv1.ForgetPeerRequest{NodeId: ""}); err == nil {
		t.Fatal("expected error for empty node_id")
	}
}

func TestForgetPeer_NoClusterConfigured(t *testing.T) {
	// Standalone gateway — no cluster, no peers KV. ForgetPeer should
	// error with "cluster not configured".
	gw := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)
	cp := gw.ControlPlane()
	_, err := cp.ForgetPeer(context.Background(), &cpv1.ForgetPeerRequest{NodeId: "X"})
	if err == nil {
		t.Fatal("expected error in standalone mode")
	}
	if !strings.Contains(err.Error(), "cluster not configured") {
		t.Fatalf("error: %v (want 'cluster not configured')", err)
	}
}
