package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/gwag/gw/ir"
)

// TestPubSub_RoundTrip exercises the gateway-bundled gwag.ps.v1.PubSub
// dispatchers end-to-end against an embedded NATS cluster: subscribe
// via ps.sub, publish via ps.pub, and verify the subscriber receives
// the Event payload. Pre-empts the auth-tier work — this iteration
// ships the broker primitive only; tier policy lands in commit 2 of
// the workstream.
func TestPubSub_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	cluster, err := StartCluster(ClusterOptions{
		NodeName:      "pubsub-test",
		ClientListen:  "127.0.0.1:0",
		ClusterListen: "127.0.0.1:0",
		DataDir:       dir,
		StartTimeout:  10 * time.Second,
		LogLevel:      "silent",
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cluster.Close)

	g := New(WithCluster(cluster), WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)

	// Trigger a schema assemble so dispatchers are registered.
	g.mu.Lock()
	if err := g.assembleLocked(); err != nil {
		g.mu.Unlock()
		t.Fatalf("assembleLocked: %v", err)
	}
	g.mu.Unlock()

	pubSID := ir.MakeSchemaID(psNamespace, psVersion, "Pub")
	subSID := ir.MakeSchemaID(psNamespace, psVersion, "Sub")
	pub := g.dispatchers.Get(pubSID)
	if pub == nil {
		t.Fatalf("no Pub dispatcher registered under %s", pubSID)
	}
	sub := g.dispatchers.Get(subSID)
	if sub == nil {
		t.Fatalf("no Sub dispatcher registered under %s", subSID)
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	t.Cleanup(subCancel)

	channel := "events.test.42"
	src, err := sub.Dispatch(subCtx, map[string]any{"channel": channel})
	if err != nil {
		t.Fatalf("Sub dispatch: %v", err)
	}
	events, ok := src.(chan any)
	if !ok {
		t.Fatalf("Sub returned %T, want chan any", src)
	}

	// nats.Subscribe is asynchronous — give the broker a beat to bind
	// the per-subject NATS subscription before the publish lands.
	// Without this the Pub fires into a void and the test hangs.
	deadline := time.Now().Add(2 * time.Second)
	for cluster.Server.NumSubscriptions() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cluster.Server.NumSubscriptions() == 0 {
		t.Fatal("broker did not bind a NATS subscription within 2s")
	}

	if _, err := pub.Dispatch(context.Background(), map[string]any{
		"channel": channel,
		"payload": "hello world",
	}); err != nil {
		t.Fatalf("Pub dispatch: %v", err)
	}

	select {
	case raw, ok := <-events:
		if !ok {
			t.Fatal("subscription channel closed before any event arrived")
		}
		ev, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("event %T, want map[string]any", raw)
		}
		if got := ev["channel"]; got != channel {
			t.Errorf("event.channel = %v, want %q", got, channel)
		}
		if got := ev["payload"]; got != "hello world" {
			t.Errorf("event.payload = %v, want %q", got, "hello world")
		}
		// Event.ts is wall-clock at Pub entry — int64 stringified by
		// messageToMap to preserve precision over the JSON wire.
		ts, ok := ev["ts"].(string)
		if !ok {
			t.Fatalf("event.ts %T = %v, want string", ev["ts"], ev["ts"])
		}
		if ts == "" || ts == "0" {
			t.Errorf("event.ts = %q, want > 0", ts)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

// TestPubSub_RejectsWildcardOnPublish pins the publish-side validation
// — wildcards belong to subscribe.
func TestPubSub_RejectsWildcardOnPublish(t *testing.T) {
	dir := t.TempDir()
	cluster, err := StartCluster(ClusterOptions{
		NodeName:      "pubsub-test",
		ClientListen:  "127.0.0.1:0",
		ClusterListen: "127.0.0.1:0",
		DataDir:       dir,
		StartTimeout:  10 * time.Second,
		LogLevel:      "silent",
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cluster.Close)

	g := New(WithCluster(cluster), WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)

	g.mu.Lock()
	if err := g.assembleLocked(); err != nil {
		g.mu.Unlock()
		t.Fatalf("assembleLocked: %v", err)
	}
	g.mu.Unlock()

	pub := g.dispatchers.Get(ir.MakeSchemaID(psNamespace, psVersion, "Pub"))
	for _, ch := range []string{"events.*", "events.>", "events.foo.>.bar"} {
		if _, err := pub.Dispatch(context.Background(), map[string]any{
			"channel": ch,
			"payload": "x",
		}); err == nil {
			t.Errorf("Pub on wildcard channel %q: want error, got nil", ch)
		}
	}
}

// TestPubSub_NoClusterSkipsInstall pins the boot-time gate: a
// standalone gateway (no WithCluster) does not register the ps/v1
// slot, so ps.pub / ps.sub do not appear in the schema. This matches
// the proto-streaming Subscription path which also requires a cluster
// to function — keeping the public surface tied to deployment
// capability instead of advertising fields that always error.
func TestPubSub_NoClusterSkipsInstall(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)

	g.mu.Lock()
	if err := g.assembleLocked(); err != nil {
		g.mu.Unlock()
		t.Fatalf("assembleLocked: %v", err)
	}
	_, ok := g.slots[poolKey{namespace: psNamespace, version: psVersion}]
	g.mu.Unlock()
	if ok {
		t.Fatal("ps/v1 slot was installed without cluster; expected to be skipped")
	}
	if d := g.dispatchers.Get(ir.MakeSchemaID(psNamespace, psVersion, "Pub")); d != nil {
		t.Error("Pub dispatcher registered without cluster")
	}
	if d := g.dispatchers.Get(ir.MakeSchemaID(psNamespace, psVersion, "Sub")); d != nil {
		t.Error("Sub dispatcher registered without cluster")
	}
}
