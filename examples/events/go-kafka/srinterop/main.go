// Command srinterop publishes and reads the order channels over Kafka
// with Schema Registry framing, for ../test.sh to check against
// Confluent's own serializer and deserializer.
//
//	srinterop produce SHIPPED_TOPIC EVENTS_TOPIC
//	srinterop consume SHIPPED_TOPIC EVENTS_TOPIC N
//
// KAFKA_BROKERS and SR_URL name the broker and the registry. produce
// registers orders.proto under <topic>-value on first publish (own).
// consume reads from the start of both topics and prints N lines; the
// records need not carry gwag headers.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/kafka"
	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
)

func main() {
	if len(os.Args) < 4 {
		log.Fatal("usage: srinterop produce|consume SHIPPED_TOPIC EVENTS_TOPIC [N]")
	}
	shipped, evs := os.Args[2], os.Args[3]
	backend, err := kafka.New(strings.Split(os.Getenv("KAFKA_BROKERS"), ","), kafka.WithPartitions(1),
		kafka.WithTopics(kafka.TopicTable(map[string]kafka.Topic{
			// A producer that is not gwag keys records by order id and
			// sends no Gwag-* headers; Subject rebuilds the subject.
			"orders.v1.OrderShipped": {Name: shipped, Subject: func(r *kgo.Record) string {
				return "events.orders." + string(r.Key) + ".shipped"
			}},
			"orders.v1.OrderEvents": {Name: evs, Subject: func(r *kgo.Record) string {
				return "events.orders." + string(r.Key) + ".lifecycle"
			}},
		})))
	if err != nil {
		log.Fatal(err)
	}
	defer backend.Close()
	client, err := sr.NewClient(sr.Config{URL: os.Getenv("SR_URL")})
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	register, err := sr.NewSerde(client, sr.AutoRegister())
	must(err)
	lookup, err := sr.NewSerde(client)
	must(err)
	switch os.Args[1] {
	case "produce":
		bus := ordersv1.NewBus(backend, events.WithSerde(register))
		must(bus.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: "o-1001"}, &ordersv1.OrderShipped{OrderId: "o-1001", Carrier: "ups"}))
		must(bus.PublishOrderEvents(ctx, ordersv1.OrderEventsSubject{OrderId: "o-1001"}, &ordersv1.OrderCreated{OrderId: "o-1001", Customer: "ann"}))
		fmt.Println("produced 2")
	case "consume":
		n, _ := strconv.Atoi(os.Args[4])
		consume(ctx, ordersv1.NewBus(backend, events.WithSerde(lookup)), n)
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func consume(ctx context.Context, bus *ordersv1.Bus, n int) {
	var mu sync.Mutex
	var lines []string
	var wg sync.WaitGroup
	wg.Add(n)
	got := func(format string, args ...any) error {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
		wg.Done()
		return nil
	}
	_, err := bus.OnOrderShipped(ctx, "srinterop", func(ctx context.Context, s ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
		raw := events.MessageFrom(ctx)
		return got("OrderShipped %s via %s (key %s, schema id %s, gwag headers %v)", m.GetOrderId(), m.GetCarrier(), raw.Key, raw.Headers[sr.HeaderSchemaID], raw.Headers[events.HeaderChannel] != "")
	}, events.DeliverAll())
	must(err)
	_, err = bus.OnOrderEvents(ctx, "srinterop", ordersv1.OrderEventsHandlers{
		OrderCreated: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCreated) error {
			raw := events.MessageFrom(ctx)
			return got("OrderCreated %s by %s (type from %s)", m.GetOrderId(), m.GetCustomer(), typeSource(raw))
		},
		OrderCancelled: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCancelled) error {
			raw := events.MessageFrom(ctx)
			return got("OrderCancelled %s: %s (type from %s)", m.GetOrderId(), m.GetReason(), typeSource(raw))
		},
		Unknown: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *events.Message) error {
			return got("Unknown %s", m.Type())
		},
	}, events.DeliverAll())
	must(err)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		log.Fatalf("timed out; got %q", lines)
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
}

func typeSource(m *events.Message) string {
	if m.Headers[events.HeaderChannel] != "" {
		return "Gwag-Type header"
	}
	return "schema id " + m.Headers[sr.HeaderSchemaID]
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
