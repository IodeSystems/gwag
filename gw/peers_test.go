package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// TestStartClusterTracking_FailedStartDoesNotBlockClose makes the
// initial self Put fail after the tracker is published on g.peers.
// Its loops never start, so nothing closes t.done; Close must not wait
// on it. A t.Fatalf on this error used to hang the test in cleanup.
func TestStartClusterTracking_FailedStartDoesNotBlockClose(t *testing.T) {
	cluster, err := StartCluster(ClusterOptions{
		NodeName:     "tracking-fail-test",
		ClientListen: freeAddr(t),
		DataDir:      t.TempDir(),
		StartTimeout: 10 * time.Second,
		LogLevel:     "silent",
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	defer cluster.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A 1-byte value cap. startClusterTracking treats the existing bucket
	// as ErrBucketExists and opens it, then the self Put exceeds the cap.
	if _, err := cluster.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       peersBucketName,
		MaxValueSize: 1,
	}); err != nil {
		t.Fatalf("pre-create peers bucket: %v", err)
	}

	gw := New(WithCluster(cluster), WithoutMetrics(), WithoutBackpressure())
	if _, err := gw.startClusterTracking(context.Background()); err == nil {
		t.Fatal("startClusterTracking: want put self error, got nil")
	}

	closed := make(chan struct{})
	go func() {
		gw.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked after a failed startClusterTracking")
	}
}
