package gateway

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// BenchmarkPubSubRoundTrip measures ps.pub through the dispatcher to
// one ps.sub consumer on the same node (cluster mode: NATS echo).
// One op = one publish plus the receive of the decoded event.
func BenchmarkPubSubRoundTrip(b *testing.B) {
	// A real free port: ClientListen port 0 makes natsd fall back to
	// its default 4222.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	clientAddr := lis.Addr().String()
	_ = lis.Close()
	cluster, err := StartCluster(ClusterOptions{
		NodeName:      "pubsub-bench",
		ClientListen:  clientAddr,
		ClusterListen: "127.0.0.1:0",
		DataDir:       b.TempDir(),
		StartTimeout:  10 * time.Second,
		LogLevel:      "silent",
	})
	if err != nil {
		b.Fatalf("StartCluster: %v", err)
	}
	defer cluster.Close()
	g := New(
		WithCluster(cluster),
		WithoutMetrics(),
		WithoutBackpressure(),
		WithChannelAuth("events.>", ChannelAuthOpen),
	)
	defer g.Close()
	g.mu.Lock()
	if err := g.assembleLocked(); err != nil {
		g.mu.Unlock()
		b.Fatalf("assembleLocked: %v", err)
	}
	g.mu.Unlock()
	pub := g.dispatchers.Get(ir.MakeSchemaID(psNamespace, psVersion, "Pub"))
	sub := g.dispatchers.Get(ir.MakeSchemaID(psNamespace, psVersion, "Sub"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src, err := sub.Dispatch(ctx, map[string]any{"channel": "events.bench"})
	if err != nil {
		b.Fatalf("Sub: %v", err)
	}
	events := src.(chan any)
	_ = cluster.Conn.Flush()
	args := map[string]any{"channel": "events.bench", "payload": "hello"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := pub.Dispatch(context.Background(), args); err != nil {
			b.Fatalf("Pub: %v", err)
		}
		select {
		case <-events:
		case <-time.After(2 * time.Second):
			b.Fatal("timeout waiting for event")
		}
	}
}
