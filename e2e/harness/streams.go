//go:build e2e

package harness

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// WaitStreamsReplicated waits until each of the gateway's KV buckets
// (KV_<bucket> streams) has a leader and want-1 current followers, and
// no replica-count change in progress. A node killed while the gateway
// still raises a bucket's replica count can leave the stream without
// a quorum: KV writes then fail on every node, and expired entries are
// not replaced. cluster_peers (NATS routes) does not tell. It asks
// through the first live node's NATS client port.
func (c *Cluster) WaitStreamsReplicated(t testing.TB, want int, timeout time.Duration, buckets ...string) {
	t.Helper()
	if len(buckets) == 0 {
		buckets = []string{"gwag-registry", "gwag-peers"}
	}
	var g *Gateway
	for _, n := range c.Live() {
		if n.NATSClientPort != 0 {
			g = n
			break
		}
	}
	if g == nil {
		t.Fatalf("harness: no live node with a NATS client port")
	}
	nc, err := nats.Connect(fmt.Sprintf("nats://127.0.0.1:%d", g.NATSClientPort))
	if err != nil {
		t.Fatalf("harness: connect to %s NATS: %v", g.Name, err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("harness: jetstream: %v", err)
	}
	Eventually(t, timeout, fmt.Sprintf("KV streams replicated %dx", want), func() error {
		for _, b := range buckets {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			s, err := js.Stream(ctx, "KV_"+b)
			cancel()
			if err != nil {
				return fmt.Errorf("%s: %w", b, err)
			}
			info := s.CachedInfo()
			cl := info.Cluster
			switch {
			case info.Config.Replicas != want:
				return fmt.Errorf("%s: replicas %d", b, info.Config.Replicas)
			case cl == nil || cl.Leader == "":
				return fmt.Errorf("%s: no leader", b)
			case cl.Desired != nil:
				return fmt.Errorf("%s: replica change in progress", b)
			case len(cl.Replicas) != want-1:
				return fmt.Errorf("%s: %d followers", b, len(cl.Replicas))
			}
			for _, p := range cl.Replicas {
				if !p.Current || p.Offline {
					return fmt.Errorf("%s: follower %s not current", b, p.Name)
				}
			}
		}
		return nil
	})
}
