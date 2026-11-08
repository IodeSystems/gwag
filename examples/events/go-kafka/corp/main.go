// Command corp reads topic corp-orders, which another team produces
// with Confluent's serializer (subject corp-orders-value, message
// corp.orders.v1.Order). Two ways, one per row of docs/events-kafka.md
// "Who produces the topic?":
//
//	corp import TOPIC   # typed code from a pinned copy of the subject
//	corp shadow TOPIC   # typed code from gwag's own copy, shadow/shop/v1/order.proto
//
// gen/ is written by ../test.sh, not committed:
//
//	gwag gen --registry $SR_URL --subject corp-orders-value --import-dir imported \
//	  --lang go --out . --opt module=github.com/iodesystems/gwag/v2/examples/events/go-kafka/corp \
//	  --opt Mregistry/corp-orders-value.proto=github.com/iodesystems/gwag/v2/examples/events/go-kafka/corp/gen/corpordersv1;corpordersv1 \
//	  --opt Mcorp/common/v1/money.proto=github.com/iodesystems/gwag/v2/examples/events/go-kafka/corp/gen/corpcommonv1;corpcommonv1
//	gwag gen --lang go --out . -I shadow --opt module=github.com/iodesystems/gwag/v2/examples/events/go-kafka/corp shop/v1/order.proto
//
// KAFKA_BROKERS and SR_URL name the broker and the registry.
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

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/kafka"
	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	"github.com/iodesystems/gwag/v2/examples/events/go-kafka/corp/gen/corpordersv1"
	shopv1 "github.com/iodesystems/gwag/v2/examples/events/go-kafka/corp/gen/shop/v1"
)

const want = 2

func main() {
	if len(os.Args) != 3 {
		log.Fatal("usage: corp import|shadow TOPIC")
	}
	mode, topic := os.Args[1], os.Args[2]
	client, err := sr.NewClient(sr.Config{URL: os.Getenv("SR_URL")})
	if err != nil {
		log.Fatal(err)
	}
	brokers := strings.Split(os.Getenv("KAFKA_BROKERS"), ",")
	ctx := context.Background()

	var mu sync.Mutex
	var lines []string
	var wg sync.WaitGroup
	wg.Add(want)
	got := func(format string, args ...any) error {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
		wg.Done()
		return nil
	}

	switch mode {
	case "import":
		// The channel key is <package>.<message name>; the topic is the
		// team's, so the mapper names it.
		backend, err := kafka.New(brokers, kafka.WithTopics(kafka.TopicTable(map[string]kafka.Topic{
			"corp.orders.v1.Order": {Name: topic},
		})))
		check(err)
		defer backend.Close()
		serde, err := sr.NewSerde(client)
		check(err)
		bus := corpordersv1.NewBus(backend, events.WithSerde(serde))
		_, err = bus.OnOrder(ctx, "corp-import", func(ctx context.Context, _ corpordersv1.OrderSubject, m *corpordersv1.Order) error {
			raw := events.MessageFrom(ctx)
			return got("import %s %s total %d %s status %s skus %v (key %s, schema id %s)", raw.Type(), m.GetOrderId(),
				m.GetTotal().GetCents(), m.GetTotal().GetCurrency(), m.GetStatus(), m.GetSkus(), raw.Key, raw.Headers[sr.HeaderSchemaID])
		}, events.DeliverAll())
		check(err)
	case "shadow":
		backend, err := kafka.New(brokers, kafka.WithTopics(kafka.TopicTable(map[string]kafka.Topic{
			"shop.v1.CorpOrders": {Name: topic},
		})))
		check(err)
		defer backend.Close()
		// Records are framed as corp.orders.v1.Order; the handler
		// takes them as the copy.
		serde, err := sr.NewSerde(client, sr.TypeNames(map[string]string{"corp.orders.v1.Order": "shop.v1.CorpOrder"}))
		check(err)
		bus := shopv1.NewBus(backend, events.WithSerde(serde))
		_, err = bus.OnCorpOrders(ctx, "corp-shadow", func(ctx context.Context, _ shopv1.CorpOrdersSubject, m *shopv1.CorpOrder) error {
			raw := events.MessageFrom(ctx)
			return got("shadow %s %s total %d (key %s)", raw.Type(), m.GetOrderId(), m.GetTotal().GetCents(), raw.Key)
		}, events.DeliverAll())
		check(err)
	default:
		log.Fatalf("unknown mode %q", mode)
	}

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

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
