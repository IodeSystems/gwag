//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/iodesystems/gwag/v2/e2e/harness"
	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/jetstream"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
)

// s11Replicas is the replica count the gateway gives event streams on
// a 3-node cluster: --cluster-replicas defaults to "odd", the largest
// odd number <= min(live peers, 3) (gw/cluster_replicas.go).
const s11Replicas = 3

// s11Channels are the named channels of examples/events orders.proto.
var s11Channels = []string{"OrderShipped", "Payments", "OrderEvents", "Audit"}

// TestS11TypedEvents: typed event channels across a 3-node cluster.
//
// "orders" registers examples/events/proto/orders/v1/orders.proto
// (plain, oneof and union channels) through the control plane on n2,
// with a v2 line next to it. "billing" (two Go replicas, one consumer
// group, in this process) declares Subscribes on n1 before orders
// exists. The story checks the start gate and the dependency graph,
// the gateway's JetStream streams, typed delivery to the Go group and
// to a TypeScript and a Python listener in their own groups, the
// GraphQL ps_sub / ps.pub bridge, dead letters, and delivery across
// the loss of the NATS node the producer and listeners use.
func TestS11TypedEvents(t *testing.T) {
	env := harness.New(t)
	cluster := env.Cluster(3, harness.GatewayOpts{Args: []string{"--subscribe-secret", s8Secret}})
	n1, n2, n3 := cluster.Nodes[0], cluster.Nodes[1], cluster.Nodes[2]
	ctx := harness.Ctx(t, 10*time.Minute)
	// KV at R3 first: the gateway takes the stream replica count from
	// the same peer tracker.
	cluster.WaitStreamsReplicated(t, s11Replicas, 60*time.Second)

	root, err := harness.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	optionsProto := s11Read(t, root, "gw/proto/ps/v1/options.proto")
	ordersV1 := s11Read(t, root, "examples/events/proto/orders/v1/orders.proto")
	ordersV2 := s11OrdersV2(t, ordersV1)
	svcAddr := s11GRPCServer(t)

	// ---- billing: two replicas, group "billing", registered on n1 ----
	rec := newS11Recorder()
	type replica struct {
		id  string
		nc  *nats.Conn
		bus *ordersv1.Bus
		reg chan s11RegResult
	}
	var billing []*replica
	for i := 1; i <= 2; i++ {
		r := &replica{id: fmt.Sprintf("billing-%d", i)}
		r.nc = cluster.NATS(t)
		r.bus = s11Bus(t, r.nc)
		// On* before SelfRegister: the bus reports what it listens on.
		// Short AckWait so a delivery lost in the failover comes back
		// within the story's timeouts.
		if err := s11Listen(ctx, r.bus, "billing", rec.handler(r.id), events.AckWait(10*time.Second)); err != nil {
			t.Fatal(err)
		}
		var subs []controlclient.Subscription
		for _, s := range r.bus.Subscriptions() {
			subs = append(subs, controlclient.Subscription{Channel: s.Channel, Group: s.Group})
		}
		r.reg = s11Register(t, ctx, controlclient.Options{
			GatewayAddr:  n1.CPAddr(),
			GatewayAddrs: []string{n2.CPAddr(), n3.CPAddr()},
			ServiceAddr:  svcAddr,
			InstanceID:   r.id,
			Services: []controlclient.Service{{
				Namespace:   "billing",
				Version:     "v1",
				ProtoSource: []byte(s11BillingProto),
				Subscribes:  subs,
			}},
		})
		billing = append(billing, r)
	}

	// ---- 2. start gate and dependency graph ----------------------------
	if !t.Run("billing waits for the channels' declarer", func(t *testing.T) {
		for _, r := range billing {
			n1.Admin().WaitInstance("billing", "v1", r.id, "waiting", 30*time.Second)
			select {
			case res := <-r.reg:
				t.Fatalf("%s: SelfRegister returned before orders registered: %v", r.id, res.err)
			default:
			}
		}
	}) {
		return
	}

	ordersReg := s11Register(t, ctx, controlclient.Options{
		GatewayAddr:  n2.CPAddr(),
		GatewayAddrs: []string{n3.CPAddr()},
		ServiceAddr:  svcAddr,
		InstanceID:   "orders-1",
		Services: []controlclient.Service{
			{Namespace: "orders", Version: "v1", ProtoSource: ordersV1, ProtoImports: map[string][]byte{"gw/proto/ps/v1/options.proto": optionsProto}},
			{Namespace: "orders", Version: "v2", ProtoSource: ordersV2, ProtoImports: map[string][]byte{"gw/proto/ps/v1/options.proto": optionsProto}},
		},
	})
	if !t.Run("orders registers, billing becomes ready", func(t *testing.T) {
		s11WaitReg(t, ordersReg, "orders-1", 30*time.Second)
		for _, r := range billing {
			s11WaitReg(t, r.reg, r.id, 30*time.Second)
			for _, g := range cluster.Nodes {
				g.Admin().WaitInstance("billing", "v1", r.id, "ready", 30*time.Second)
			}
		}
	}) {
		return
	}

	t.Run("graph has channel edges and orders/v1 is not sunset-able", func(t *testing.T) {
		harness.Eventually(t, 30*time.Second, "n1 graph shows billing's channel edges", func() error {
			gr, err := n1.Admin().Graph(ctx, "")
			if err != nil {
				return err
			}
			got := map[string]bool{}
			for _, e := range gr.DesiredEdges {
				if e.ConsumerNamespace != "billing" {
					continue
				}
				if e.EdgeKind != "channel" || e.Group != "billing" || e.Provider != "orders" ||
					strings.Join(e.Lines, ",") != "v1" || !e.Met || e.InstanceCount != 2 {
					return fmt.Errorf("billing edge %+v", e)
				}
				got[e.Channel] = true
			}
			for _, c := range s11Channels {
				if !got["orders.v1."+c] {
					return fmt.Errorf("no channel edge for orders.v1.%s (have %v)", c, got)
				}
			}
			v1, v2 := gr.Node("orders/v1"), gr.Node("orders/v2")
			if v1 == nil || v2 == nil {
				return errors.New("orders/v1 or orders/v2 missing from the graph")
			}
			if v1.Sunsetable || !strings.Contains(v1.SunsetReason, "billing/v1 subscribes to orders.v1.OrderEvents (group billing)") {
				return fmt.Errorf("orders/v1 sunsetable=%v reason=%q", v1.Sunsetable, v1.SunsetReason)
			}
			if !v2.Sunsetable {
				return fmt.Errorf("orders/v2 has no subscriber, want sunset-able: %q", v2.SunsetReason)
			}
			return nil
		})
	})

	// ---- 1. streams ------------------------------------------------------
	var streams []string
	for _, ver := range []string{"v1", "v2"} {
		for _, c := range s11Channels {
			streams = append(streams, "EV_orders_"+ver+"_"+c)
		}
	}
	t.Run("each named channel has a stream at the cluster replica policy", func(t *testing.T) {
		// billing's bus created the v1 streams at R1 before orders
		// registered; the gateway raises them. 15s: each node's own
		// view of the replica count can lag the KV buckets for 30s or
		// more after the cluster forms; stream replicas must not
		// (TestEventStreams_ReplicasFollowPolicyWhileCurrentRLags).
		cluster.WaitStreamReplicas(t, s11Replicas, 15*time.Second, streams...)
		js := cluster.JetStream(t)
		wantSubject := map[string]string{
			"EV_orders_v1_OrderShipped": "events.orders.*.shipped",
			"EV_orders_v1_Payments":     "events.payments.*.*",
			"EV_orders_v1_OrderEvents":  "events.orders.*.lifecycle",
			"EV_orders_v1_Audit":        "audit.orders.>",
		}
		for name, subj := range wantSubject {
			s, err := js.Stream(ctx, name)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			c := s.CachedInfo().Config
			if len(c.Subjects) != 1 || c.Subjects[0] != subj || c.Duplicates != 2*time.Minute || c.MaxAge != 7*24*time.Hour {
				t.Errorf("%s: subjects %v duplicates %s max age %s", name, c.Subjects, c.Duplicates, c.MaxAge)
			}
			if _, err := s.Consumer(ctx, "billing"); err != nil {
				t.Errorf("%s: consumer billing: %v", name, err)
			}
		}
	})

	// ---- 4 (setup). TypeScript and Python listeners ---------------------
	// Started before the publish: a new group starts at new messages.
	tsL := s11StartTS(t, env, root, n2)
	pyL := s11StartPy(t, env, root, n3)

	// ---- 3. typed delivery to the Go group ------------------------------
	pubNC := cluster.NATS(t)
	producer := s11Bus(t, pubNC)
	const nTyped = 70
	typed := map[string]string{} // msg id -> line
	for i := 0; i < nTyped; i++ {
		id := fmt.Sprintf("typed-%d", i)
		line, pub := s11Event(i, "t")
		if err := pub(ctx, producer, events.MsgID(id)); err != nil {
			t.Fatalf("publish %s: %v", id, err)
		}
		typed[id] = line
	}
	t.Run("the billing group gets each event once, both replicas work", func(t *testing.T) {
		rec.waitIDs(t, typed, 60*time.Second)
		st := rec.stats(typed)
		t.Logf("typed: %d events, %d deliveries, %d duplicated, per replica %v", nTyped, st.deliveries, st.duplicated, st.perReplica)
		if st.duplicated > 0 {
			t.Logf("typed: duplicated ids %v", st.dupIDs)
		}
		for _, r := range billing {
			if st.perReplica[r.id] == 0 {
				t.Errorf("%s got no work: %v", r.id, st.perReplica)
			}
		}
	})

	// ---- 4. cross-language listeners -------------------------------------
	wantLines := map[string]bool{}
	for _, l := range typed {
		wantLines[l] = true
	}
	for _, l := range []*s11Script{tsL, pyL} {
		t.Run(l.lang+" listener gets every event", func(t *testing.T) {
			if l.skip != "" {
				t.Skip(l.skip)
			}
			harness.Eventually(t, 60*time.Second, l.lang+" listener prints every event", func() error {
				got := l.lines()
				var missing []string
				for w := range wantLines {
					if got[w] == 0 {
						missing = append(missing, w)
					}
				}
				if len(missing) > 0 {
					sort.Strings(missing)
					return fmt.Errorf("%d of %d missing, e.g. %q", len(missing), len(wantLines), missing[0])
				}
				return nil
			})
			dup := 0
			for w, n := range l.lines() {
				if wantLines[w] && n > 1 {
					dup++
				}
			}
			t.Logf("%s: %d events, %d duplicated", l.lang, len(wantLines), dup)
		})
	}

	// ---- 5. GraphQL bridge ------------------------------------------------
	t.Run("ps_sub sees typed events as JSON with payloadType", func(t *testing.T) {
		const pattern = "events.orders.*.shipped"
		tok, err := n3.Sign(ctx, pattern, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		sub, err := n3.Subscribe(t, fmt.Sprintf(`subscription { ps_sub(channel: %q, %s) { channel payload payloadType } }`, pattern, tok.Args()))
		if err != nil {
			t.Fatal(err)
		}
		harness.Eventually(t, 10*time.Second, "n3 has one consumer on the pattern", func() error {
			return wantConsumers(ctx, n3, pattern, 1)
		})
		if err := producer.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: "bridge-1"},
			&ordersv1.OrderShipped{OrderId: "bridge-1", Carrier: "dhl"}, events.MsgID("bridge-1")); err != nil {
			t.Fatal(err)
		}
		ev := s11NextPsSub(t, sub, "events.orders.bridge-1.shipped", 15*time.Second)
		if ev.PayloadType != "orders.v1.OrderShipped" {
			t.Errorf("payloadType %q", ev.PayloadType)
		}
		var m ordersv1.OrderShipped
		if err := protojson.Unmarshal([]byte(ev.Payload), &m); err != nil {
			t.Fatalf("payload %q is not OrderShipped JSON: %v", ev.Payload, err)
		}
		if m.GetOrderId() != "bridge-1" || m.GetCarrier() != "dhl" {
			t.Errorf("payload %q", ev.Payload)
		}

		// ps.pub of a typed event on n2 reaches the GraphQL subscriber
		// on n3 as well as the generated listeners (next test).
		ch := "events.orders.bridge-2.shipped"
		ptok, err := n2.Sign(ctx, ch, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := n2.PublishTyped(ctx, ch, `{"orderId":"bridge-2","carrier":"fedex"}`, "orders.v1.OrderShipped", ptok); err != nil {
			t.Fatal(err)
		}
		ev = s11NextPsSub(t, sub, ch, 15*time.Second)
		if ev.PayloadType != "orders.v1.OrderShipped" || !strings.Contains(ev.Payload, "fedex") {
			t.Errorf("ps.pub event on ps_sub: %+v", ev)
		}
	})

	t.Run("ps.pub JSON reaches the typed Go group as proto", func(t *testing.T) {
		ch := "events.orders.bridge-3.lifecycle"
		tok, err := n2.Sign(ctx, ch, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		// A union channel needs payload_type.
		if err := n2.Publish(ctx, ch, `{"orderId":"bridge-3","reason":"fraud"}`, tok); err == nil {
			t.Errorf("ps.pub on a union channel without payload_type: want an error")
		}
		if err := n2.PublishTyped(ctx, ch, `{"orderId":"bridge-3","reason":"fraud"}`, "orders.v1.OrderCancelled", tok); err != nil {
			t.Fatal(err)
		}
		for _, line := range []string{"OrderShipped bridge-2 via fedex", "OrderEvents bridge-3 cancelled: fraud"} {
			harness.Eventually(t, 30*time.Second, "billing receives "+line, func() error {
				if rec.lineCount(line) == 0 {
					return errors.New("not received")
				}
				return nil
			})
		}
	})

	// ---- 7. dead letter ----------------------------------------------------
	t.Run("an always-failing handler dead-letters after MaxDeliver", func(t *testing.T) {
		const stream = "EV_orders_v1_OrderShipped"
		dlqNC := cluster.NATS(t)
		dlq, err := dlqNC.SubscribeSync(events.DLQSubject(stream))
		if err != nil {
			t.Fatal(err)
		}
		if err := dlqNC.Flush(); err != nil {
			t.Fatal(err)
		}
		probe := s11Bus(t, cluster.NATS(t))
		var attempts atomic.Int32
		stop, err := probe.OnOrderShipped(ctx, "dlq-probe", func(ctx context.Context, _ ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
			if m.GetOrderId() == "dlq-1" {
				attempts.Add(1)
			}
			return errors.New("s11: handler always fails")
		}, events.MaxDeliver(3), events.Backoff(50*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		if err := producer.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: "dlq-1"},
			&ordersv1.OrderShipped{OrderId: "dlq-1", Carrier: "none"}, events.MsgID("dlq-1")); err != nil {
			t.Fatal(err)
		}
		msg, err := dlq.NextMsg(30 * time.Second)
		if err != nil {
			t.Fatalf("no dead letter on %s: %v (handler attempts %d)", events.DLQSubject(stream), err, attempts.Load())
		}
		if got := msg.Header.Get(events.HeaderError); got != "s11: handler always fails" {
			t.Errorf("Gwag-Error %q", got)
		}
		if msg.Header.Get(events.HeaderType) != "orders.v1.OrderShipped" || msg.Header.Get(events.HeaderChannel) != "orders.v1.OrderShipped" ||
			msg.Header.Get(events.HeaderNatsMsgID) != "dlq-1" {
			t.Errorf("dead letter headers %v", msg.Header)
		}
		var m ordersv1.OrderShipped
		if err := proto.Unmarshal(msg.Data, &m); err != nil || m.GetOrderId() != "dlq-1" {
			t.Errorf("dead letter body: %v %v", &m, err)
		}
		if n := attempts.Load(); n != 3 {
			t.Errorf("handler attempts %d, want MaxDeliver 3", n)
		}
	})

	// ---- 6. failover ---------------------------------------------------------
	t.Run("killing the clients' NATS node loses no acknowledged event", func(t *testing.T) {
		for name, nc := range map[string]*nats.Conn{"producer": pubNC, "billing-1": billing[0].nc, "billing-2": billing[1].nc} {
			if nc.ConnectedUrl() != n1.NATSURL() {
				t.Fatalf("%s is connected to %s, want n1 %s", name, nc.ConnectedUrl(), n1.NATSURL())
			}
		}
		res := s11FailoverPublish(t, ctx, producer, 60, func(acked int) bool { return acked >= 20 }, func() {
			t.Logf("killing n1 after %d acked publishes", 20)
			n1.Proc.Kill()
		})
		t.Logf("failover: %d acked, %d gave up, %d publish errors retried", len(res.acked), len(res.gaveUp), res.retries)
		if len(res.gaveUp) > 0 {
			t.Errorf("producer gave up on %v", res.gaveUp)
		}
		rec.waitIDs(t, res.acked, 120*time.Second)
		st := rec.stats(res.acked)
		t.Logf("failover: %d deliveries, %d duplicated (%v), per replica %v", st.deliveries, st.duplicated, st.dupIDs, st.perReplica)
		for name, nc := range map[string]*nats.Conn{"producer": pubNC, "billing-1": billing[0].nc, "billing-2": billing[1].nc} {
			t.Logf("%s now on %s (reconnects %d)", name, nc.ConnectedUrl(), nc.Stats().Reconnects)
		}
	})
}

// ---- events ------------------------------------------------------------

// s11Event returns the i-th sample event: its listener line (the
// format of examples/events listeners, single-spaced) and a publish
// func. Seven shapes rotate: plain, the three oneof cases, the two
// union members, and the trailing-wildcard channel.
func s11Event(i int, prefix string) (string, func(context.Context, *ordersv1.Bus, ...events.PublishOption) error) {
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

// s11Listen subscribes bus to every channel as group; each delivery
// calls got with its line.
func s11Listen(ctx context.Context, bus *ordersv1.Bus, group string, got func(ctx context.Context, line string) error, opts ...events.SubscribeOption) error {
	emit := func(ctx context.Context, format string, a ...any) error { return got(ctx, fmt.Sprintf(format, a...)) }
	if _, err := bus.OnOrderEvents(ctx, group, ordersv1.OrderEventsHandlers{
		OrderCreated: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCreated) error {
			return emit(ctx, "OrderEvents %s created by %s", s.OrderId, m.GetCustomer())
		},
		OrderCancelled: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCancelled) error {
			return emit(ctx, "OrderEvents %s cancelled: %s", s.OrderId, m.GetReason())
		},
	}, opts...); err != nil {
		return err
	}
	if _, err := bus.OnPayments(ctx, group, ordersv1.PaymentsHandlers{
		Captured: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentCaptured) error {
			return emit(ctx, "Payments %s/%s captured %d for %s", s.Region, s.P1, m.GetAmountCents(), m.GetOrderId())
		},
		Refunded: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentRefunded) error {
			return emit(ctx, "Payments %s/%s refunded %d: %s", s.Region, s.P1, m.GetAmountCents(), m.GetReason())
		},
		Note: func(ctx context.Context, s ordersv1.PaymentsSubject, note string) error {
			return emit(ctx, "Payments %s/%s note %s", s.Region, s.P1, note)
		},
	}, opts...); err != nil {
		return err
	}
	if _, err := bus.OnOrderShipped(ctx, group, func(ctx context.Context, s ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
		return emit(ctx, "OrderShipped %s via %s", s.OrderId, m.GetCarrier())
	}, opts...); err != nil {
		return err
	}
	_, err := bus.OnAudit(ctx, group, func(ctx context.Context, s ordersv1.AuditSubject, m *ordersv1.AuditEntry) error {
		return emit(ctx, "Audit %s: %s %s", s.Rest, m.GetActor(), m.GetAction())
	}, opts...)
	return err
}

// s11Bus is a generated Bus over the JetStream backend on nc. Prefetch
// 1 spreads a group's work across its members.
func s11Bus(t *testing.T, nc *nats.Conn) *ordersv1.Bus {
	t.Helper()
	b, err := jetstream.New(nc, jetstream.WithPrefetch(1), jetstream.WithLogf(t.Logf))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return ordersv1.NewBus(b)
}

// s11Recorder counts deliveries by message id, line and replica.
type s11Recorder struct {
	mu      sync.Mutex
	ids     map[string]int
	idLine  map[string]string
	idRepl  map[string]map[string]int
	lines   map[string]int
	badLine []string
}

func newS11Recorder() *s11Recorder {
	return &s11Recorder{ids: map[string]int{}, idLine: map[string]string{}, idRepl: map[string]map[string]int{}, lines: map[string]int{}}
}

func (r *s11Recorder) handler(replica string) func(context.Context, string) error {
	return func(ctx context.Context, line string) error {
		id := ""
		if m := events.MessageFrom(ctx); m != nil {
			id = m.ID
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.lines[line]++
		if id == "" {
			return nil
		}
		r.ids[id]++
		if prev, ok := r.idLine[id]; ok && prev != line {
			r.badLine = append(r.badLine, fmt.Sprintf("%s: %q then %q", id, prev, line))
		}
		r.idLine[id] = line
		if r.idRepl[id] == nil {
			r.idRepl[id] = map[string]int{}
		}
		r.idRepl[id][replica]++
		return nil
	}
}

func (r *s11Recorder) lineCount(line string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lines[line]
}

// waitIDs waits until every id in want was delivered, and checks each
// delivery carried the expected line.
func (r *s11Recorder) waitIDs(t *testing.T, want map[string]string, timeout time.Duration) {
	t.Helper()
	harness.Eventually(t, timeout, fmt.Sprintf("billing receives all %d events", len(want)), func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		var missing []string
		for id := range want {
			if r.ids[id] == 0 {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Errorf("%d of %d not delivered: %v", len(missing), len(want), s11Head(missing, 10))
		}
		return nil
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, line := range want {
		if r.idLine[id] != line {
			t.Errorf("%s: delivered as %q, want %q", id, r.idLine[id], line)
		}
	}
	if len(r.badLine) > 0 {
		t.Errorf("one id, two lines: %v", r.badLine)
	}
}

type s11Stats struct {
	deliveries, duplicated int
	dupIDs                 []string
	perReplica             map[string]int
}

func (r *s11Recorder) stats(ids map[string]string) s11Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := s11Stats{perReplica: map[string]int{}}
	for id := range ids {
		n := r.ids[id]
		st.deliveries += n
		if n > 1 {
			st.duplicated++
			st.dupIDs = append(st.dupIDs, fmt.Sprintf("%s x%d", id, n))
		}
		for rep, c := range r.idRepl[id] {
			st.perReplica[rep] += c
		}
	}
	sort.Strings(st.dupIDs)
	return st
}

// ---- failover publisher -----------------------------------------------

type s11FailoverResult struct {
	acked   map[string]string // id -> line
	gaveUp  []string
	retries int
}

// s11FailoverPublish publishes n events "fo-<i>" one at a time. A
// failed publish is retried with the same message id (JetStream
// drops a repeat within the duplicates window) until it is acked or
// 60s pass. fire runs once, after the publish that makes when(acked)
// true.
func s11FailoverPublish(t *testing.T, ctx context.Context, bus *ordersv1.Bus, n int, when func(acked int) bool, fire func()) s11FailoverResult {
	t.Helper()
	res := s11FailoverResult{acked: map[string]string{}}
	fired := false
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("fo-%d", i)
		line, pub := s11Event(i, "fo")
		deadline := time.Now().Add(60 * time.Second)
		for {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := pub(pctx, bus, events.MsgID(id))
			cancel()
			if err == nil {
				res.acked[id] = line
				break
			}
			res.retries++
			if time.Now().After(deadline) || ctx.Err() != nil {
				t.Logf("publish %s: giving up: %v", id, err)
				res.gaveUp = append(res.gaveUp, id)
				break
			}
			// Client retry backoff, not a wait for a condition.
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
			}
		}
		if !fired && when(len(res.acked)) {
			fired = true
			fire()
		}
	}
	return res
}

// ---- service registration ---------------------------------------------

type s11RegResult struct {
	reg *controlclient.Registration
	err error
}

// s11Register runs controlclient.SelfRegister in the background (it
// blocks until every binding is ready) and closes the registration at
// test end.
func s11Register(t *testing.T, ctx context.Context, opts controlclient.Options) chan s11RegResult {
	t.Helper()
	id := opts.InstanceID
	opts.Logger = func(format string, args ...any) { t.Logf(id+": "+format, args...) }
	if opts.TTL == 0 {
		opts.TTL = 6 * time.Second
	}
	if opts.ReadyPollInterval == 0 {
		opts.ReadyPollInterval = 200 * time.Millisecond
	}
	out := make(chan s11RegResult, 1)
	var mu sync.Mutex
	var reg *controlclient.Registration
	t.Cleanup(func() {
		mu.Lock()
		r := reg
		mu.Unlock()
		if r != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = r.Close(cctx)
			cancel()
		}
	})
	go func() {
		r, err := controlclient.SelfRegister(ctx, opts)
		mu.Lock()
		reg = r
		mu.Unlock()
		out <- s11RegResult{r, err}
	}()
	return out
}

func s11WaitReg(t *testing.T, ch chan s11RegResult, id string, timeout time.Duration) {
	t.Helper()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("%s: SelfRegister: %v", id, res.err)
		}
		ch <- res // keep it readable
	case <-time.After(timeout):
		t.Fatalf("%s: SelfRegister still waiting after %s", id, timeout)
	}
}

// s11GRPCServer is a gRPC server with no services: the address the
// in-process registrations hand to the gateway. Nothing calls it.
func s11GRPCServer(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

const s11BillingProto = `syntax = "proto3";
package billing.v1;
message Invoice { string id = 1; }
service Billing { rpc Get(Invoice) returns (Invoice); }
`

func s11Read(t *testing.T, root, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// s11OrdersV2 is orders.proto as package orders.v2 with every pattern
// under "v2.": a second line of orders whose channels do not overlap
// v1's.
func s11OrdersV2(t *testing.T, v1 []byte) []byte {
	t.Helper()
	s := string(v1)
	for _, r := range [][2]string{
		{"package orders.v1;", "package orders.v2;"},
		{`pattern: "events.`, `pattern: "v2.events.`},
		{`pattern: "audit.`, `pattern: "v2.audit.`},
	} {
		if !strings.Contains(s, r[0]) {
			t.Fatalf("orders.proto has no %q", r[0])
		}
		s = strings.ReplaceAll(s, r[0], r[1])
	}
	return []byte(s)
}

// ---- ps_sub -----------------------------------------------------------

type s11PsEvent struct {
	Channel, Payload, PayloadType string
}

// s11NextPsSub reads sub until an event on channel arrives.
func s11NextPsSub(t *testing.T, sub *harness.Subscription, channel string, timeout time.Duration) s11PsEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case data := <-sub.Events:
			var d struct {
				PsSub s11PsEvent `json:"ps_sub"`
			}
			if err := json.Unmarshal(data, &d); err != nil {
				t.Fatalf("ps_sub event %s: %v", data, err)
			}
			if d.PsSub.Channel == channel {
				return d.PsSub
			}
		case <-sub.Done:
			t.Fatalf("ps_sub ended: %v", sub.Err())
		case <-deadline:
			t.Fatalf("no ps_sub event on %s within %s", channel, timeout)
		}
	}
}

// ---- TypeScript and Python listeners ---------------------------------

// s11Script is a listener program in another language, in its own
// consumer group. skip says why it did not start.
type s11Script struct {
	lang string
	proc *harness.Proc
	skip string
}

// lines counts the delivery lines printed so far, whitespace
// normalized to single spaces.
func (s *s11Script) lines() map[string]int {
	out := map[string]int{}
	for _, l := range strings.Split(s.proc.CurrentLogs(), "\n") {
		l = strings.Join(strings.Fields(l), " ")
		for _, p := range []string{"OrderShipped ", "OrderEvents ", "Payments ", "Audit "} {
			if strings.HasPrefix(l, p) {
				out[l]++
			}
		}
	}
	return out
}

// s11Setup runs a setup command in dir; it returns why it failed, or "".
func s11Setup(ctx context.Context, dir, name string, args ...string) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Sprintf("%s %s in %s: %v\n%s", name, strings.Join(args, " "), dir, err, b)
	}
	return ""
}

// s11StartTS starts examples/events/ts/src/interop.ts listen-group on
// node g's NATS. It needs node >= 24 and pnpm (it installs the
// locked dependencies, as examples/events/check.sh does).
func s11StartTS(t *testing.T, env *harness.Env, root string, g *harness.Gateway) *s11Script {
	t.Helper()
	s := &s11Script{lang: "TypeScript"}
	node, err := exec.LookPath("node")
	if err != nil {
		s.skip = "node not on PATH"
		return s
	}
	if _, err := exec.LookPath("pnpm"); err != nil {
		s.skip = "pnpm not on PATH"
		return s
	}
	v, err := exec.Command(node, "--version").Output()
	if err != nil {
		s.skip = fmt.Sprintf("node --version: %v", err)
		return s
	}
	major, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(strings.TrimSpace(string(v)), "v"), ".", 2)[0])
	if major < 24 {
		s.skip = fmt.Sprintf("node %s: the example needs node >= 24", strings.TrimSpace(string(v)))
		return s
	}
	ctx := harness.Ctx(t, 10*time.Minute)
	for _, dir := range []string{"ts/events", "examples/events/ts"} {
		if why := s11Setup(ctx, filepath.Join(root, dir), "pnpm", "install", "--frozen-lockfile"); why != "" {
			s.skip = why
			return s
		}
	}
	s.proc = env.StartWith("ts-listener", harness.ProcOpts{
		Dir: filepath.Join(root, "examples/events/ts"),
		Env: []string{"NATS_URL=" + g.NATSURL()},
	}, node, "src/interop.ts", "listen-group", "ts")
	s.proc.WaitLog("ready", 60*time.Second)
	return s
}

// s11StartPy starts examples/events/py/interop.py listen on node g's
// NATS. It needs uv (uv sync --locked, as py/check.sh does).
func s11StartPy(t *testing.T, env *harness.Env, root string, g *harness.Gateway) *s11Script {
	t.Helper()
	s := &s11Script{lang: "Python"}
	if _, err := exec.LookPath("uv"); err != nil {
		s.skip = "uv not on PATH"
		return s
	}
	dir := filepath.Join(root, "examples/events/py")
	if why := s11Setup(harness.Ctx(t, 10*time.Minute), dir, "uv", "sync", "--locked", "--quiet"); why != "" {
		s.skip = why
		return s
	}
	// The venv's python directly: no uv process between the harness and
	// the listener. stdin stays open; interop.py listens until EOF.
	s.proc = env.StartWith("py-listener", harness.ProcOpts{
		Dir:   dir,
		Env:   []string{"NATS_URL=" + g.NATSURL(), "PYTHONUNBUFFERED=1"},
		Stdin: true,
	}, filepath.Join(dir, ".venv", "bin", "python"), "interop.py", "listen", "--group", "py", "--prefetch", "1")
	s.proc.WaitLog("READY", 60*time.Second)
	return s
}

func s11Head(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
