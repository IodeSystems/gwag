package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// TestExpiringBucket_UpgradesOldBucket: a registry bucket made by a
// gateway before delete markers (TTL, no markers) gets markers turned
// on in place, with its other limits kept; an entry that expires then
// reaches a watcher as a Purge. A pre-marker gateway's replica update,
// which leaves LimitMarkerTTL out, is refused by the server instead of
// turning markers off again.
func TestExpiringBucket_UpgradesOldBucket(t *testing.T) {
	const ttl = 2 * time.Second
	cluster, err := StartCluster(ClusterOptions{
		NodeName:     "markers-upgrade",
		ClientListen: freeAddr(t),
		DataDir:      t.TempDir(),
		StartTimeout: 10 * time.Second,
		LogLevel:     "silent",
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cluster.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cluster.WaitForJetStream(ctx); err != nil {
		t.Fatal(err)
	}
	// As a pre-marker gateway created it, plus a limit to check it is
	// kept.
	if _, err := cluster.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       registryBucketName,
		Replicas:     1,
		TTL:          ttl,
		MaxValueSize: 1 << 20,
	}); err != nil {
		t.Fatalf("pre-create registry bucket: %v", err)
	}

	g := New(WithCluster(cluster), WithoutMetrics(), WithoutBackpressure(), withClusterKVTTL(ttl))
	t.Cleanup(g.Close)
	// Background: the ctx parents the watch + reconciler loops.
	if _, err := g.startClusterTracking(context.Background()); err != nil {
		t.Fatalf("startClusterTracking: %v", err)
	}

	stream, err := cluster.JS.Stream(ctx, "KV_"+registryBucketName)
	if err != nil {
		t.Fatal(err)
	}
	cfg := stream.CachedInfo().Config
	if cfg.SubjectDeleteMarkerTTL != ttl || !cfg.AllowMsgTTL {
		t.Fatalf("markers not on: SubjectDeleteMarkerTTL=%s AllowMsgTTL=%v", cfg.SubjectDeleteMarkerTTL, cfg.AllowMsgTTL)
	}
	if cfg.MaxMsgSize != 1<<20 || cfg.MaxAge != ttl {
		t.Fatalf("upgrade changed limits: MaxMsgSize=%d MaxAge=%s", cfg.MaxMsgSize, cfg.MaxAge)
	}

	kv := g.registryKV()
	w, err := kv.Watch(ctx, "probe")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if _, err := kv.Put(ctx, "probe", []byte("x")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(ttl + 10*time.Second)
	for expired := false; !expired; {
		select {
		case ev := <-w.Updates():
			if ev != nil && ev.Operation() == jetstream.KeyValuePurge {
				expired = true
			}
		case <-deadline:
			t.Fatal("no Purge event after the entry's TTL")
		}
	}

	// The sweep's leader lookup: expired (marker), missing, deleted
	// are gone; a live key is not.
	gone := func(key string) bool {
		t.Helper()
		g, err := kvKeyGone(ctx, cluster.Conn, registryBucketName, key)
		if err != nil {
			t.Fatalf("kvKeyGone %s: %v", key, err)
		}
		return g
	}
	if !gone("probe") || !gone("never-written") {
		t.Fatal("kvKeyGone: an expired or missing key is not gone")
	}
	if _, err := kv.Put(ctx, "live", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if gone("live") {
		t.Fatal("kvKeyGone: a live key is gone")
	}
	if err := kv.Delete(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	if !gone("live") {
		t.Fatal("kvKeyGone: a deleted key is not gone")
	}

	// What a pre-marker gateway's setReplicas sends.
	if _, err := cluster.JS.UpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   registryBucketName,
		Replicas: 1,
		TTL:      ttl,
	}); err == nil {
		t.Fatal("update without LimitMarkerTTL accepted; markers would be off")
	}
}
