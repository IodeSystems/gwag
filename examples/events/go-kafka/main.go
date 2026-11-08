// Command go-kafka runs the generated order bus over Kafka: the same
// typed calls as ../go-jetstream, another Backend.
//
//	KAFKA_BROKERS=localhost:9092 go run .
//
// ./test.sh starts apache/kafka in docker and runs this and the
// backend tests.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
	"github.com/iodesystems/gwag/v2/examples/events/go-kafka/kafka"
)

func main() {
	brokers := os.Getenv("KAFKA_BROKERS")
	if brokers == "" {
		log.Fatal("set KAFKA_BROKERS (e.g. localhost:9092)")
	}
	backend, err := kafka.New(strings.Split(brokers, ","))
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
	}))
	check(bus.OnOrderShipped(ctx, "notify", func(ctx context.Context, s ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
		return got("OrderShipped  %s via %s", s.OrderId, m.GetCarrier())
	}))
	// A new group starts at new messages; give the consumers a moment
	// to resolve their start offsets.
	time.Sleep(time.Second)

	wg.Add(4)
	must(bus.PublishOrderEvents(ctx, ordersv1.OrderEventsSubject{OrderId: "o-1001"}, &ordersv1.OrderCreated{OrderId: "o-1001", Customer: "ann"}))
	must(bus.PublishOrderEvents(ctx, ordersv1.OrderEventsSubject{OrderId: "o-1002"}, &ordersv1.OrderCancelled{OrderId: "o-1002", Reason: "out of stock"}))
	must(bus.PublishPayments(ctx, ordersv1.PaymentsSubject{Region: "eu", P1: "card"},
		&ordersv1.Payment{Kind: &ordersv1.Payment_Captured{Captured: &ordersv1.PaymentCaptured{OrderId: "o-1001", AmountCents: 4200}}}))
	must(bus.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: "o-1001"}, &ordersv1.OrderShipped{OrderId: "o-1001", Carrier: "ups"}))

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		log.Fatal("timed out waiting for deliveries")
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
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
