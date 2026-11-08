package jetstream_test

import (
	"context"
	"testing"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/eventstest"
	"github.com/iodesystems/gwag/v2/events/jetstream"
)

func startNATS(t *testing.T) *nats.Conn {
	t.Helper()
	srv, err := natsd.NewServer(&natsd.Options{
		Host:      "127.0.0.1",
		Port:      natsd.RANDOM_PORT,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoSigs:    true,
		NoLog:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// backend keeps a core-NATS listener on the dead-letter subjects.
type backend struct {
	*jetstream.Backend
	dlq chan *nats.Msg
}

func TestBackend(t *testing.T) {
	eventstest.Run(t, eventstest.Harness{
		New: func(t *testing.T) events.Backend {
			nc := startNATS(t)
			b, err := jetstream.New(nc, jetstream.WithPrefetch(1))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = b.Close() })
			dlq := make(chan *nats.Msg, 16)
			if _, err := nc.ChanSubscribe(events.DLQPrefix+">", dlq); err != nil {
				t.Fatal(err)
			}
			return backend{b, dlq}
		},
		DeadLetters: func(t *testing.T, b events.Backend, stream string, n int) []*events.Message {
			var out []*events.Message
			for len(out) < n {
				select {
				case m := <-b.(backend).dlq:
					em := &events.Message{Subject: m.Subject, Headers: map[string]string{}, Data: m.Data}
					for k, v := range m.Header {
						em.Headers[k] = v[0]
					}
					if m.Subject == events.DLQSubject(stream) {
						out = append(out, em)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("got %d dead letters, want %d", len(out), n)
				}
			}
			return out
		},
		Settle: 20 * time.Millisecond,
	})
}

// The backend creates a missing stream with its defaults and leaves an
// existing stream's config alone.
func TestStreamCreatedOnlyIfMissing(t *testing.T) {
	nc := startNATS(t)
	js, err := natsjs.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	c := events.NewCore(b)

	existing := events.MustChannel("orders.v1.OrderEvents", "events.orders.{orderId}")
	pre := jetstream.StreamConfig(existing)
	pre.MaxAge = time.Hour
	if _, err := js.CreateStream(ctx, pre); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(ctx, existing, []string{"o1"}, wrapperspb.String("x")); err != nil {
		t.Fatal(err)
	}
	s, err := js.Stream(ctx, existing.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CachedInfo().Config.MaxAge; got != time.Hour {
		t.Fatalf("existing stream changed: MaxAge = %v", got)
	}

	fresh := events.MustChannel("orders.v1.Other", "events.other.*")
	if err := c.Publish(ctx, fresh, []string{"x"}, wrapperspb.String("x")); err != nil {
		t.Fatal(err)
	}
	s, err = js.Stream(ctx, fresh.Stream)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.CachedInfo().Config
	if cfg.MaxAge != 7*24*time.Hour || cfg.Duplicates != 2*time.Minute || cfg.Subjects[0] != "events.other.*" ||
		cfg.Storage != natsjs.FileStorage || cfg.Retention != natsjs.LimitsPolicy || cfg.Replicas != 1 {
		t.Fatalf("created config = %+v", cfg)
	}
}

// A group is one durable consumer: a second subscriber in the group
// after a restart resumes where the group left off.
func TestGroupIsDurable(t *testing.T) {
	nc := startNATS(t)
	b, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	c := events.NewCore(b)
	ctx := context.Background()
	ch := eventstest.Channel(t)
	got := make(chan string, 8)
	stop, err := c.Subscribe(ctx, ch, "g", func(ctx context.Context, segs []string, m *events.Message) error {
		got <- string(m.Data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	time.Sleep(50 * time.Millisecond)
	if err := c.Publish(ctx, ch, []string{"k"}, wrapperspb.String("while away")); err != nil {
		t.Fatal(err)
	}
	stop, err = c.Subscribe(ctx, ch, "g", func(ctx context.Context, segs []string, m *events.Message) error {
		got <- "back"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	select {
	case s := <-got:
		if s != "back" {
			t.Fatalf("got %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message published while no subscriber was running was lost")
	}
}
