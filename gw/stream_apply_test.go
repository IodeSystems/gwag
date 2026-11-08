package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// lineLogger records the NATS server log lines the gateway writes
// through Server.Warnf / Errorf.
type lineLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *lineLogger) add(level, f string, a ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(f, a...))
	l.mu.Unlock()
}
func (l *lineLogger) Noticef(string, ...any)    {}
func (l *lineLogger) Debugf(string, ...any)     {}
func (l *lineLogger) Tracef(string, ...any)     {}
func (l *lineLogger) Warnf(f string, a ...any)  { l.add("WRN", f, a...) }
func (l *lineLogger) Errorf(f string, a ...any) { l.add("ERR", f, a...) }
func (l *lineLogger) Fatalf(f string, a ...any) { l.add("FTL", f, a...) }

// gatewayLines is the captured lines the gateway wrote.
func (l *lineLogger) gatewayLines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.lines {
		if strings.Contains(s, "gateway:") {
			out = append(out, s)
		}
	}
	return out
}

// startThreeNodeNATS starts a 3-node NATS cluster (no gateways), all
// nodes at once, and waits for JetStream.
func startThreeNodeNATS(t *testing.T) ([]*Cluster, []*lineLogger) {
	t.Helper()
	addrs := []string{freeAddr(t), freeAddr(t), freeAddr(t)}
	cls := make([]*Cluster, 3)
	logs := make([]*lineLogger, 3)
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i := range addrs {
		var seeds []string
		for j, a := range addrs {
			if j != i {
				seeds = append(seeds, a)
			}
		}
		logs[i] = &lineLogger{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cls[i], errs[i] = StartCluster(ClusterOptions{
				NodeName:      fmt.Sprintf("herd-%d", i),
				ClientListen:  freeAddr(t),
				ClusterListen: addrs[i],
				Peers:         seeds,
				DataDir:       t.TempDir(),
				StartTimeout:  15 * time.Second,
				Logger:        logs[i],
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("StartCluster %d: %v", i, err)
		}
		t.Cleanup(cls[i].Close)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, cl := range cls {
		if err := cl.WaitForJetStream(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return cls, logs
}

// createR1Stream creates name at R1, retrying while the meta leader
// is elected.
func createR1Stream(t *testing.T, js jetstream.JetStream, name string) jetstream.StreamConfig {
	t.Helper()
	cfg := jetstream.StreamConfig{Name: name, Subjects: []string{"herd." + name}, Replicas: 1}
	waitUntilErr(t, 30*time.Second, "create "+name, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := js.CreateStream(ctx, cfg)
		return err
	})
	return cfg
}

// TestApplyStream_ConcurrentIdenticalUpdates covers the cluster
// formation herd: every node raises the same streams at the same
// moment.
//
// Part 1 shows the NATS behaviour: three concurrent identical
// R1→R3 UpdateStream calls, one per node. nats-server v2.15.2
// leaves one of them unanswered (its caller times out) while the
// stream still ends at R3. Logged, not asserted: it is the server's
// behaviour, not the gateway's.
//
// Part 2: applyStream from the three nodes at once returns on every
// node with the stream at R3: fast when only the meta leader writes,
// within one attempt timeout when every node writes.
//
// Part 3: three gateways on the cluster, all starting at once with
// the same event channel. Every node's currentR reaches 3, the KV
// buckets and the event stream reach R3, and no gateway logs a
// warning (no "event stream … context deadline exceeded").
func TestApplyStream_ConcurrentIdenticalUpdates(t *testing.T) {
	cls, logs := startThreeNodeNATS(t)

	// Part 1.
	cfg := createR1Stream(t, cls[0].JS, "RAW")
	cfg.Replicas = 3
	var mu sync.Mutex
	var unanswered int
	var wg sync.WaitGroup
	for i := range cls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := cls[i].JS.UpdateStream(ctx, cfg)
			if errors.Is(err, context.DeadlineExceeded) {
				mu.Lock()
				unanswered++
				mu.Unlock()
			} else if err != nil {
				t.Errorf("node %d: raw update: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	t.Logf("raw: %d of 3 concurrent identical UpdateStream calls unanswered within 3s", unanswered)

	// Part 2. "leader" is the real meta-leader check; "every node
	// writes" makes all three write at once, so a lost answer is
	// recovered by re-reading the stream.
	for _, mode := range []struct {
		stream string
		leader func(*Cluster) func() bool
	}{
		{"LEADER", func(cl *Cluster) func() bool { return cl.Server.JetStreamIsLeader }},
		{"EVERYNODE", func(*Cluster) func() bool { return func() bool { return true } }},
	} {
		createR1Stream(t, cls[0].JS, mode.stream)
		start := time.Now()
		errs := make([]error, 3)
		took := make([]time.Duration, 3)
		for i, cl := range cls {
			wg.Add(1)
			go func(i int, cl *Cluster) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				got, err := applyStream(ctx, cl.JS, mode.stream, mode.leader(cl), func(cur *jetstream.StreamConfig) (jetstream.StreamConfig, bool) {
					if cur == nil {
						return jetstream.StreamConfig{}, false
					}
					w := *cur
					w.Replicas = 3
					return w, true
				})
				if err == nil && got.Replicas != 3 {
					err = fmt.Errorf("returned replicas = %d", got.Replicas)
				}
				errs[i], took[i] = err, time.Since(start)
			}(i, cl)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("%s node %d: applyStream: %v", mode.stream, i, err)
			}
		}
		t.Logf("%s: applyStream returned after %v", mode.stream, took)
		for i, d := range took {
			// The follower wait, or one unanswered attempt, plus one
			// answered write.
			if d > applyStreamFollowerWait+applyStreamAttemptTimeout {
				t.Errorf("%s node %d: applyStream took %v", mode.stream, i, d)
			}
		}
	}

	// Part 3.
	gws := make([]*Gateway, 3)
	for i, cl := range cls {
		g := New(WithCluster(cl), WithoutMetrics(), WithoutBackpressure(), WithChannelAuth("events.>", ChannelAuthOpen))
		t.Cleanup(g.Close)
		gws[i] = g
	}
	start := time.Now()
	for _, g := range gws {
		wg.Add(1)
		go func(g *Gateway) {
			defer wg.Done()
			g.ControlPlane() // boots cluster tracking
			if err := installEventSlot(t, g, "orders", "v1", "orders.proto", ordersEventsProto); err != nil {
				t.Error(err)
				return
			}
			g.mu.Lock()
			err := g.assembleLocked()
			g.mu.Unlock()
			if err != nil {
				t.Error(err)
			}
		}(g)
	}
	wg.Wait()
	streams := []string{"EV_orders_v1_OrderEvents", "EV_orders_v1_Audit"}
	for _, b := range replicaBuckets {
		streams = append(streams, "KV_"+b)
	}
	waitUntilErr(t, 10*time.Second, "every node at R3", func() error {
		for i, g := range gws {
			g.mu.Lock()
			pt := g.peers
			g.mu.Unlock()
			if pt == nil {
				return fmt.Errorf("node %d: no tracker", i)
			}
			if r := pt.currentR.Load(); r != 3 {
				return fmt.Errorf("node %d: currentR = %d", i, r)
			}
		}
		for _, n := range streams {
			cfg, err := readStreamConfig(context.Background(), cls[0].JS, n)
			if err != nil {
				return err
			}
			if cfg == nil || cfg.Replicas != 3 {
				return fmt.Errorf("%s not at R3", n)
			}
		}
		return nil
	})
	t.Logf("gateways: R3 everywhere after %v", time.Since(start).Round(time.Millisecond))
	for i, l := range logs {
		for _, line := range l.gatewayLines() {
			// Boot can log this while JetStream meta settles; it is
			// not a stream write.
			if strings.Contains(line, "gateway: cluster tracking pending") {
				continue
			}
			t.Errorf("node %d logged: %s", i, line)
		}
	}
}
