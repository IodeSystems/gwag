package gat

import (
	"testing"
	"time"
)

func TestMatchChannel(t *testing.T) {
	tok := func(s string) []string {
		if s == "" {
			return nil
		}
		return splitDot(s)
	}
	cases := []struct {
		pattern, channel string
		want             bool
	}{
		{"users.42", "users.42", true},
		{"users.42", "users.43", false},
		{"users.*", "users.42", true},
		{"users.*", "users.42.profile", false}, // * is exactly one segment
		{"users.>", "users.42", true},
		{"users.>", "users.42.profile", true}, // > is one-or-more
		{"users.>", "users", false},           // > needs at least one
		{"*.created", "order.created", true},
		{"*.created", "order.deleted", false},
		{">", "anything.at.all", true},
		{"a.b.c", "a.b", false},
		{"a.b", "a.b.c", false},
	}
	for _, c := range cases {
		got := matchChannel(tok(c.pattern), tok(c.channel))
		if got != c.want {
			t.Errorf("matchChannel(%q, %q) = %v, want %v", c.pattern, c.channel, got, c.want)
		}
	}
}

func splitDot(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == '.' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

func TestPubSub_FanoutToMatchingSubscribers(t *testing.T) {
	ps := newPubSub()
	exact, cancelExact := ps.Subscribe("users.42")
	glob, cancelGlob := ps.Subscribe("users.*")
	other, cancelOther := ps.Subscribe("orders.>")
	defer cancelExact()
	defer cancelGlob()
	defer cancelOther()

	ps.Publish("users.42", []byte("hi"))

	if ev := recvWithin(t, exact); string(ev.Payload) != "hi" || ev.Channel != "users.42" {
		t.Errorf("exact subscriber got %+v", ev)
	}
	if ev := recvWithin(t, glob); string(ev.Payload) != "hi" {
		t.Errorf("glob subscriber got %+v", ev)
	}
	if _, ok := tryRecv(other); ok {
		t.Error("orders.> subscriber should not have received a users.42 event")
	}
}

func TestPubSub_CancelStopsDelivery(t *testing.T) {
	ps := newPubSub()
	ch, cancel := ps.Subscribe("c")
	cancel()
	if _, ok := <-ch; ok {
		t.Error("channel should be closed after cancel")
	}
	// Publishing after cancel must not panic.
	ps.Publish("c", []byte("x"))
}

func TestPubSub_SlowSubscriberDropsOldest(t *testing.T) {
	ps := newPubSub()
	ch, cancel := ps.Subscribe("c")
	defer cancel()
	// Publish more than the buffer holds without draining.
	for i := range subscriberQueueDepth * 3 {
		ps.Publish("c", []byte{byte(i)})
	}
	// The publisher never blocked; the buffer holds the most recent
	// subscriberQueueDepth events.
	n := 0
	for {
		if _, ok := tryRecv(ch); !ok {
			break
		}
		n++
	}
	if n == 0 || n > subscriberQueueDepth {
		t.Errorf("buffered %d events; want 1..%d", n, subscriberQueueDepth)
	}
}

func TestGateway_PubSubFacade(t *testing.T) {
	g, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Close()
	ps := g.PubSub()
	ch, cancel := ps.Subscribe("events.*")
	defer cancel()
	ps.Publish("events.created", []byte("payload"))
	if ev := recvWithin(t, ch); string(ev.Payload) != "payload" {
		t.Errorf("facade delivered %q", ev.Payload)
	}
}

func TestGateway_CloseCancelsSubscriptions(t *testing.T) {
	g, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := g.PubSub().Subscribe("c")
	g.Close()
	if _, ok := <-ch; ok {
		t.Error("Close should close active subscription channels")
	}
}

func recvWithin(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func tryRecv(ch <-chan Event) (Event, bool) {
	select {
	case ev := <-ch:
		return ev, true
	default:
		return Event{}, false
	}
}
