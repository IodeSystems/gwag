// Command go-interop is the Go half of the Go/Python interop test
// (examples/events/py/tests/test_interop.py). examples/events/py/interop.py
// is the Python half; the two take the same flags and print the same
// lines.
//
//	go-interop serve                       # embedded NATS with JetStream; prints its URL
//	go-interop listen  -group G [-prefetch N] [-sleep-ms N]
//	go-interop publish [-bulk N] [-prefix P] [-no-canonical]
//
// listen and publish read NATS_URL. listen subscribes to every
// channel, prints READY, then one line per delivery until stdin
// closes, then DONE.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/iodesystems/gwag/v2/events/jetstream"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: go-interop serve | listen | publish")
	}
	switch os.Args[1] {
	case "serve":
		serve()
	case "listen":
		listen(os.Args[2:])
	case "publish":
		publish(os.Args[2:])
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func serve() {
	dir, err := os.MkdirTemp("", "gwag-interop-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	srv, err := natsd.NewServer(&natsd.Options{Host: "127.0.0.1", Port: natsd.RANDOM_PORT, JetStream: true, StoreDir: dir, NoSigs: true, NoLog: true})
	if err != nil {
		log.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		log.Fatal("nats not ready")
	}
	defer srv.Shutdown()
	fmt.Println(srv.ClientURL())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); stop() }()
	<-ctx.Done()
}

// newBus connects to NATS_URL and returns a bus and its cleanup.
func newBus(prefetch int) (*ordersv1.Bus, func()) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		log.Fatal("set NATS_URL")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		log.Fatal(err)
	}
	backend, err := jetstream.New(nc, jetstream.WithPrefetch(prefetch))
	if err != nil {
		log.Fatal(err)
	}
	return ordersv1.NewBus(backend), func() { _ = backend.Close(); nc.Close() }
}

func listen(args []string) {
	fs := flag.NewFlagSet("listen", flag.ExitOnError)
	group := fs.String("group", "", "consumer group")
	sleepMS := fs.Int("sleep-ms", 0, "pause per message")
	prefetch := fs.Int("prefetch", 16, "messages a subscriber pulls ahead")
	_ = fs.Parse(args)
	if *group == "" {
		log.Fatal("-group is required")
	}
	bus, cleanup := newBus(*prefetch)
	defer cleanup()
	ctx := context.Background()
	emit := func(format string, a ...any) error {
		if *sleepMS > 0 {
			time.Sleep(time.Duration(*sleepMS) * time.Millisecond)
		}
		fmt.Printf(format+"\n", a...)
		return nil
	}
	var stops []func()
	add := func(stop func(), err error) {
		if err != nil {
			log.Fatal(err)
		}
		stops = append(stops, stop)
	}
	add(bus.OnOrderEvents(ctx, *group, ordersv1.OrderEventsHandlers{
		OrderCreated: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCreated) error {
			return emit("OrderEvents   %s created by %s", s.OrderId, m.GetCustomer())
		},
		OrderCancelled: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCancelled) error {
			return emit("OrderEvents   %s cancelled: %s", s.OrderId, m.GetReason())
		},
	}))
	add(bus.OnPayments(ctx, *group, ordersv1.PaymentsHandlers{
		Captured: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentCaptured) error {
			return emit("Payments      %s/%s captured %d for %s", s.Region, s.P1, m.GetAmountCents(), m.GetOrderId())
		},
		Refunded: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentRefunded) error {
			return emit("Payments      %s/%s refunded %d: %s", s.Region, s.P1, m.GetAmountCents(), m.GetReason())
		},
		Note: func(ctx context.Context, s ordersv1.PaymentsSubject, text string) error {
			return emit("Payments      %s/%s note %s", s.Region, s.P1, text)
		},
	}))
	add(bus.OnOrderShipped(ctx, *group, func(ctx context.Context, s ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
		return emit("OrderShipped  %s via %s", s.OrderId, m.GetCarrier())
	}))
	add(bus.OnAudit(ctx, *group, func(ctx context.Context, s ordersv1.AuditSubject, m *ordersv1.AuditEntry) error {
		return emit("Audit         %s: %s %s", s.Rest, m.GetActor(), m.GetAction())
	}))
	fmt.Println("READY")
	_, _ = io.Copy(io.Discard, os.Stdin)
	for _, stop := range stops {
		stop()
	}
	fmt.Println("DONE")
}

func publish(args []string) {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	bulk := fs.Int("bulk", 0, "extra OrderShipped messages")
	prefix := fs.String("prefix", "go", "bulk id prefix")
	noCanonical := fs.Bool("no-canonical", false, "skip the seven canonical messages")
	prefetch := fs.Int("prefetch", 16, "messages a subscriber pulls ahead")
	_ = fs.Parse(args)
	bus, cleanup := newBus(*prefetch)
	defer cleanup()
	ctx := context.Background()
	must := func(err error) {
		if err != nil {
			log.Fatal(err)
		}
	}
	if !*noCanonical {
		must(bus.PublishOrderEvents(ctx, ordersv1.OrderEventsSubject{OrderId: "o-1001"}, &ordersv1.OrderCreated{OrderId: "o-1001", Customer: "ann"}))
		must(bus.PublishPayments(ctx, ordersv1.PaymentsSubject{Region: "eu", P1: "card"},
			&ordersv1.Payment{Kind: &ordersv1.Payment_Captured{Captured: &ordersv1.PaymentCaptured{OrderId: "o-1001", AmountCents: 4200}}}))
		must(bus.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: "o-1001"}, &ordersv1.OrderShipped{OrderId: "o-1001", Carrier: "ups"}))
		must(bus.PublishOrderEvents(ctx, ordersv1.OrderEventsSubject{OrderId: "o-1002"}, &ordersv1.OrderCancelled{OrderId: "o-1002", Reason: "out of stock"}))
		must(bus.PublishPayments(ctx, ordersv1.PaymentsSubject{Region: "eu", P1: "card"},
			&ordersv1.Payment{Kind: &ordersv1.Payment_Refunded{Refunded: &ordersv1.PaymentRefunded{OrderId: "o-1002", AmountCents: 1500, Reason: "cancelled"}}}))
		must(bus.PublishAudit(ctx, ordersv1.AuditSubject{Rest: "o-1002.refund"}, &ordersv1.AuditEntry{Actor: "support", Action: "refund"}))
		must(bus.PublishPayments(ctx, ordersv1.PaymentsSubject{Region: "us", P1: "gift"},
			&ordersv1.Payment{Kind: &ordersv1.Payment_Note{Note: "thank you"}}))
	}
	for i := 0; i < *bulk; i++ {
		id := fmt.Sprintf("o-bulk-%s-%d", *prefix, i)
		must(bus.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: id}, &ordersv1.OrderShipped{OrderId: id, Carrier: "ups"}))
	}
	fmt.Println("DONE")
}
