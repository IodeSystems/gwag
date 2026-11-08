package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// newFailingTrackingCluster starts a single-node cluster whose peers
// bucket has a 1-byte value cap. startClusterTracking treats the
// existing bucket as ErrBucketExists and opens it, then the self Put
// exceeds the cap, so every start fails after publishing its tracker.
func newFailingTrackingCluster(t *testing.T) *Cluster {
	t.Helper()
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
	t.Cleanup(cluster.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cluster.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       peersBucketName,
		MaxValueSize: 1,
	}); err != nil {
		t.Fatalf("pre-create peers bucket: %v", err)
	}
	return cluster
}

// closeWithin fails the test if gw.Close does not return within d.
func closeWithin(t *testing.T, gw *Gateway, d time.Duration, msg string) {
	t.Helper()
	closed := make(chan struct{})
	go func() {
		gw.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(d):
		t.Fatal(msg)
	}
}

// TestStartClusterTracking_FailedStartDoesNotBlockClose makes the
// initial self Put fail after the tracker is published on g.peers.
// Its loops never start, so nothing closes t.done; Close must not wait
// on it. A t.Fatalf on this error used to hang the test in cleanup.
func TestStartClusterTracking_FailedStartDoesNotBlockClose(t *testing.T) {
	cluster := newFailingTrackingCluster(t)

	gw := New(WithCluster(cluster), WithoutMetrics(), WithoutBackpressure())
	if _, err := gw.startClusterTracking(context.Background()); err == nil {
		t.Fatal("startClusterTracking: want put self error, got nil")
	}
	closeWithin(t, gw, 5*time.Second, "Close blocked after a failed startClusterTracking")
}

// TestStartClusterTracking_CloseDuringFailingStart calls Close while a
// start is still in flight. If Close takes the published tracker before
// the start fails, the start finds g.peers already cleared and returns,
// and Close waits on loops that never start. ControlPlane's
// bootClusterTracking makes this reachable: it runs a start in the
// background while test cleanup closes the gateway.
//
// No hook pauses the start between publish and failure, so the Close
// is swept across one measured start's duration instead.
func TestStartClusterTracking_CloseDuringFailingStart(t *testing.T) {
	cluster := newFailingTrackingCluster(t)

	probe := New(WithCluster(cluster), WithoutMetrics(), WithoutBackpressure())
	began := time.Now()
	if _, err := probe.startClusterTracking(context.Background()); err == nil {
		t.Fatal("startClusterTracking: want put self error, got nil")
	}
	span := time.Since(began)
	closeWithin(t, probe, 5*time.Second, "Close blocked after a failed startClusterTracking")

	const iterations = 60
	for i := 0; i < iterations; i++ {
		gw := New(WithCluster(cluster), WithoutMetrics(), WithoutBackpressure())
		started := make(chan struct{})
		finished := make(chan struct{})
		go func() {
			close(started)
			_, _ = gw.startClusterTracking(context.Background())
			close(finished)
		}()
		<-started
		time.Sleep(span * time.Duration(i) / iterations)
		closeWithin(t, gw, 5*time.Second, "Close blocked on a start that failed while Close ran")
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("startClusterTracking did not return after Close")
		}
	}
}

// TestStartClusterTracking_DoesNotHoldGatewayLock: a node whose
// JetStream has no meta leader (its only route peer is down) answers
// no bucket call. startClusterTracking waits on those calls; g.mu must
// stay free meanwhile, or every registration, and with it a booting
// gateway's HTTP listener, waits too. Before the fix it held g.mu
// across the calls (5s each, without end in a key listing).
func TestStartClusterTracking_DoesNotHoldGatewayLock(t *testing.T) {
	cl, err := StartCluster(ClusterOptions{
		NodeName:      "no-meta",
		ClientListen:  freeAddr(t),
		ClusterListen: freeAddr(t),
		Peers:         []string{freeAddr(t)}, // nothing listens there
		DataDir:       t.TempDir(),
		StartTimeout:  10 * time.Second,
		LogLevel:      "silent",
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cl.Close)
	g := New(WithCluster(cl), WithoutMetrics(), WithoutBackpressure())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := g.startClusterTracking(ctx)
		done <- err
	}()
	time.Sleep(500 * time.Millisecond) // the first bucket call is in flight
	select {
	case err := <-done:
		t.Fatalf("startClusterTracking returned without a meta leader: %v", err)
	default:
	}
	locked := make(chan struct{})
	go func() {
		g.mu.Lock()
		g.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("g.mu held while startClusterTracking waits on JetStream")
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("startClusterTracking succeeded without a meta leader")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("startClusterTracking did not return after its context was cancelled")
	}
	g.Close()
}
