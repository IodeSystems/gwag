package gateway

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

func TestClusterReplicas_For(t *testing.T) {
	cases := []struct {
		policy ClusterReplicas
		live   int
		want   int
	}{
		{"", 0, 1},
		{"", 1, 1},
		{"", 2, 1},
		{"", 3, 3},
		{"", 4, 3},
		{"", 9, 3},
		{"odd", 2, 1},
		{"odd", 3, 3},
		{"auto", 1, 1},
		{"auto", 2, 2},
		{"auto", 3, 3},
		{"auto", 7, 3},
		{"1", 3, 1},
		{"2", 5, 2},
		{"2", 1, 1},
		{"5", 3, 3},
		{"5", 7, 5},
	}
	for _, c := range cases {
		if got := c.policy.For(c.live); got != c.want {
			t.Errorf("%q.For(%d) = %d, want %d", c.policy, c.live, got, c.want)
		}
	}
}

func TestParseClusterReplicas(t *testing.T) {
	ok := map[string]ClusterReplicas{"": "odd", "odd": "odd", " AUTO ": "auto", "1": "1", "5": "5"}
	for in, want := range ok {
		got, err := ParseClusterReplicas(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"0", "6", "-1", "even", "2.5", "three"} {
		if _, err := ParseClusterReplicas(in); err == nil {
			t.Errorf("Parse(%q): want error", in)
		}
	}
}

func TestClusterReplicas_InvalidPanicsInNew(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with an invalid policy did not panic")
		}
	}()
	New(WithClusterReplicas("bogus"), WithoutMetrics())
}

// With the default (odd) policy, two nodes stay at R1 and the third
// raises the buckets straight to R3, never through R2.
func TestClusterReplicas_Odd(t *testing.T) {
	addrs := []string{freeAddr(t), freeAddr(t), freeAddr(t)}
	start := func(i int) *Gateway {
		var seeds []string
		for j, a := range addrs {
			if j != i {
				seeds = append(seeds, a)
			}
		}
		cl, err := StartCluster(ClusterOptions{
			NodeName:      fmt.Sprintf("odd-%d", i),
			ClientListen:  freeAddr(t),
			ClusterListen: addrs[i],
			Peers:         seeds,
			DataDir:       t.TempDir(),
			StartTimeout:  15 * time.Second,
			LogLevel:      "silent",
		})
		if err != nil {
			t.Fatalf("StartCluster %d: %v", i, err)
		}
		t.Cleanup(cl.Close)
		g := New(WithCluster(cl), WithoutMetrics(), WithoutBackpressure())
		t.Cleanup(g.Close)
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer()
		cpv1.RegisterControlPlaneServer(srv, g.ControlPlane())
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		return g
	}
	tracker := func(g *Gateway) *peerTracker {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.peers
	}
	// bucketReplicas reads the configured replica count of every
	// managed bucket stream.
	bucketReplicas := func(g *Gateway) (map[string]int, error) {
		out := map[string]int{}
		for _, b := range []string{peersBucketName, registryBucketName, stableBucketName, deprecatedBucketName, mcpConfigBucketName} {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			s, err := g.cfg.cluster.JS.Stream(ctx, "KV_"+b)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("%s: %w", b, err)
			}
			info, err := s.Info(ctx)
			cancel()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", b, err)
			}
			out[b] = info.Config.Replicas
		}
		return out, nil
	}

	g0, g1 := start(0), start(1)
	waitUntilErr(t, 60*time.Second, "two nodes see 2 peers", func() error {
		for i, g := range []*Gateway{g0, g1} {
			pt := tracker(g)
			if pt == nil {
				return fmt.Errorf("node %d: no tracker", i)
			}
			if l := len(pt.LivePeers()); l != 2 {
				return fmt.Errorf("node %d: %d live peers", i, l)
			}
		}
		return nil
	})
	// Give a reconcile every chance to (wrongly) raise to R2.
	time.Sleep(3 * time.Second)
	for i, g := range []*Gateway{g0, g1} {
		if r := tracker(g).currentR.Load(); r != 1 {
			t.Fatalf("node %d: currentR = %d with 2 peers, want 1", i, r)
		}
	}
	got, err := bucketReplicas(g0)
	if err != nil {
		t.Fatal(err)
	}
	for b, r := range got {
		if r != 1 {
			t.Errorf("2 nodes: %s replicas = %d, want 1", b, r)
		}
	}

	g2 := start(2)
	waitUntilErr(t, 60*time.Second, "three nodes at R3", func() error {
		for i, g := range []*Gateway{g0, g1, g2} {
			pt := tracker(g)
			if pt == nil {
				return fmt.Errorf("node %d: no tracker", i)
			}
			if l := len(pt.LivePeers()); l != 3 {
				return fmt.Errorf("node %d: %d live peers", i, l)
			}
		}
		if tracker(g0).currentR.Load() != 3 {
			return fmt.Errorf("currentR = %d", tracker(g0).currentR.Load())
		}
		got, err := bucketReplicas(g0)
		if err != nil {
			return err
		}
		for b, r := range got {
			if r != 3 {
				return fmt.Errorf("%s replicas = %d", b, r)
			}
		}
		return nil
	})
}
