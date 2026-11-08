package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/ir"
	psv1 "github.com/iodesystems/gwag/v2/gw/proto/ps/v1"
)

// compileEventProto compiles src (which may import options.proto) as
// one file.
func compileEventProto(t *testing.T, name string, src string) protoreflect.FileDescriptor {
	t.Helper()
	optionsBytes, err := os.ReadFile("proto/ps/v1/options.proto")
	if err != nil {
		t.Fatalf("read options.proto: %v", err)
	}
	fd, err := compileProtoBytes(name, []byte(src), map[string][]byte{
		"gw/proto/ps/v1/options.proto": optionsBytes,
	})
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return fd
}

const ordersEventsProto = `syntax = "proto3";
package orders.v1;

import "gw/proto/ps/v1/options.proto";

message OrderCreated {
  option (gwag.ps.v1.binding) = { name: "OrderEvents", pattern: "events.orders.{orderId}" };
  string order_id = 1;
  int64 total = 2;
}

message OrderShipped {
  option (gwag.ps.v1.binding) = { name: "OrderEvents", pattern: "events.orders.{orderId}" };
  string order_id = 1;
  string carrier = 2;
}

message Audit {
  option (gwag.ps.v1.binding) = { name: "Audit", pattern: "events.audit" };
  string who = 1;
}

service Orders {
  rpc Get(OrderCreated) returns (OrderCreated);
}
`

func TestExtractChannelBindings_NamedAndUnion(t *testing.T) {
	fd := compileEventProto(t, "orders.proto", ordersEventsProto)
	got, err := extractChannelBindings(fd)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d bindings: %+v", len(got), got)
	}
	for _, b := range got[:2] {
		if b.Name != "OrderEvents" || b.Channel != "orders.v1.OrderEvents" || b.Pattern != "events.orders.{orderId}" {
			t.Errorf("binding %+v", b)
		}
	}
	if got[2].Channel != "orders.v1.Audit" {
		t.Errorf("audit channel = %q", got[2].Channel)
	}
}

func TestExtractChannelBindings_Rejects(t *testing.T) {
	cases := map[string]string{
		"same name, different pattern": `
message A { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.x" }; }
message B { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.y" }; }`,
		"same pattern, different names": `
message A { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.x" }; }
message B { option (gwag.ps.v1.binding) = { name: "F", pattern: "a.x" }; }`,
		"named and unnamed on one pattern": `
message A { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.*" }; }
message B { option (gwag.ps.v1.binding) = { pattern: "a.{id}" }; }`,
		"malformed brace": `
message A { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.{id" }; }`,
		"brace inside segment": `
message A { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.x{id}" }; }`,
		"duplicate segment name": `
message A { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.{id}.{id}" }; }`,
		"name without pattern": `
message A { option (gwag.ps.v1.binding) = { name: "E" }; }`,
		"name not an identifier": `
message A { option (gwag.ps.v1.binding) = { name: "E-1", pattern: "a.x" }; }`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			src := "syntax = \"proto3\";\npackage p.v1;\nimport \"gw/proto/ps/v1/options.proto\";\n" + body + "\n"
			fd := compileEventProto(t, "p.proto", src)
			if _, err := extractChannelBindings(fd); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestNormalizeChannelPattern(t *testing.T) {
	norm, names, err := normalizeChannelPattern("events.{tenant}.orders.{orderId}.>")
	if err != nil {
		t.Fatal(err)
	}
	if norm != "events.*.orders.*.>" || strings.Join(names, ",") != "tenant,orderId" {
		t.Errorf("got %q %v", norm, names)
	}
	for _, bad := range []string{"", "a..b", "a.>.b", "a.{}", "a.{1x}", "a.b*"} {
		if _, _, err := normalizeChannelPattern(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	if !subjectMatchesPattern("events.orders.{orderId}", "events.orders.42") {
		t.Error("named segment should match one token")
	}
	if subjectMatchesPattern("events.orders.{orderId}", "events.orders.42.x") {
		t.Error("named segment matched two tokens")
	}
}

// installEventSlot installs src as an internal-proto slot at (ns, ver)
// with a no-op handler for every unary method of its first service.
func installEventSlot(t *testing.T, g *Gateway, ns, ver, file, src string) error {
	t.Helper()
	fd := compileEventProto(t, file, src)
	handlers := map[string]internalProtoHandler{}
	svc := fd.Services().Get(0)
	for i := 0; i < svc.Methods().Len(); i++ {
		handlers[string(svc.Methods().Get(i).Name())] = func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
			return req, nil
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.addInternalProtoSlotLocked(ns, ver, fd, []byte(src), handlers, nil)
}

func TestEventChannels_UnionIndexAndCrossSlot(t *testing.T) {
	g := newSlotGateway(t)
	if err := installEventSlot(t, g, "orders", "v1", "orders.proto", ordersEventsProto); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.rebuildChannelBindingIndexLocked()
	g.mu.Unlock()
	idx := g.channelBindingIndex.Load()
	c := idx.lookupChannel("events.orders.42")
	if c == nil || c.key != "orders.v1.OrderEvents" || !c.union() || len(c.descs) != 2 {
		t.Fatalf("lookupChannel = %+v", c)
	}
	if c.streamName() != "EV_orders_v1_OrderEvents" {
		t.Errorf("stream name %q", c.streamName())
	}

	// Another slot claiming the same pattern (spelled with a different
	// segment name) is rejected.
	other := `syntax = "proto3";
package billing.v1;
import "gw/proto/ps/v1/options.proto";
message Charged {
  option (gwag.ps.v1.binding) = { name: "OrderEvents", pattern: "events.orders.{id}" };
  string id = 1;
}
service Billing { rpc Get(Charged) returns (Charged); }
`
	err := installEventSlot(t, g, "billing", "v1", "billing.proto", other)
	if err == nil || !strings.Contains(err.Error(), "cross-slot pattern conflict") {
		t.Fatalf("want cross-slot conflict, got %v", err)
	}
}

// typedPubSubGateway boots a one-node cluster gateway with the orders
// event slot installed and the events.> space open.
func typedPubSubGateway(t *testing.T, opts ...Option) (*Gateway, *Cluster) {
	t.Helper()
	cluster, err := StartCluster(ClusterOptions{
		NodeName:      "typed-ps",
		ClientListen:  "127.0.0.1:0",
		ClusterListen: "127.0.0.1:0",
		DataDir:       t.TempDir(),
		StartTimeout:  10 * time.Second,
		LogLevel:      "silent",
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cluster.Close)
	g := New(append([]Option{
		WithCluster(cluster),
		WithoutMetrics(),
		WithoutBackpressure(),
		WithChannelAuth("events.>", ChannelAuthOpen),
	}, opts...)...)
	t.Cleanup(g.Close)
	if err := installEventSlot(t, g, "orders", "v1", "orders.proto", ordersEventsProto); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	err = g.assembleLocked()
	g.mu.Unlock()
	if err != nil {
		t.Fatalf("assembleLocked: %v", err)
	}
	return g, cluster
}

func psDispatchers(t *testing.T, g *Gateway) (pub, sub ir.Dispatcher) {
	t.Helper()
	pub = g.dispatchers.Get(ir.MakeSchemaID(psNamespace, psVersion, "Pub"))
	sub = g.dispatchers.Get(ir.MakeSchemaID(psNamespace, psVersion, "Sub"))
	if pub == nil || sub == nil {
		t.Fatal("ps dispatchers not registered")
	}
	return pub, sub
}

func waitNATSSubs(t *testing.T, cl *Cluster, n uint32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for cl.Server.NumSubscriptions() < n {
		if time.Now().After(deadline) {
			t.Fatalf("NATS subscriptions = %d, want >= %d", cl.Server.NumSubscriptions(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func nextEvent(t *testing.T, ch chan any) map[string]any {
	t.Helper()
	select {
	case raw, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed")
		}
		return raw.(map[string]any)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for event")
	}
	return nil
}

// TestTypedEvents_SubTranscodesRawProto: a typed event published
// straight onto NATS (raw proto body + Gwag-Type header) reaches
// ps.sub as JSON; an unknown type is dropped; envelope events still
// pass.
func TestTypedEvents_SubTranscodesRawProto(t *testing.T) {
	g, cl := typedPubSubGateway(t)
	_, sub := psDispatchers(t, g)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	before := cl.Server.NumSubscriptions()
	src, err := sub.Dispatch(ctx, map[string]any{"channel": "events.orders.{orderId}"})
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	events := src.(chan any)
	waitNATSSubs(t, cl, before+1)

	desc := g.channelBindingIndex.Load().messageDesc("orders.v1.OrderShipped")
	msg := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal([]byte(`{"orderId":"42","carrier":"ups"}`), msg); err != nil {
		t.Fatal(err)
	}
	body, _ := proto.Marshal(msg)
	nc := cl.Conn
	unknown := nats.NewMsg("events.orders.42")
	unknown.Header[EventHeaderType] = []string{"nope.v1.Missing"}
	unknown.Data = body
	if err := nc.PublishMsg(unknown); err != nil {
		t.Fatal(err)
	}
	typed := nats.NewMsg("events.orders.42")
	typed.Header[EventHeaderType] = []string{"orders.v1.OrderShipped"}
	typed.Header[EventHeaderChannel] = []string{"orders.v1.OrderEvents"}
	typed.Header[EventHeaderMsgID] = []string{"m-1"}
	typed.Data = body
	if err := nc.PublishMsg(typed); err != nil {
		t.Fatal(err)
	}
	env, _ := proto.Marshal(&psv1.Event{Channel: "events.orders.43", Payload: "legacy"})
	if err := nc.Publish("events.orders.43", env); err != nil {
		t.Fatal(err)
	}

	ev := nextEvent(t, events)
	if ev["payloadType"] != "orders.v1.OrderShipped" || ev["channel"] != "events.orders.42" {
		t.Fatalf("typed event = %v", ev)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(ev["payload"].(string)), &got); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if got["carrier"] != "ups" || got["orderId"] != "42" {
		t.Errorf("payload = %v", got)
	}
	ev = nextEvent(t, events)
	if ev["payload"] != "legacy" {
		t.Errorf("envelope event = %v (unknown type should have been dropped)", ev)
	}
}

// TestTypedEvents_PubPublishesRawProto: ps.pub with JSON on a named
// channel lands on NATS as binary proto with the typed-event headers.
func TestTypedEvents_PubPublishesRawProto(t *testing.T) {
	g, cl := typedPubSubGateway(t)
	pub, _ := psDispatchers(t, g)
	got := make(chan *nats.Msg, 4)
	s, err := cl.Conn.ChanSubscribe("events.>", got)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Unsubscribe() })
	_ = cl.Conn.Flush()

	// Union channel without payloadType: refused.
	_, err = pub.Dispatch(context.Background(), map[string]any{
		"channel": "events.orders.7", "payload": `{"orderId":"7"}`,
	})
	if err == nil || !strings.Contains(err.Error(), "payloadType required") {
		t.Fatalf("union without payloadType: err = %v", err)
	}
	// Wrong shape: refused.
	_, err = pub.Dispatch(context.Background(), map[string]any{
		"channel": "events.orders.7", "payload": `{"nope":1}`, "payloadType": "orders.v1.OrderCreated",
	})
	if err == nil {
		t.Fatal("bad payload accepted")
	}
	if _, err := pub.Dispatch(context.Background(), map[string]any{
		"channel": "events.orders.7", "payload": `{"orderId":"7","total":"12"}`, "payloadType": "orders.v1.OrderCreated",
	}); err != nil {
		t.Fatalf("pub: %v", err)
	}
	// Plain channel: payloadType defaults to the bound type.
	if _, err := pub.Dispatch(context.Background(), map[string]any{
		"channel": "events.audit", "payload": `{"who":"ann"}`,
	}); err != nil {
		t.Fatalf("pub audit: %v", err)
	}

	for i, want := range []struct{ subject, typ, channel string }{
		{"events.orders.7", "orders.v1.OrderCreated", "orders.v1.OrderEvents"},
		{"events.audit", "orders.v1.Audit", "orders.v1.Audit"},
	} {
		var m *nats.Msg
		select {
		case m = <-got:
		case <-time.After(3 * time.Second):
			t.Fatalf("message %d: timeout", i)
		}
		if m.Subject != want.subject || m.Header.Get(EventHeaderType) != want.typ || m.Header.Get(EventHeaderChannel) != want.channel || m.Header.Get(EventHeaderMsgID) == "" {
			t.Fatalf("message %d: subject %s headers %v", i, m.Subject, m.Header)
		}
		desc := g.channelBindingIndex.Load().messageDesc(want.typ)
		dm := dynamicpb.NewMessage(desc)
		if err := proto.Unmarshal(m.Data, dm); err != nil {
			t.Fatalf("message %d: body is not %s: %v", i, want.typ, err)
		}
		if i == 0 && dm.Get(desc.Fields().ByName("total")).Int() != 12 {
			t.Errorf("total = %v", dm.Get(desc.Fields().ByName("total")))
		}
	}
}

// TestTypedEvents_PubOverGraphQL: the payload_type argument reaches
// ps.pub through the GraphQL handler.
func TestTypedEvents_PubOverGraphQL(t *testing.T) {
	g, cl := typedPubSubGateway(t)
	got := make(chan *nats.Msg, 1)
	s, err := cl.Conn.ChanSubscribe("events.orders.9", got)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Unsubscribe() })
	_ = cl.Conn.Flush()
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	q := `{"query":"{ ps { pub(channel: \"events.orders.9\", payload: \"{\\\"orderId\\\":\\\"9\\\"}\", payload_type: \"orders.v1.OrderShipped\") { __typename } } }"}`
	resp, err := http.Post(srv.URL+"/graphql", "application/json", strings.NewReader(q))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "errors") {
		t.Fatalf("graphql: %s", body)
	}
	select {
	case m := <-got:
		if m.Header.Get(EventHeaderType) != "orders.v1.OrderShipped" {
			t.Fatalf("headers %v", m.Header)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no message; response %s", body)
	}
}
