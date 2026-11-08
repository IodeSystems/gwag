// Tests of the generated Go API (examples/events/gen) over the memory
// backend.
package events_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/memory"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
)

type log struct {
	mu  sync.Mutex
	got []string
	n   chan struct{}
}

func newLog() *log { return &log{n: make(chan struct{}, 64)} }

func (l *log) add(s string) {
	l.mu.Lock()
	l.got = append(l.got, s)
	l.mu.Unlock()
	l.n <- struct{}{}
}

func (l *log) wait(t *testing.T, n int) []string {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-l.n:
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d of %d", i, n)
		}
	}
	time.Sleep(20 * time.Millisecond)
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.got...)
}

func newBus(t *testing.T, opts ...events.Option) (*ordersv1.Bus, *memory.Backend) {
	b := memory.New()
	t.Cleanup(func() { _ = b.Close() })
	return ordersv1.NewBus(b, opts...), b
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestPlainChannel(t *testing.T) {
	bus, _ := newBus(t)
	ctx := context.Background()
	l := newLog()
	_, err := bus.OnOrderShipped(ctx, "notify", func(ctx context.Context, s ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
		msg := events.MessageFrom(ctx)
		l.add(s.OrderId + " " + m.GetCarrier() + " " + msg.Subject + " " + msg.Type())
		return nil
	})
	must(t, err)
	must(t, bus.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: "o1"}, &ordersv1.OrderShipped{OrderId: "o1", Carrier: "ups"}))
	got := l.wait(t, 1)
	if got[0] != "o1 ups events.orders.o1.shipped orders.v1.OrderShipped" {
		t.Fatalf("got %q", got[0])
	}
	if err := bus.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: "a.b"}, &ordersv1.OrderShipped{}); err == nil {
		t.Fatal("segment with a dot: no error")
	}
}

func TestOneofChannel(t *testing.T) {
	bus, _ := newBus(t, events.WithLogf(func(string, ...any) {}))
	ctx := context.Background()
	l := newLog()
	_, err := bus.OnPayments(ctx, "ledger", ordersv1.PaymentsHandlers{
		Captured: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentCaptured) error {
			l.add("captured " + s.Region + "/" + s.P1 + " " + m.GetOrderId())
			return nil
		},
		Note: func(ctx context.Context, s ordersv1.PaymentsSubject, note string) error {
			l.add("note " + note)
			return nil
		},
		// Refunded is nil: acked unhandled.
		Unknown: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.Payment) error {
			l.add("unknown")
			return nil
		},
	})
	must(t, err)
	s := ordersv1.PaymentsSubject{Region: "eu", P1: "card"}
	must(t, bus.PublishPayments(ctx, s, &ordersv1.Payment{Kind: &ordersv1.Payment_Captured{Captured: &ordersv1.PaymentCaptured{OrderId: "o1"}}}))
	must(t, bus.PublishPayments(ctx, s, &ordersv1.Payment{Kind: &ordersv1.Payment_Refunded{Refunded: &ordersv1.PaymentRefunded{OrderId: "o1"}}}))
	must(t, bus.PublishPayments(ctx, s, &ordersv1.Payment{Kind: &ordersv1.Payment_Note{Note: "hi"}}))
	must(t, bus.PublishPayments(ctx, s, &ordersv1.Payment{}))
	got := l.wait(t, 3)
	want := []string{"captured eu/card o1", "note hi", "unknown"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestUnionChannel(t *testing.T) {
	var logged []string
	var mu sync.Mutex
	bus, _ := newBus(t, events.WithLogf(func(f string, a ...any) { mu.Lock(); logged = append(logged, f); mu.Unlock() }))
	ctx := context.Background()
	l := newLog()
	_, err := bus.OnOrderEvents(ctx, "billing", ordersv1.OrderEventsHandlers{
		OrderCreated: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCreated) error {
			l.add("created " + s.OrderId + " " + m.GetCustomer())
			return nil
		},
		OrderCancelled: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCancelled) error {
			l.add("cancelled " + s.OrderId + " " + m.GetReason())
			return nil
		},
	})
	must(t, err)
	s := ordersv1.OrderEventsSubject{OrderId: "o7"}
	must(t, bus.PublishOrderEvents(ctx, s, &ordersv1.OrderCreated{OrderId: "o7", Customer: "ann"}))
	must(t, bus.PublishOrderEvents(ctx, s, &ordersv1.OrderCancelled{OrderId: "o7", Reason: "late"}))
	// A type this code does not know: acked, logged once.
	ch := events.MustChannel("orders.v1.OrderEvents", "events.orders.{orderId}.lifecycle")
	for i := 0; i < 2; i++ {
		must(t, bus.Core().Publish(ctx, ch, []string{"o7"}, &ordersv1.AuditEntry{Actor: "x"}))
	}
	must(t, bus.PublishOrderEvents(ctx, s, &ordersv1.OrderCreated{OrderId: "o7", Customer: "bob"}))
	got := l.wait(t, 3)
	if len(got) != 3 || got[0] != "created o7 ann" || got[1] != "cancelled o7 late" || got[2] != "created o7 bob" {
		t.Fatalf("got %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 1 {
		t.Fatalf("unknown type logged %d times, want once", len(logged))
	}
}

func TestUnionUnknownHandler(t *testing.T) {
	bus, _ := newBus(t)
	ctx := context.Background()
	l := newLog()
	_, err := bus.OnOrderEvents(ctx, "audit", ordersv1.OrderEventsHandlers{
		Unknown: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *events.Message) error {
			var a ordersv1.AuditEntry
			if err := proto.Unmarshal(m.Data, &a); err != nil {
				return err
			}
			l.add(m.Type() + " " + a.GetActor())
			return nil
		},
	})
	must(t, err)
	ch := events.MustChannel("orders.v1.OrderEvents", "events.orders.{orderId}.lifecycle")
	must(t, bus.Core().Publish(ctx, ch, []string{"o1"}, &ordersv1.AuditEntry{Actor: "eve"}))
	if got := l.wait(t, 1); got[0] != "orders.v1.AuditEntry eve" {
		t.Fatalf("got %q", got)
	}
}

func TestRestSegmentAndSubscriptions(t *testing.T) {
	bus, _ := newBus(t)
	ctx := context.Background()
	l := newLog()
	_, err := bus.OnAudit(ctx, "archive", func(ctx context.Context, s ordersv1.AuditSubject, m *ordersv1.AuditEntry) error {
		l.add(s.Rest + " " + m.GetAction())
		return nil
	})
	must(t, err)
	_, err = bus.OnOrderShipped(ctx, "notify", func(context.Context, ordersv1.OrderShippedSubject, *ordersv1.OrderShipped) error { return nil })
	must(t, err)
	must(t, bus.PublishAudit(ctx, ordersv1.AuditSubject{Rest: "o1.refund"}, &ordersv1.AuditEntry{Action: "refund"}))
	if got := l.wait(t, 1); got[0] != "o1.refund refund" {
		t.Fatalf("got %q", got)
	}
	subs := bus.Subscriptions()
	want := []events.Subscription{{Channel: "orders.v1.Audit", Group: "archive"}, {Channel: "orders.v1.OrderShipped", Group: "notify"}}
	if len(subs) != 2 || subs[0] != want[0] || subs[1] != want[1] {
		t.Fatalf("Subscriptions() = %v", subs)
	}
}

// A failing handler is redelivered, then dead-lettered.
func TestHandlerErrorDeadLetters(t *testing.T) {
	bus, mem := newBus(t)
	ctx := context.Background()
	calls := 0
	var mu sync.Mutex
	_, err := bus.OnOrderShipped(ctx, "flaky", func(context.Context, ordersv1.OrderShippedSubject, *ordersv1.OrderShipped) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return context.DeadlineExceeded
	}, events.MaxDeliver(3), events.Backoff(time.Millisecond))
	must(t, err)
	must(t, bus.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: "o1"}, &ordersv1.OrderShipped{}))
	deadline := time.Now().Add(5 * time.Second)
	for len(mem.DeadLetters()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no dead letter")
		}
		time.Sleep(5 * time.Millisecond)
	}
	d := mem.DeadLetters()[0]
	if d.Subject != "gwag.dlq.EV_orders_v1_OrderShipped" || d.Headers[events.HeaderError] == "" {
		t.Fatalf("dead letter = %+v", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}
