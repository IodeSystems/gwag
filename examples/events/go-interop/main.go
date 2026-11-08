// Command go-interop is the Go side of the Go / TypeScript interop
// check (examples/events/interop.sh). It prints the same lines as
// examples/events/ts/src/interop.ts.
//
//	go-interop listen           # group "interop": print the seven sample events, then exit
//	go-interop publish          # publish the seven sample events
//	go-interop listen-shared    # group "shared": print one "shared <id>" line per delivery until SIGTERM
//	go-interop publish-many N   # publish N OrderShipped events
//
// NATS_URL defaults to nats://127.0.0.1:4222.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/iodesystems/gwag/v2/events/jetstream"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: go-interop listen | publish | listen-shared | publish-many N")
	}
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = nats.DefaultURL
	}
	nc, err := nats.Connect(url)
	if err != nil {
		log.Fatal(err)
	}
	defer nc.Close()
	// Prefetch 1 spreads a group's work evenly across subscribers.
	backend, err := jetstream.New(nc, jetstream.WithPrefetch(1))
	if err != nil {
		log.Fatal(err)
	}
	defer backend.Close()
	bus := ordersv1.NewBus(backend)
	ctx := context.Background()

	switch os.Args[1] {
	case "listen":
		var mu sync.Mutex
		var lines []string
		got := func(format string, args ...any) error {
			mu.Lock()
			lines = append(lines, fmt.Sprintf(format, args...))
			mu.Unlock()
			return nil
		}
		check(bus.OnOrderEvents(ctx, "interop", ordersv1.OrderEventsHandlers{
			OrderCreated: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCreated) error {
				return got("OrderEvents %s created by %s", s.OrderId, m.GetCustomer())
			},
			OrderCancelled: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCancelled) error {
				return got("OrderEvents %s cancelled: %s", s.OrderId, m.GetReason())
			},
		}))
		check(bus.OnPayments(ctx, "interop", ordersv1.PaymentsHandlers{
			Captured: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentCaptured) error {
				return got("Payments %s/%s captured %d for %s", s.Region, s.P1, m.GetAmountCents(), m.GetOrderId())
			},
			Refunded: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentRefunded) error {
				return got("Payments %s/%s refunded %d: %s", s.Region, s.P1, m.GetAmountCents(), m.GetReason())
			},
			Note: func(ctx context.Context, s ordersv1.PaymentsSubject, note string) error {
				return got("Payments %s/%s note %s", s.Region, s.P1, note)
			},
		}))
		check(bus.OnOrderShipped(ctx, "interop", func(ctx context.Context, s ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
			return got("OrderShipped %s via %s", s.OrderId, m.GetCarrier())
		}))
		check(bus.OnAudit(ctx, "interop", func(ctx context.Context, s ordersv1.AuditSubject, m *ordersv1.AuditEntry) error {
			return got("Audit %s: %s %s", s.Rest, m.GetActor(), m.GetAction())
		}))
		fmt.Println("ready")
		deadline := time.Now().Add(30 * time.Second)
		for {
			mu.Lock()
			n := len(lines)
			mu.Unlock()
			if n >= 7 {
				break
			}
			if time.Now().After(deadline) {
				log.Fatalf("timed out: got %d of 7", n)
			}
			time.Sleep(10 * time.Millisecond)
		}
		mu.Lock()
		sort.Strings(lines)
		for _, l := range lines {
			fmt.Println(l)
		}
		mu.Unlock()

	case "publish":
		check0(bus.PublishOrderEvents(ctx, ordersv1.OrderEventsSubject{OrderId: "o-1001"}, &ordersv1.OrderCreated{OrderId: "o-1001", Customer: "ann"}))
		check0(bus.PublishOrderEvents(ctx, ordersv1.OrderEventsSubject{OrderId: "o-1002"}, &ordersv1.OrderCancelled{OrderId: "o-1002", Reason: "out of stock"}))
		check0(bus.PublishPayments(ctx, ordersv1.PaymentsSubject{Region: "eu", P1: "card"},
			&ordersv1.Payment{Kind: &ordersv1.Payment_Captured{Captured: &ordersv1.PaymentCaptured{OrderId: "o-1001", AmountCents: 4200}}}))
		check0(bus.PublishPayments(ctx, ordersv1.PaymentsSubject{Region: "eu", P1: "card"},
			&ordersv1.Payment{Kind: &ordersv1.Payment_Refunded{Refunded: &ordersv1.PaymentRefunded{OrderId: "o-1002", AmountCents: 1500, Reason: "cancelled"}}}))
		check0(bus.PublishPayments(ctx, ordersv1.PaymentsSubject{Region: "us", P1: "wire"},
			&ordersv1.Payment{Kind: &ordersv1.Payment_Note{Note: "hello"}}))
		check0(bus.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: "o-1001"}, &ordersv1.OrderShipped{OrderId: "o-1001", Carrier: "ups"}))
		check0(bus.PublishAudit(ctx, ordersv1.AuditSubject{Rest: "o-1002.refund"}, &ordersv1.AuditEntry{Actor: "support", Action: "refund"}))

	case "listen-shared":
		check(bus.OnOrderShipped(ctx, "shared", func(ctx context.Context, s ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
			time.Sleep(5 * time.Millisecond)
			fmt.Printf("shared %s\n", m.GetOrderId())
			return nil
		}))
		fmt.Println("ready")
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig

	case "publish-many":
		n, err := strconv.Atoi(os.Args[2])
		if err != nil {
			log.Fatal(err)
		}
		for i := 0; i < n; i++ {
			check0(bus.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: fmt.Sprintf("k%d", i)},
				&ordersv1.OrderShipped{OrderId: fmt.Sprintf("go-%d", i), Carrier: "ups"}))
		}

	default:
		log.Fatalf("unknown mode %q", os.Args[1])
	}
}

func check(_ func(), err error) { check0(err) }

func check0(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
