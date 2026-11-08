package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// newLevelCluster starts a single-node JetStream with no gateway yet,
// so a test can seed the cluster state a gateway then joins.
func newLevelCluster(t *testing.T) *Cluster {
	t.Helper()
	cl, err := StartCluster(ClusterOptions{
		NodeName:      "lvl",
		ClientListen:  "127.0.0.1:0",
		ClusterListen: "127.0.0.1:0",
		DataDir:       t.TempDir(),
		StartTimeout:  10 * time.Second,
		LogLevel:      "silent",
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cl.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cl.WaitForJetStream(ctx); err != nil {
		t.Fatalf("WaitForJetStream: %v", err)
	}
	return cl
}

func levelGateway(t *testing.T, cl *Cluster, opts ...Option) *Gateway {
	t.Helper()
	g := New(append([]Option{WithCluster(cl), WithoutMetrics(), WithoutBackpressure()}, opts...)...)
	t.Cleanup(g.Close)
	return g
}

func healthOf(t *testing.T, g *Gateway) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	g.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func streamMarkerTTL(t *testing.T, js jetstream.JetStream, bucket string) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, err := readStreamConfig(ctx, js, "KV_"+bucket)
	if err != nil || cfg == nil {
		t.Fatalf("read %s: %v", bucket, err)
	}
	return cfg.SubjectDeleteMarkerTTL
}

// seedLegacyPeer writes a peers entry the way a gateway older than
// feature levels does: no version, no levels, bucket without markers.
func seedLegacyPeer(t *testing.T, cl *Cluster, nodeID, name string) jetstream.KeyValue {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kv, err := cl.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: peersBucketName, Replicas: 1, TTL: peerTTL})
	if err != nil {
		t.Fatalf("create peers bucket: %v", err)
	}
	if _, err := cl.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: registryBucketName, Replicas: 1, TTL: registryTTL}); err != nil {
		t.Fatalf("create registry bucket: %v", err)
	}
	b := []byte(`{"node_id":"` + nodeID + `","name":"` + name + `","joined_unix_ms":1}`)
	if _, err := kv.Put(ctx, nodeID, b); err != nil {
		t.Fatalf("put legacy peer: %v", err)
	}
	return kv
}

func TestCheckClusterLevel(t *testing.T) {
	for l := clusterLevelMin; l <= clusterLevelMax(); l++ {
		if err := checkClusterLevel(l); err != nil {
			t.Errorf("level %d: %v", l, err)
		}
	}
	err := checkClusterLevel(clusterLevelMax() + 1)
	if err == nil || !strings.Contains(err.Error(), "above the highest level") {
		t.Fatalf("max+1: %v", err)
	}
	err = checkClusterLevel(clusterLevelMin - 1)
	if err == nil || !strings.Contains(err.Error(), "below the lowest level") {
		t.Fatalf("min-1: %v", err)
	}
}

func TestClusterFeatures_Ordered(t *testing.T) {
	prev := clusterLevelMin
	for _, f := range clusterFeatures {
		if f.Level != prev+1 {
			t.Fatalf("feature %s has level %d, want %d (levels go up by one)", f.Name, f.Level, prev+1)
		}
		prev = f.Level
	}
}

// A new cluster starts at the highest level: markers on, level record
// written, levels in the peers entry.
func TestClusterLevel_NewClusterStartsAtMax(t *testing.T) {
	cl := newLevelCluster(t)
	g := levelGateway(t, cl)
	if _, err := g.startClusterTracking(context.Background()); err != nil {
		t.Fatalf("startClusterTracking: %v", err)
	}
	if got := g.ClusterLevel(); got != clusterLevelMax() {
		t.Fatalf("level %d, want %d", got, clusterLevelMax())
	}
	if d := streamMarkerTTL(t, cl.JS, registryBucketName); d <= 0 {
		t.Fatalf("registry markers off at level %d", g.ClusterLevel())
	}
	resp, err := g.ControlPlane().ListPeers(context.Background(), &cpv1.ListPeersRequest{})
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if resp.GetActiveLevel() != uint32(clusterLevelMax()) || len(resp.GetPeers()) != 1 {
		t.Fatalf("ListPeers: %+v", resp)
	}
	p := resp.GetPeers()[0]
	if p.GetMaxLevel() != uint32(clusterLevelMax()) || p.GetMinLevel() != clusterLevelMin || p.GetVersion() == "" {
		t.Fatalf("peer entry: %+v", p)
	}
}

// A node of this version joining older nodes runs at their level and
// writes the old format; finalize is refused while they are live and
// turns the new format on once they are gone.
func TestClusterLevel_JoinLegacyThenFinalize(t *testing.T) {
	cl := newLevelCluster(t)
	peers := seedLegacyPeer(t, cl, "LEGACYNODE", "old1")
	g := levelGateway(t, cl)
	if _, err := g.startClusterTracking(context.Background()); err != nil {
		t.Fatalf("startClusterTracking: %v", err)
	}
	if got := g.ClusterLevel(); got != 0 {
		t.Fatalf("level %d, want 0 with a legacy peer", got)
	}
	for _, b := range []string{peersBucketName, registryBucketName} {
		if d := streamMarkerTTL(t, cl.JS, b); d != 0 {
			t.Fatalf("%s: markers on (%s) at level 0", b, d)
		}
	}
	if code, body := healthOf(t, g); code == http.StatusServiceUnavailable && body["status"] == "incompatible" {
		t.Fatalf("health at level 0: %v", body)
	}

	cp := g.ControlPlane()
	_, err := cp.FinalizeCluster(context.Background(), &cpv1.FinalizeClusterRequest{})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "old1 (no level support) max 0") {
		t.Fatalf("finalize with legacy peer: %v", err)
	}
	dry, err := cp.FinalizeCluster(context.Background(), &cpv1.FinalizeClusterRequest{DryRun: true, Level: 0})
	if err != nil || len(dry.GetBlockers()) != 1 || dry.GetActiveLevel() != 0 {
		t.Fatalf("dry run with legacy peer: resp=%v err=%v", dry, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := peers.Delete(ctx, "LEGACYNODE"); err != nil {
		t.Fatalf("delete legacy peer: %v", err)
	}
	dry, err = cp.FinalizeCluster(context.Background(), &cpv1.FinalizeClusterRequest{DryRun: true})
	if err != nil || dry.GetActiveLevel() != uint32(clusterLevelMax()) {
		t.Fatalf("dry run: %v %v", dry, err)
	}
	if g.ClusterLevel() != 0 {
		t.Fatal("dry run changed the level")
	}
	resp, err := cp.FinalizeCluster(context.Background(), &cpv1.FinalizeClusterRequest{})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if resp.GetPreviousLevel() != 0 || resp.GetActiveLevel() != uint32(clusterLevelMax()) {
		t.Fatalf("finalize: %+v", resp)
	}
	for _, b := range []string{peersBucketName, registryBucketName} {
		if d := streamMarkerTTL(t, cl.JS, b); d <= 0 {
			t.Fatalf("%s: markers off after finalize", b)
		}
	}
	// Idempotent; never lowers.
	if resp, err := cp.FinalizeCluster(context.Background(), &cpv1.FinalizeClusterRequest{}); err != nil || resp.GetActiveLevel() != resp.GetPreviousLevel() {
		t.Fatalf("second finalize: %v %v", resp, err)
	}
	if _, err := cp.FinalizeCluster(context.Background(), &cpv1.FinalizeClusterRequest{Level: 0}); err != nil {
		t.Fatalf("finalize level 0 again: %v", err)
	}
}

func TestClusterLevel_FinalizeAboveMaxBlockedBySelf(t *testing.T) {
	cl := newLevelCluster(t)
	g := levelGateway(t, cl)
	if _, err := g.startClusterTracking(context.Background()); err != nil {
		t.Fatalf("startClusterTracking: %v", err)
	}
	_, err := g.ControlPlane().FinalizeCluster(context.Background(), &cpv1.FinalizeClusterRequest{Level: uint32(clusterLevelMax() + 1)})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "lvl ") {
		t.Fatalf("finalize above max: %v", err)
	}
}

// A cluster finalized past this binary is refused at start, with the
// reason in health.
func TestClusterLevel_RefusesLevelAboveMax(t *testing.T) {
	cl := newLevelCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kv, err := openClusterBucket(ctx, cl.JS)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(clusterLevelRecord{Level: clusterLevelMax() + 1})
	if _, err := kv.Put(ctx, clusterLevelKey, b); err != nil {
		t.Fatal(err)
	}
	g := levelGateway(t, cl)
	_, err = g.startClusterTracking(context.Background())
	if err == nil || !strings.Contains(err.Error(), "above the highest level") {
		t.Fatalf("startClusterTracking: %v", err)
	}
	code, body := healthOf(t, g)
	if code != http.StatusServiceUnavailable || body["status"] != "incompatible" || !strings.Contains(body["error"].(string), "above the highest level") {
		t.Fatalf("health: %d %v", code, body)
	}
	if g.peers != nil {
		t.Fatal("tracker started on an incompatible level")
	}
}

// A raise past this binary while it runs turns health incompatible.
func TestClusterLevel_WatchRaiseAboveMax(t *testing.T) {
	cl := newLevelCluster(t)
	g := levelGateway(t, cl)
	if _, err := g.startClusterTracking(context.Background()); err != nil {
		t.Fatalf("startClusterTracking: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, _ := json.Marshal(clusterLevelRecord{Level: clusterLevelMax() + 1})
	if _, err := g.peers.cluster.Put(ctx, clusterLevelKey, b); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := healthOf(t, g)
		if code == http.StatusServiceUnavailable && body["status"] == "incompatible" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("health after raise: %d %v", code, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Auto-finalize raises once the legacy peer is gone.
func TestClusterLevel_AutoFinalize(t *testing.T) {
	cl := newLevelCluster(t)
	peers := seedLegacyPeer(t, cl, "LEGACYNODE", "old1")
	g := levelGateway(t, cl, WithClusterAutoFinalize())
	if _, err := g.startClusterTracking(context.Background()); err != nil {
		t.Fatalf("startClusterTracking: %v", err)
	}
	g.peers.autoFinalize(context.Background())
	if g.ClusterLevel() != 0 {
		t.Fatal("auto-finalize raised with a legacy peer live")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := peers.Delete(ctx, "LEGACYNODE"); err != nil {
		t.Fatal(err)
	}
	g.peers.autoFinalize(context.Background())
	if g.ClusterLevel() != clusterLevelMax() {
		t.Fatalf("level %d after auto-finalize", g.ClusterLevel())
	}
}
