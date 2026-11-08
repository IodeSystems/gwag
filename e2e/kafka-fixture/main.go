// kafka-fixture: the Go services of the Kafka e2e story
// (e2e/s12_kafka_events_test.go). Its own module, so the root module
// (and the e2e package) carries no franz-go dependency; the harness
// builds it from this directory.
//
//	kafka-fixture orders  --brokers B --sr URL --gateway CP [--gateway-alt CP]... --instance ID
//	                      --proto-v1 F --proto-v2 F --options F --control ADDR
//	kafka-fixture billing --brokers B --sr URL --gateway CP [--gateway-alt CP]... --instance ID --group G
//	kafka-fixture retry-probe --brokers B --sr URL --group G
//
// orders registers orders/v1 and orders/v2 (it declares the channels)
// and publishes over Kafka with the Schema Registry serde
// (AutoRegister). Its control listener takes
//
//	POST /publish?prefix=P&n=N[&interval=D]   publish events P-0..P-(N-1) in the background
//	GET  /acked?prefix=P                      {"acked":{id:line},"done":bool,"errors":n}
//
// billing listens on every channel as group G, registers billing/v1
// with Subscribes, and prints one line per delivery:
//
//	DELIVER <msg id> <delivery> | <line>
//
// retry-probe listens on OrderShipped as group G with RetryTopic,
// MaxDeliver 3; its handler fails for order ids "dlq-*". It prints
//
//	ATTEMPT <msg id> <delivery>
//	RETRY <msg id> attempt=<Gwag-Attempt>                  (records on the group's retry topic)
//	DLQ <msg id> key=<key> type=<Gwag-Type> channel=<Gwag-Channel> attempt=<Gwag-Attempt> error=<Gwag-Error> body=<order id>
//
// Status lines: SUBSCRIBED, REGISTERED, READY, LISTENING.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/kafka"
	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, " ") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

var out sync.Mutex

// say prints one line to stdout; lines from concurrent handlers do not
// interleave.
func say(format string, a ...any) {
	out.Lock()
	defer out.Unlock()
	fmt.Printf(format+"\n", a...)
}

func main() {
	log.SetFlags(log.Lmicroseconds)
	if len(os.Args) < 2 {
		log.Fatal("usage: kafka-fixture orders|billing|retry-probe [flags]")
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	brokers := fs.String("brokers", "", "Kafka brokers, comma separated")
	srURL := fs.String("sr", "", "Schema Registry URL")
	gateway := fs.String("gateway", "", "control plane address")
	var alts listFlag
	fs.Var(&alts, "gateway-alt", "another control plane of the cluster (repeatable)")
	instance := fs.String("instance", "", "instance id")
	group := fs.String("group", "", "consumer group")
	protoV1 := fs.String("proto-v1", "", "orders v1 .proto")
	protoV2 := fs.String("proto-v2", "", "orders v2 .proto")
	options := fs.String("options", "", "gw/proto/ps/v1/options.proto")
	control := fs.String("control", "", "control listen address (orders)")
	_ = fs.Parse(os.Args[2:])

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	backend, err := kafka.New(strings.Split(*brokers, ","))
	check(err)
	defer backend.Close()
	client, err := sr.NewClient(sr.Config{URL: *srURL})
	check(err)

	cp := controlclient.Options{
		GatewayAddr:       *gateway,
		GatewayAddrs:      alts,
		InstanceID:        *instance,
		TTL:               6 * time.Second,
		ReadyPollInterval: 200 * time.Millisecond,
		Logger:            log.Printf,
	}
	serde := func(opts ...sr.SerdeOption) events.Option {
		s, err := sr.NewSerde(client, opts...)
		check(err)
		return events.WithSerde(s)
	}
	switch cmd {
	case "orders":
		bus := ordersv1.NewBus(backend, serde(sr.AutoRegister()))
		imports := map[string][]byte{"gw/proto/ps/v1/options.proto": read(*options)}
		cp.Services = []controlclient.Service{
			{Namespace: "orders", Version: "v1", ProtoSource: read(*protoV1), ProtoImports: imports},
			{Namespace: "orders", Version: "v2", ProtoSource: read(*protoV2), ProtoImports: imports},
		}
		runOrders(ctx, bus, cp, *control)
	case "billing":
		bus := ordersv1.NewBus(backend, serde())
		runBilling(ctx, bus, cp, *group)
	case "retry-probe":
		bus := ordersv1.NewBus(backend, serde())
		runRetryProbe(ctx, backend, bus, strings.Split(*brokers, ","), *group)
	default:
		log.Fatalf("unknown command %q", cmd)
	}
}

// ---- orders -------------------------------------------------------------

type batch struct {
	acked  map[string]string
	done   bool
	errors int
}

func runOrders(ctx context.Context, bus *ordersv1.Bus, cp controlclient.Options, control string) {
	cp.ServiceAddr = grpcServer()
	reg := register(ctx, cp)
	var mu sync.Mutex
	batches := map[string]*batch{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /publish", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		prefix := q.Get("prefix")
		n, err := strconv.Atoi(q.Get("n"))
		if prefix == "" || err != nil {
			http.Error(w, "need prefix and n", http.StatusBadRequest)
			return
		}
		var interval time.Duration
		if v := q.Get("interval"); v != "" {
			if interval, err = time.ParseDuration(v); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		b := &batch{acked: map[string]string{}}
		mu.Lock()
		if _, ok := batches[prefix]; ok {
			mu.Unlock()
			http.Error(w, "prefix used", http.StatusConflict)
			return
		}
		batches[prefix] = b
		mu.Unlock()
		go func() {
			for i := 0; i < n; i++ {
				id := fmt.Sprintf("%s-%d", prefix, i)
				line, pub := event(i, prefix)
				// A failed publish is retried with the same id; Kafka
				// does not dedupe, so a retry after a lost ack can
				// deliver twice (the consumer side reports it).
				for {
					pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					err := pub(pctx, bus, events.MsgID(id))
					cancel()
					if err == nil {
						break
					}
					log.Printf("publish %s: %v", id, err)
					mu.Lock()
					b.errors++
					mu.Unlock()
					if ctx.Err() != nil {
						return
					}
					time.Sleep(200 * time.Millisecond) // client retry backoff
				}
				mu.Lock()
				b.acked[id] = line
				mu.Unlock()
				if interval > 0 {
					time.Sleep(interval) // publish pace
				}
			}
			mu.Lock()
			b.done = true
			mu.Unlock()
		}()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /acked", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		b := batches[r.URL.Query().Get("prefix")]
		if b == nil {
			http.Error(w, "no such batch", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"acked": b.acked, "done": b.done, "errors": b.errors})
	})
	lis, err := net.Listen("tcp", control)
	check(err)
	go func() { _ = http.Serve(lis, mux) }()
	say("LISTENING control=%s", lis.Addr())
	<-ctx.Done()
	closeReg(reg)
}

// event returns the i-th sample event: its listener line (the format
// of the examples/events listeners, single-spaced) and a publish func.
// The same seven shapes as e2e/s11_typed_events_test.go s11Event.
func event(i int, prefix string) (string, func(context.Context, *ordersv1.Bus, ...events.PublishOption) error) {
	oid := fmt.Sprintf("%s-%d", prefix, i)
	switch i % 7 {
	case 0:
		return fmt.Sprintf("OrderShipped %s via ups", oid), func(ctx context.Context, b *ordersv1.Bus, o ...events.PublishOption) error {
			return b.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: oid}, &ordersv1.OrderShipped{OrderId: oid, Carrier: "ups"}, o...)
		}
	case 1:
		return fmt.Sprintf("Payments eu/c%s captured %d for %s", oid, i, oid), func(ctx context.Context, b *ordersv1.Bus, o ...events.PublishOption) error {
			return b.PublishPayments(ctx, ordersv1.PaymentsSubject{Region: "eu", P1: "c" + oid},
				&ordersv1.Payment{Kind: &ordersv1.Payment_Captured{Captured: &ordersv1.PaymentCaptured{OrderId: oid, AmountCents: int64(i)}}}, o...)
		}
	case 2:
		return fmt.Sprintf("Payments eu/r%s refunded %d: r%s", oid, i, oid), func(ctx context.Context, b *ordersv1.Bus, o ...events.PublishOption) error {
			return b.PublishPayments(ctx, ordersv1.PaymentsSubject{Region: "eu", P1: "r" + oid},
				&ordersv1.Payment{Kind: &ordersv1.Payment_Refunded{Refunded: &ordersv1.PaymentRefunded{OrderId: oid, AmountCents: int64(i), Reason: "r" + oid}}}, o...)
		}
	case 3:
		return fmt.Sprintf("Payments us/n%s note n%s", oid, oid), func(ctx context.Context, b *ordersv1.Bus, o ...events.PublishOption) error {
			return b.PublishPayments(ctx, ordersv1.PaymentsSubject{Region: "us", P1: "n" + oid}, &ordersv1.Payment{Kind: &ordersv1.Payment_Note{Note: "n" + oid}}, o...)
		}
	case 4:
		return fmt.Sprintf("OrderEvents %s created by cust%d", oid, i), func(ctx context.Context, b *ordersv1.Bus, o ...events.PublishOption) error {
			return b.PublishOrderEvents(ctx, ordersv1.OrderEventsSubject{OrderId: oid}, &ordersv1.OrderCreated{OrderId: oid, Customer: fmt.Sprintf("cust%d", i)}, o...)
		}
	case 5:
		return fmt.Sprintf("OrderEvents %s cancelled: why%d", oid, i), func(ctx context.Context, b *ordersv1.Bus, o ...events.PublishOption) error {
			return b.PublishOrderEvents(ctx, ordersv1.OrderEventsSubject{OrderId: oid}, &ordersv1.OrderCancelled{OrderId: oid, Reason: fmt.Sprintf("why%d", i)}, o...)
		}
	default:
		return fmt.Sprintf("Audit %s.x: actor%d act", oid, i), func(ctx context.Context, b *ordersv1.Bus, o ...events.PublishOption) error {
			return b.PublishAudit(ctx, ordersv1.AuditSubject{Rest: oid + ".x"}, &ordersv1.AuditEntry{Actor: fmt.Sprintf("actor%d", i), Action: "act"}, o...)
		}
	}
}

// ---- billing ------------------------------------------------------------

const billingProto = `syntax = "proto3";
package billing.v1;
message Invoice { string id = 1; }
service Billing { rpc Get(Invoice) returns (Invoice); }
`

func runBilling(ctx context.Context, bus *ordersv1.Bus, cp controlclient.Options, group string) {
	got := func(ctx context.Context, format string, a ...any) error {
		id, delivery := "", 0
		if m := events.MessageFrom(ctx); m != nil {
			id, delivery = m.ID, m.Delivery
		}
		say("DELIVER %s %d | %s", id, delivery, fmt.Sprintf(format, a...))
		return nil
	}
	_, err := bus.OnOrderEvents(ctx, group, ordersv1.OrderEventsHandlers{
		OrderCreated: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCreated) error {
			return got(ctx, "OrderEvents %s created by %s", s.OrderId, m.GetCustomer())
		},
		OrderCancelled: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCancelled) error {
			return got(ctx, "OrderEvents %s cancelled: %s", s.OrderId, m.GetReason())
		},
	})
	check(err)
	_, err = bus.OnPayments(ctx, group, ordersv1.PaymentsHandlers{
		Captured: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentCaptured) error {
			return got(ctx, "Payments %s/%s captured %d for %s", s.Region, s.P1, m.GetAmountCents(), m.GetOrderId())
		},
		Refunded: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentRefunded) error {
			return got(ctx, "Payments %s/%s refunded %d: %s", s.Region, s.P1, m.GetAmountCents(), m.GetReason())
		},
		Note: func(ctx context.Context, s ordersv1.PaymentsSubject, note string) error {
			return got(ctx, "Payments %s/%s note %s", s.Region, s.P1, note)
		},
	})
	check(err)
	_, err = bus.OnOrderShipped(ctx, group, func(ctx context.Context, s ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
		return got(ctx, "OrderShipped %s via %s", s.OrderId, m.GetCarrier())
	})
	check(err)
	_, err = bus.OnAudit(ctx, group, func(ctx context.Context, s ordersv1.AuditSubject, m *ordersv1.AuditEntry) error {
		return got(ctx, "Audit %s: %s %s", s.Rest, m.GetActor(), m.GetAction())
	})
	check(err)
	say("SUBSCRIBED")

	// The bus reports what it listens on; the gateway counts each
	// (channel, group) like a dependency on orders.
	var subs []controlclient.Subscription
	for _, s := range bus.Subscriptions() {
		subs = append(subs, controlclient.Subscription{Channel: s.Channel, Group: s.Group})
	}
	cp.ServiceAddr = grpcServer()
	cp.Services = []controlclient.Service{{Namespace: "billing", Version: "v1", ProtoSource: []byte(billingProto), Subscribes: subs}}
	reg := register(ctx, cp)
	<-ctx.Done()
	closeReg(reg)
}

// ---- retry-probe ----------------------------------------------------------

func runRetryProbe(ctx context.Context, backend *kafka.Backend, bus *ordersv1.Bus, brokers []string, group string) {
	_, err := bus.OnOrderShipped(ctx, group, func(ctx context.Context, s ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
		if !strings.HasPrefix(m.GetOrderId(), "dlq-") {
			return nil
		}
		raw := events.MessageFrom(ctx)
		say("ATTEMPT %s %d", raw.ID, raw.Delivery)
		return errors.New("s12: handler always fails")
	}, kafka.RetryTopic(), events.MaxDeliver(3), events.Backoff(200*time.Millisecond))
	check(err)

	// Read the group's retry topic and the channel's DLQ from the start,
	// outside any group. Subscribe created both topics.
	t := backend.TopicOf(events.MustChannel("orders.v1.OrderShipped", "events.orders.{orderId}.shipped"))
	retry, dlq := t.Retry(group), t.DLQ
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(retry, dlq), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	check(err)
	defer cl.Close()
	say("READY retry=%s dlq=%s", retry, dlq)
	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return
		}
		fetches.EachRecord(func(r *kgo.Record) {
			h := map[string]string{}
			for _, x := range r.Headers {
				h[x.Key] = string(x.Value)
			}
			switch r.Topic {
			case retry:
				say("RETRY %s attempt=%s", h[events.HeaderMsgID], h[kafka.HeaderAttempt])
			case dlq:
				body := "undecodable"
				if _, _, b, err := sr.Unframe(r.Value); err == nil {
					var m ordersv1.OrderShipped
					if proto.Unmarshal(b, &m) == nil {
						body = m.GetOrderId()
					}
				}
				say("DLQ %s key=%s type=%s channel=%s attempt=%s error=%s body=%s", h[events.HeaderMsgID], r.Key,
					h[events.HeaderType], h[events.HeaderChannel], h[kafka.HeaderAttempt], h[events.HeaderError], body)
			}
		})
	}
}

// ---- registration -----------------------------------------------------------

type regHolder struct {
	mu  sync.Mutex
	reg *controlclient.Registration
}

// register runs SelfRegister in the background (it blocks until every
// binding is ready: billing waits for orders) and prints REGISTERED
// when it returns.
func register(ctx context.Context, cp controlclient.Options) *regHolder {
	h := &regHolder{}
	go func() {
		r, err := controlclient.SelfRegister(ctx, cp)
		if err != nil {
			if ctx.Err() == nil {
				log.Fatalf("SelfRegister: %v", err)
			}
			return
		}
		h.mu.Lock()
		h.reg = r
		h.mu.Unlock()
		say("REGISTERED %s", cp.InstanceID)
	}()
	return h
}

func closeReg(h *regHolder) {
	h.mu.Lock()
	r := h.reg
	h.mu.Unlock()
	if r != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = r.Close(ctx)
		cancel()
	}
}

// grpcServer is a gRPC server with no services: the address the
// registrations hand to the gateway. Nothing calls it.
func grpcServer() string {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go func() { _ = grpc.NewServer().Serve(lis) }()
	return lis.Addr().String()
}

func read(path string) []byte {
	b, err := os.ReadFile(path)
	check(err)
	return b
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
