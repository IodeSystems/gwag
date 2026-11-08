// Command go-jetstream publishes and handles the sample order events
// over NATS JetStream with the generated Go bus.
//
//	go run ./examples/events/go-jetstream             # embedded NATS
//	NATS_URL=nats://localhost:14222 go run ./examples/events/go-jetstream
//
// With NATS_URL unset it starts an in-process NATS server with
// JetStream in a temp dir. Against a gwag cluster, point NATS_URL at a
// gateway's NATS client port (gwag up: 14222).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/iodesystems/gwag/v2/events/jetstream"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
)

func main() {
	nc, stopNATS, err := connect()
	if err != nil {
		log.Fatal(err)
	}
	defer stopNATS()

	backend, err := jetstream.New(nc)
	if err != nil {
		log.Fatal(err)
	}
	defer backend.Close()
	bus := ordersv1.NewBus(backend)
	ctx := context.Background()

	var mu sync.Mutex
	var lines []string
	var wg sync.WaitGroup
	got := func(format string, args ...any) error {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
		wg.Done()
		return nil
	}

	// Each On* call is one durable consumer per (channel, group).
	// Replicas of this service calling it with the same group share
	// the messages.
	check(bus.OnOrderEvents(ctx, "billing", ordersv1.OrderEventsHandlers{
		OrderCreated: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCreated) error {
			return got("OrderEvents   %s created by %s", s.OrderId, m.GetCustomer())
		},
		OrderCancelled: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCancelled) error {
			return got("OrderEvents   %s cancelled: %s", s.OrderId, m.GetReason())
		},
	}))
	check(bus.OnPayments(ctx, "ledger", ordersv1.PaymentsHandlers{
		Captured: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentCaptured) error {
			return got("Payments      %s/%s captured %d for %s", s.Region, s.P1, m.GetAmountCents(), m.GetOrderId())
		},
		Refunded: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentRefunded) error {
			return got("Payments      %s/%s refunded %d: %s", s.Region, s.P1, m.GetAmountCents(), m.GetReason())
		},
	}))
	check(bus.OnOrderShipped(ctx, "notify", func(ctx context.Context, s ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
		return got("OrderShipped  %s via %s", s.OrderId, m.GetCarrier())
	}))
	check(bus.OnAudit(ctx, "archive", func(ctx context.Context, s ordersv1.AuditSubject, m *ordersv1.AuditEntry) error {
		return got("Audit         %s: %s %s", s.Rest, m.GetActor(), m.GetAction())
	}))

	// What a service passes to controlclient's Service.Subscribes.
	for _, s := range bus.Subscriptions() {
		fmt.Printf("subscribes    %s as %s\n", s.Channel, s.Group)
	}

	wg.Add(6)
	id := ordersv1.OrderEventsSubject{OrderId: "o-1001"}
	must(bus.PublishOrderEvents(ctx, id, &ordersv1.OrderCreated{OrderId: "o-1001", Customer: "ann"}))
	must(bus.PublishPayments(ctx, ordersv1.PaymentsSubject{Region: "eu", P1: "card"},
		&ordersv1.Payment{Kind: &ordersv1.Payment_Captured{Captured: &ordersv1.PaymentCaptured{OrderId: "o-1001", AmountCents: 4200}}}))
	must(bus.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: "o-1001"}, &ordersv1.OrderShipped{OrderId: "o-1001", Carrier: "ups"}))
	must(bus.PublishOrderEvents(ctx, ordersv1.OrderEventsSubject{OrderId: "o-1002"}, &ordersv1.OrderCancelled{OrderId: "o-1002", Reason: "out of stock"}))
	must(bus.PublishPayments(ctx, ordersv1.PaymentsSubject{Region: "eu", P1: "card"},
		&ordersv1.Payment{Kind: &ordersv1.Payment_Refunded{Refunded: &ordersv1.PaymentRefunded{OrderId: "o-1002", AmountCents: 1500, Reason: "cancelled"}}}))
	must(bus.PublishAudit(ctx, ordersv1.AuditSubject{Rest: "o-1002.refund"}, &ordersv1.AuditEntry{Actor: "support", Action: "refund"}))

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		log.Fatal("timed out waiting for deliveries")
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
}

func connect() (*nats.Conn, func(), error) {
	if url := os.Getenv("NATS_URL"); url != "" {
		nc, err := nats.Connect(url)
		if err != nil {
			return nil, nil, err
		}
		return nc, nc.Close, nil
	}
	dir, err := os.MkdirTemp("", "gwag-events-*")
	if err != nil {
		return nil, nil, err
	}
	srv, err := natsd.NewServer(&natsd.Options{Host: "127.0.0.1", Port: natsd.RANDOM_PORT, JetStream: true, StoreDir: dir, NoSigs: true, NoLog: true})
	if err != nil {
		return nil, nil, err
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		return nil, nil, fmt.Errorf("nats not ready")
	}
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		return nil, nil, err
	}
	return nc, func() { nc.Close(); srv.Shutdown(); os.RemoveAll(dir) }, nil
}

func check(_ func(), err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
