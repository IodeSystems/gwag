package gat_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/gat/bustest"
)

func TestLocalBus_Conformance(t *testing.T) {
	bustest.Run(t, func(t *testing.T) bustest.Harness {
		return bustest.Harness{Bus: gat.NewLocalBus()}
	})
}

func TestPeerMeshBus_Conformance(t *testing.T) {
	bustest.Run(t, func(t *testing.T) bustest.Harness {
		b := gat.NewPeerMeshBus(gat.PeerMesh{})
		t.Cleanup(b.Close)
		return bustest.Harness{Bus: b}
	})
}

// TestPeerMeshBus_CrossNode: a publish on A reaches A's subscriber
// (local) and B's subscriber (via B's Handler) once each.
func TestPeerMeshBus_CrossNode(t *testing.T) {
	key := []byte("k")
	bB := gat.NewPeerMeshBus(gat.PeerMesh{Auth: key})
	defer bB.Close()
	srvB := httptest.NewServer(bB.Handler())
	defer srvB.Close()
	bA := gat.NewPeerMeshBus(gat.PeerMesh{Peers: []string{srvB.URL}, Auth: key})
	defer bA.Close()

	gotA, gotB := make(chan string, 4), make(chan string, 4)
	unA, _ := bA.Subscribe("e.*", func(c string, p []byte) { gotA <- c + "=" + string(p) })
	unB, _ := bB.Subscribe("e.*", func(c string, p []byte) { gotB <- c + "=" + string(p) })
	defer unA()
	defer unB()

	if err := bA.Publish("e.1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for name, ch := range map[string]chan string{"A": gotA, "B": gotB} {
		select {
		case s := <-ch:
			if s != "e.1=x" {
				t.Errorf("%s got %q", name, s)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: timeout", name)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if len(gotA)+len(gotB) != 0 {
		t.Errorf("duplicate deliveries: A=%d B=%d", len(gotA), len(gotB))
	}
}

func TestLocalBus_RejectsEmpty(t *testing.T) {
	b := gat.NewLocalBus()
	if err := b.Publish("", nil); err == nil {
		t.Error("Publish(\"\"): want error")
	}
	if _, err := b.Subscribe("", func(string, []byte) {}); err == nil {
		t.Error("Subscribe(\"\"): want error")
	}
}

// TestMatchChannel is the single table for the matcher shared by gat
// (LocalBus, peer mesh) and gwag (channel auth, channel bindings). It
// merges the cases each side pinned before the matcher was shared,
// plus the edge cases where the two used to differ.
func TestMatchChannel(t *testing.T) {
	cases := []struct {
		pattern, channel string
		want             bool
	}{
		// exact
		{"users.42", "users.42", true},
		{"users.42", "users.43", false},
		{"a.b.c", "a.b", false},
		{"a.b", "a.b.c", false},
		{"events.orders.42.update", "events.orders.42.update", true},
		// `*` — exactly one segment
		{"users.*", "users.42", true},
		{"users.*", "users.42.profile", false},
		{"*.created", "order.created", true},
		{"*.created", "order.deleted", false},
		{"events.*.update", "events.orders.update", true},
		{"events.*.update", "events.orders.42.update", false},
		{"*", "x", true},
		{"*", "x.y", false},
		// `>` — one or more trailing segments
		{"users.>", "users.42", true},
		{"users.>", "users.42.profile", true},
		{"users.>", "users", false},
		{"events.orders.>", "events.orders.42.update", true},
		{"events.orders.>", "events.orders", false},
		{"events.orders.>", "events.public.foo", false},
		{">", "anything.at.all", true},
		{">", "x", true},
		{"*.>", "a.b", true},
		{"*.>", "a", false},
		// empty never matches
		{">", "", false},
		{"", "", false},
		{"", "a", false},
		{"a", "", false},
		// `>` not last is not a valid NATS subject: matches nothing.
		// (gwag's old matcher treated it as "match the rest"; gat's
		// old matcher and NATS itself did not.)
		{"a.>.b", "a.x.b", false},
		{"a.>.b", "a.x", false},
		// empty segments are literal
		{"a..b", "a..b", true},
		{"a.*.b", "a..b", true},
	}
	for _, c := range cases {
		if got := gat.MatchChannel(c.pattern, c.channel); got != c.want {
			t.Errorf("MatchChannel(%q, %q) = %v, want %v", c.pattern, c.channel, got, c.want)
		}
	}
}

func BenchmarkMatchChannel(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		gat.MatchChannel("events.*.orders.>", "events.eu.orders.42.created")
	}
}
