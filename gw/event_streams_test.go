package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func waitStream(t *testing.T, js jetstream.JetStream, name string, cond func(jetstream.StreamConfig) bool) jetstream.StreamConfig {
	t.Helper()
	var last jetstream.StreamConfig
	waitGate(t, 20*time.Second, "stream "+name, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s, err := js.Stream(ctx, name)
		if err != nil {
			return false
		}
		info, err := s.Info(ctx)
		if err != nil {
			return false
		}
		last = info.Config
		return cond(last)
	})
	return last
}

// TestEventStreams_EnsuredPerNamedChannel: each named channel gets a
// stream with the documented config; a typed ps.pub lands in it once
// even when the same message id is published twice.
func TestEventStreams_EnsuredPerNamedChannel(t *testing.T) {
	g, cl := typedPubSubGateway(t, WithEventStreamMaxAge(time.Hour))
	cfg := waitStream(t, cl.JS, "EV_orders_v1_OrderEvents", func(jetstream.StreamConfig) bool { return true })
	if len(cfg.Subjects) != 1 || cfg.Subjects[0] != "events.orders.*" {
		t.Errorf("subjects = %v", cfg.Subjects)
	}
	if cfg.MaxAge != time.Hour || cfg.Duplicates != 2*time.Minute || cfg.Storage != jetstream.FileStorage || cfg.Retention != jetstream.LimitsPolicy || cfg.Replicas != 1 {
		t.Errorf("config = %+v", cfg)
	}
	waitStream(t, cl.JS, "EV_orders_v1_Audit", func(c jetstream.StreamConfig) bool { return c.Subjects[0] == "events.audit" })

	pub, _ := psDispatchers(t, g)
	if _, err := pub.Dispatch(context.Background(), map[string]any{
		"channel": "events.audit", "payload": `{"who":"ann"}`,
	}); err != nil {
		t.Fatal(err)
	}
	// Same id twice straight onto NATS: the duplicates window keeps one.
	for i := 0; i < 2; i++ {
		m := nats.NewMsg("events.audit")
		m.Header[EventHeaderType] = []string{"orders.v1.Audit"}
		m.Header[EventHeaderMsgID] = []string{"dup-1"}
		if err := cl.Conn.PublishMsg(m); err != nil {
			t.Fatal(err)
		}
	}
	_ = cl.Conn.Flush()
	waitGate(t, 10*time.Second, "2 messages in EV_orders_v1_Audit", func() bool {
		s, err := cl.JS.Stream(context.Background(), "EV_orders_v1_Audit")
		if err != nil {
			return false
		}
		info, err := s.Info(context.Background())
		return err == nil && info.State.Msgs == 2
	})
}

// TestEventStreams_ClusterReplicaPolicy: on the 2-node shared cluster
// with the auto policy the stream reaches R2, like the KV buckets.
func TestEventStreams_ClusterReplicaPolicy(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	waitForPeers(t, a.gw, b.gw)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = testCluster.a.JS.DeleteStream(ctx, "EV_orders_v1_OrderEvents")
		_ = testCluster.a.JS.DeleteStream(ctx, "EV_orders_v1_Audit")
	})
	if err := installEventSlot(t, a.gw, "orders", "v1", "orders.proto", ordersEventsProto); err != nil {
		t.Fatal(err)
	}
	a.gw.mu.Lock()
	err := a.gw.assembleLocked()
	a.gw.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	waitStream(t, testCluster.a.JS, "EV_orders_v1_OrderEvents", func(c jetstream.StreamConfig) bool { return c.Replicas == 2 })
}

// TestEventStreams_ReplicasFollowPolicyWhileCurrentRLags: a node whose
// own KV replica raise has not been answered yet (currentR still 1)
// ensures streams at the policy for its live peers, not at R1. When
// a cluster forms every node raises the KV buckets at once and NATS
// leaves some of the identical requests unanswered, so currentR can
// stay behind for a peer-refresh interval or more (e2e story S11).
func TestEventStreams_ReplicasFollowPolicyWhileCurrentRLags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   ClusterReplicas
		live     int
		currentR int32
		want     int
	}{
		{"lagging currentR, odd, 3 live", ClusterReplicasOdd, 3, 1, 3},
		{"lagging currentR, auto, 2 live", ClusterReplicasAuto, 2, 1, 2},
		{"odd, 2 live stays R1", ClusterReplicasOdd, 2, 1, 1},
		{"never below currentR", ClusterReplicasOdd, 1, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &Gateway{cfg: &config{clusterReplicas: tc.policy}}
			p := &peerTracker{gw: g, live: map[string]struct{}{}}
			for i := 0; i < tc.live; i++ {
				p.live[string(rune('a'+i))] = struct{}{}
			}
			p.currentR.Store(tc.currentR)
			g.peers = p
			if got := g.eventStreamReplicas(); got != tc.want {
				t.Errorf("eventStreamReplicas() = %d, want %d", got, tc.want)
			}
		})
	}
}
