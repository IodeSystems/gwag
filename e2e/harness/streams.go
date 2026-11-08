//go:build e2e

package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// NATSURL is the node's NATS client URL; "" without NATS.
func (g *Gateway) NATSURL() string {
	if g.NATSClientPort == 0 {
		return ""
	}
	return fmt.Sprintf("nats://127.0.0.1:%d", g.NATSClientPort)
}

// NATSURLs is every node's NATS client URL, in node order. Join them
// with "," for nats.Connect, so a client moves to another node when
// its node dies.
func (c *Cluster) NATSURLs() []string {
	var out []string
	for _, g := range c.Nodes {
		if u := g.NATSURL(); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// NATS connects to the cluster's NATS client ports, first URL first
// (no randomizing), reconnecting without limit. The connection closes
// at test end.
func (c *Cluster) NATS(t testing.TB, opts ...nats.Option) *nats.Conn {
	t.Helper()
	opts = append([]nats.Option{
		nats.DontRandomize(),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(100 * time.Millisecond),
	}, opts...)
	nc, err := nats.Connect(strings.Join(c.NATSURLs(), ","), opts...)
	if err != nil {
		t.Fatalf("harness: connect to cluster NATS: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// JetStream returns a JetStream handle over a connection to the first
// live node with NATS. The connection closes at test end.
func (c *Cluster) JetStream(t testing.TB) jetstream.JetStream {
	t.Helper()
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
	nc, err := nats.Connect(g.NATSURL())
	if err != nil {
		t.Fatalf("harness: connect to %s NATS: %v", g.Name, err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("harness: jetstream: %v", err)
	}
	return js
}

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
	streams := make([]string, len(buckets))
	for i, b := range buckets {
		streams[i] = "KV_" + b
	}
	c.WaitStreamReplicas(t, want, timeout, streams...)
}

// WaitStreamReplicas waits until each named JetStream stream exists
// with want replicas, a leader and want-1 current followers, and no
// replica-count change in progress.
func (c *Cluster) WaitStreamReplicas(t testing.TB, want int, timeout time.Duration, streams ...string) {
	t.Helper()
	js := c.JetStream(t)
	Eventually(t, timeout, fmt.Sprintf("streams %v replicated %dx", streams, want), func() error {
		for _, name := range streams {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			s, err := js.Stream(ctx, name)
			cancel()
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			info := s.CachedInfo()
			cl := info.Cluster
			switch {
			case info.Config.Replicas != want:
				return fmt.Errorf("%s: replicas %d", name, info.Config.Replicas)
			case cl == nil || cl.Leader == "":
				return fmt.Errorf("%s: no leader", name)
			case cl.Desired != nil:
				return fmt.Errorf("%s: replica change in progress", name)
			case len(cl.Replicas) != want-1:
				return fmt.Errorf("%s: %d followers", name, len(cl.Replicas))
			}
			for _, p := range cl.Replicas {
				if !p.Current || p.Offline {
					return fmt.Errorf("%s: follower %s not current", name, p.Name)
				}
			}
		}
		return nil
	})
}
