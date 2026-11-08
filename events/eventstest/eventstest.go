// Package eventstest runs the shared behaviour checks every
// events.Backend must pass: fan-out across groups, work sharing in a
// group, redelivery, dead-lettering, dedup. Backend packages call Run
// from their tests.
//
// Stability: experimental
package eventstest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/iodesystems/gwag/v2/events"
)

// Harness gives Run a fresh backend per subtest.
type Harness struct {
	// New returns an empty backend. Each call must not see messages
	// from earlier calls (use a fresh server or unique channel keys).
	New func(t *testing.T) events.Backend
	// DeadLetters waits until n dead letters for stream exist and
	// returns them.
	DeadLetters func(t *testing.T, b events.Backend, stream string, n int) []*events.Message
	// Settle waits after Subscribe returns until the subscriber is
	// receiving. Optional.
	Settle time.Duration
	// NoDedup skips the dedup check, for backends without message-id
	// dedup (Kafka).
	NoDedup bool
}

var (
	seq atomic.Int64
	run = events.NewID()[:8]
)

// Channel returns a channel with a key unique to this process, so
// runs against a long-lived broker do not see each other's messages.
func Channel(t *testing.T) events.Channel {
	n := seq.Add(1)
	return events.MustChannel(fmt.Sprintf("test.v1.C%s_%d", run, n), fmt.Sprintf("test.c%s_%d.{id}", run, n))
}

// Run runs every check.
func Run(t *testing.T, h Harness) {
	t.Run("FanOutAcrossGroups", func(t *testing.T) { fanOut(t, h) })
	t.Run("GroupSharesWork", func(t *testing.T) { groupShares(t, h) })
	t.Run("Redelivery", func(t *testing.T) { redelivery(t, h) })
	t.Run("DeadLetter", func(t *testing.T) { deadLetter(t, h) })
	if !h.NoDedup {
		t.Run("Dedup", func(t *testing.T) { dedup(t, h) })
	}
	t.Run("DeliverAll", func(t *testing.T) { deliverAll(t, h) })
}

type got struct {
	mu   sync.Mutex
	msgs []string
}

func (g *got) add(s string) { g.mu.Lock(); g.msgs = append(g.msgs, s); g.mu.Unlock() }
func (g *got) len() int     { g.mu.Lock(); defer g.mu.Unlock(); return len(g.msgs) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sub(t *testing.T, h Harness, c *events.Core, ch events.Channel, group string, fn func(ctx context.Context, segs []string, m *events.Message) error, opts ...events.SubscribeOption) {
	t.Helper()
	stop, err := c.Subscribe(context.Background(), ch, group, fn, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	time.Sleep(h.Settle)
}

func pub(t *testing.T, c *events.Core, ch events.Channel, id, body string, opts ...events.PublishOption) {
	t.Helper()
	if err := c.Publish(context.Background(), ch, []string{id}, wrapperspb.String(body), opts...); err != nil {
		t.Fatal(err)
	}
}

func decode(m *events.Message) string {
	var v wrapperspb.StringValue
	if err := proto.Unmarshal(m.Data, &v); err != nil {
		return "decode error: " + err.Error()
	}
	return v.Value
}

func fanOut(t *testing.T, h Harness) {
	b := h.New(t)
	c := events.NewCore(b)
	ch := Channel(t)
	var a, bb got
	sub(t, h, c, ch, "a", func(ctx context.Context, segs []string, m *events.Message) error {
		if events.MessageFrom(ctx) != m {
			t.Error("MessageFrom(ctx) is not the delivered message")
		}
		if m.Type() != "google.protobuf.StringValue" || m.Headers[events.HeaderChannel] != ch.Key {
			t.Errorf("headers = %v", m.Headers)
		}
		a.add(segs[0] + "=" + decode(m))
		return nil
	})
	sub(t, h, c, ch, "b", func(ctx context.Context, segs []string, m *events.Message) error {
		bb.add(segs[0] + "=" + decode(m))
		return nil
	})
	pub(t, c, ch, "x1", "hello")
	waitFor(t, "both groups", func() bool { return a.len() == 1 && bb.len() == 1 })
	if a.msgs[0] != "x1=hello" || bb.msgs[0] != "x1=hello" {
		t.Fatalf("a=%v b=%v", a.msgs, bb.msgs)
	}
	want := []events.Subscription{{Channel: ch.Key, Group: "a"}, {Channel: ch.Key, Group: "b"}}
	if s := c.Subscriptions(); len(s) != 2 || s[0] != want[0] || s[1] != want[1] {
		t.Fatalf("Subscriptions() = %v", s)
	}
}

func groupShares(t *testing.T, h Harness) {
	b := h.New(t)
	c := events.NewCore(b)
	ch := Channel(t)
	var one, two got
	// Slow handlers so both subscribers pick up work.
	slow := func(g *got) events.SegmentHandler {
		return func(ctx context.Context, segs []string, m *events.Message) error {
			time.Sleep(5 * time.Millisecond)
			g.add(decode(m))
			return nil
		}
	}
	sub(t, h, c, ch, "shared", slow(&one))
	sub(t, h, c, ch, "shared", slow(&two))
	const n = 40
	// Distinct subjects, so a backend that partitions by subject
	// (Kafka) spreads them.
	for i := 0; i < n; i++ {
		pub(t, c, ch, fmt.Sprint("k", i), fmt.Sprint(i))
	}
	waitFor(t, "all messages", func() bool { return one.len()+two.len() >= n })
	time.Sleep(50 * time.Millisecond)
	if total := one.len() + two.len(); total != n {
		t.Fatalf("delivered %d, want %d (each message once per group)", total, n)
	}
	if one.len() == 0 || two.len() == 0 {
		t.Fatalf("work not shared: %d / %d", one.len(), two.len())
	}
	if len(c.Subscriptions()) != 1 {
		t.Fatalf("Subscriptions() = %v, want one entry", c.Subscriptions())
	}
}

func redelivery(t *testing.T, h Harness) {
	b := h.New(t)
	c := events.NewCore(b)
	ch := Channel(t)
	var deliveries []int
	var mu sync.Mutex
	done := make(chan struct{})
	sub(t, h, c, ch, "r", func(ctx context.Context, segs []string, m *events.Message) error {
		mu.Lock()
		defer mu.Unlock()
		deliveries = append(deliveries, m.Delivery)
		if m.Delivery < 3 {
			return errors.New("not yet")
		}
		close(done)
		return nil
	}, events.Backoff(10*time.Millisecond), events.MaxDeliver(5))
	pub(t, c, ch, "k", "retry me")
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("no third delivery")
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(deliveries) != "[1 2 3]" {
		t.Fatalf("deliveries = %v", deliveries)
	}
}

func deadLetter(t *testing.T, h Harness) {
	b := h.New(t)
	c := events.NewCore(b)
	ch := Channel(t)
	var calls atomic.Int32
	sub(t, h, c, ch, "d", func(ctx context.Context, segs []string, m *events.Message) error {
		calls.Add(1)
		return errors.New("boom")
	}, events.Backoff(5*time.Millisecond), events.MaxDeliver(2))
	pub(t, c, ch, "k", "doomed", events.Header("X-Trace", "t1"))
	dead := h.DeadLetters(t, b, ch.Stream, 1)
	m := dead[0]
	if m.Subject != events.DLQSubject(ch.Stream) {
		t.Errorf("subject = %q", m.Subject)
	}
	if m.Headers[events.HeaderError] != "boom" || m.Headers["X-Trace"] != "t1" || m.Type() != "google.protobuf.StringValue" {
		t.Errorf("headers = %v", m.Headers)
	}
	if decode(m) != "doomed" {
		t.Errorf("body = %q", decode(m))
	}
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Errorf("handler calls = %d, want 2", n)
	}
}

func dedup(t *testing.T, h Harness) {
	b := h.New(t)
	c := events.NewCore(b)
	ch := Channel(t)
	var g got
	sub(t, h, c, ch, "u", func(ctx context.Context, segs []string, m *events.Message) error {
		g.add(decode(m) + "/" + m.ID)
		return nil
	})
	pub(t, c, ch, "k", "first", events.MsgID("same"))
	pub(t, c, ch, "k", "second", events.MsgID("same"))
	pub(t, c, ch, "k", "third")
	waitFor(t, "two messages", func() bool { return g.len() >= 2 })
	time.Sleep(50 * time.Millisecond)
	if g.len() != 2 || g.msgs[0] != "first/same" {
		t.Fatalf("got %v", g.msgs)
	}
}

func deliverAll(t *testing.T, h Harness) {
	b := h.New(t)
	c := events.NewCore(b)
	ch := Channel(t)
	pub(t, c, ch, "k", "old")
	var fresh, all got
	sub(t, h, c, ch, "new", func(ctx context.Context, segs []string, m *events.Message) error {
		fresh.add(decode(m))
		return nil
	})
	sub(t, h, c, ch, "all", func(ctx context.Context, segs []string, m *events.Message) error {
		all.add(decode(m))
		return nil
	}, events.DeliverAll())
	pub(t, c, ch, "k", "new")
	waitFor(t, "deliveries", func() bool { return fresh.len() >= 1 && all.len() >= 2 })
	time.Sleep(50 * time.Millisecond)
	if fmt.Sprint(fresh.msgs) != "[new]" || fmt.Sprint(all.msgs) != "[old new]" {
		t.Fatalf("new group got %v, DeliverAll group got %v", fresh.msgs, all.msgs)
	}
}
