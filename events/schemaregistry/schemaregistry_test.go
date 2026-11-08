package schemaregistry_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/memory"
	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	"github.com/iodesystems/gwag/v2/events/schemaregistry/srtest"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
)

// registry returns SR_URL (a real registry; examples/events/go-kafka/
// test.sh starts one) or an in-memory fake.
func registry(t *testing.T) *sr.Client {
	t.Helper()
	u := os.Getenv("SR_URL")
	if u == "" {
		f := srtest.New()
		t.Cleanup(f.Close)
		u = f.URL
	}
	c, err := sr.NewClient(sr.Config{URL: u})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFrame(t *testing.T) {
	for _, tc := range []struct {
		idx  []int
		want []byte
	}{
		{[]int{0}, []byte{0, 0, 0, 1, 2, 0, 'b'}},
		{[]int{1}, []byte{0, 0, 0, 1, 2, 2, 2, 'b'}},
		{[]int{2, 0}, []byte{0, 0, 0, 1, 2, 4, 4, 0, 'b'}},
	} {
		got := sr.Frame(258, tc.idx, []byte("b"))
		if !bytes.Equal(got, tc.want) {
			t.Errorf("Frame(%v) = %v, want %v", tc.idx, got, tc.want)
		}
		id, idx, body, err := sr.Unframe(got)
		if err != nil || id != 258 || string(body) != "b" || len(idx) != len(tc.idx) {
			t.Errorf("Unframe = %d %v %q %v", id, idx, body, err)
		}
	}
	if _, _, _, err := sr.Unframe([]byte{1, 2, 3, 4, 5, 6}); !errors.Is(err, sr.ErrNotFramed) {
		t.Errorf("Unframe(plain) err = %v", err)
	}
	md := (&ordersv1.OrderCreated{}).ProtoReflect().Descriptor()
	if got := sr.MessageIndexes(md); len(got) != 1 || got[0] != 4 {
		t.Errorf("MessageIndexes(OrderCreated) = %v", got)
	}
}

var orderEvents = events.MustChannel("orders.v1.OrderEvents", "events.orders.{orderId}.lifecycle")

// own: AutoRegister registers the file and its gwag options import on
// first publish; the subscriber decodes it.
func TestOwnAutoRegister(t *testing.T) {
	c := registry(t)
	topic := "own-" + events.NewID()[:8]
	ctx := context.Background()
	b := memory.New()
	defer b.Close()
	bus := events.NewCore(topicBackend{b, topic}, events.WithSerde(mustSerde(t, c, sr.AutoRegister())))
	got := make(chan *events.Message, 1)
	stop, err := bus.Subscribe(ctx, orderEvents, "g", func(ctx context.Context, segs []string, m *events.Message) error {
		got <- m
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := bus.Publish(ctx, orderEvents, []string{"o-1"}, &ordersv1.OrderCancelled{OrderId: "o-1", Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	m := <-got
	var oc ordersv1.OrderCancelled
	if err := proto.Unmarshal(m.Data, &oc); err != nil || oc.GetReason() != "r" {
		t.Fatalf("decoded %v: %v", &oc, err)
	}
	if m.Headers[sr.HeaderSchemaID] == "" || m.Type() != "orders.v1.OrderCancelled" {
		t.Fatalf("headers %v", m.Headers)
	}
	v, err := c.SubjectVersion(ctx, topic+"-value", "latest", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.References) != 1 || v.References[0].Name != "gw/proto/ps/v1/options.proto" || v.References[0].Subject != "gw/proto/ps/v1/options.proto" {
		t.Fatalf("references %+v", v.References)
	}
}

// A framed record without gwag headers (another producer) gets its
// type from the schema id and message index.
func TestDecodeWithoutHeaders(t *testing.T) {
	c := registry(t)
	ctx := context.Background()
	topic := "imp-" + events.NewID()[:8]
	s := mustSerde(t, c, sr.AutoRegister())
	out := &events.Message{Headers: map[string]string{}}
	if err := s.Encode(ctx, orderEvents, topic, &ordersv1.OrderCreated{OrderId: "o-9", Customer: "bo"}, out); err != nil {
		t.Fatal(err)
	}
	if out.Data[0] != sr.MagicByte {
		t.Fatalf("not framed: %v", out.Data[:6])
	}
	in := &events.Message{Headers: map[string]string{}, Data: out.Data}
	if err := mustSerde(t, c).Decode(ctx, orderEvents, topic, in); err != nil {
		t.Fatal(err)
	}
	if in.Type() != "orders.v1.OrderCreated" {
		t.Fatalf("type %q", in.Type())
	}
	var oc ordersv1.OrderCreated
	if err := proto.Unmarshal(in.Data, &oc); err != nil || oc.GetCustomer() != "bo" {
		t.Fatalf("%v %v", &oc, err)
	}
	// TypeNames renames for a shadow copy.
	in = &events.Message{Headers: map[string]string{}, Data: out.Data}
	if err := mustSerde(t, c, sr.TypeNames(map[string]string{"orders.v1.OrderCreated": "shop.v1.Created"})).Decode(ctx, orderEvents, topic, in); err != nil || in.Type() != "shop.v1.Created" {
		t.Fatalf("renamed type %q: %v", in.Type(), err)
	}
	// Plain proto passes through.
	plain, _ := proto.Marshal(&oc)
	in = &events.Message{Headers: map[string]string{events.HeaderType: "orders.v1.OrderCreated"}, Data: plain}
	if err := mustSerde(t, c).Decode(ctx, orderEvents, topic, in); err != nil || !bytes.Equal(in.Data, plain) {
		t.Fatalf("plain: %v", err)
	}
}

// Lookup-only (the default) fails until the schema is registered;
// UseLatest frames with the latest version.
func TestLookupAndLatest(t *testing.T) {
	c := registry(t)
	ctx := context.Background()
	topic := "look-" + events.NewID()[:8]
	out := &events.Message{Headers: map[string]string{}}
	err := mustSerde(t, c).Encode(ctx, orderEvents, topic, &ordersv1.OrderCreated{}, out)
	if !errors.Is(err, sr.ErrNotRegistered) {
		t.Fatalf("lookup of unregistered: %v", err)
	}
	if _, err := c.EnsureFile(ctx, topic+"-value", (&ordersv1.OrderCreated{}).ProtoReflect().Descriptor().ParentFile(), sr.RegisterOptions{Register: true}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*sr.Serde{mustSerde(t, c), mustSerde(t, c, sr.UseLatest())} {
		if err := s.Encode(ctx, orderEvents, topic, &ordersv1.OrderCreated{}, out); err != nil {
			t.Fatal(err)
		}
		if _, idx, _, err := sr.Unframe(out.Data); err != nil || len(idx) != 1 || idx[0] != 4 {
			t.Fatalf("indexes %v: %v", idx, err)
		}
	}
	if sub := mustSerde(t, c, sr.WithSubjectNameStrategy(sr.TopicRecordNameStrategy)).Subject("t", (&ordersv1.OrderCreated{}).ProtoReflect().Descriptor()); sub != "t-orders.v1.OrderCreated" {
		t.Fatalf("TopicRecordNameStrategy: %s", sub)
	}
}

func TestAuth(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.Write([]byte(`{"compatibilityLevel":"FULL"}`))
	}))
	defer srv.Close()
	for _, cfg := range []sr.Config{
		{URL: "http://u:p@" + srv.Listener.Addr().String()},
		{URL: srv.URL, Token: "tok"},
	} {
		c, err := sr.NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if l, err := c.CompatibilityLevel(context.Background(), "s"); err != nil || l != "FULL" {
			t.Fatalf("level %q: %v", l, err)
		}
		if c.URL() != srv.URL {
			t.Fatalf("URL() = %q keeps user info", c.URL())
		}
	}
	if got[0] != "Basic dTpw" || got[1] != "Bearer tok" {
		t.Fatalf("Authorization = %q", got)
	}
}

// topicBackend names every channel's topic, as the Kafka backend's
// mapper does.
type topicBackend struct {
	events.Backend
	topic string
}

func (b topicBackend) Topic(events.Channel) string { return b.topic }

func mustSerde(t *testing.T, c *sr.Client, opts ...sr.SerdeOption) *sr.Serde {
	t.Helper()
	s, err := sr.NewSerde(c, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// More than one of AutoRegister, UseLatest, UseSchemaID is rejected,
// whatever the order, with the text the TypeScript and Python serdes use.
func TestExclusiveModes(t *testing.T) {
	c, _ := sr.NewClient(sr.Config{URL: "http://127.0.0.1:1"})
	for _, opts := range [][]sr.SerdeOption{
		{sr.AutoRegister(), sr.UseLatest()},
		{sr.UseSchemaID(3), sr.AutoRegister()},
		{sr.UseLatest(), sr.UseSchemaID(3)},
		{sr.AutoRegister(), sr.UseLatest(), sr.UseSchemaID(3)},
	} {
		s, err := sr.NewSerde(c, opts...)
		if !errors.Is(err, sr.ErrExclusiveModes) || s != nil {
			t.Fatalf("got %v, %v", s, err)
		}
	}
	if got := sr.ErrExclusiveModes.Error(); got != "schema registry: auto-register, use-latest and use-schema-id are mutually exclusive; set at most one" {
		t.Fatalf("text %q", got)
	}
	if _, err := sr.NewSerde(c, sr.UseLatest(), sr.Normalize()); err != nil {
		t.Fatal(err)
	}
}

// ChannelClient sends one channel's lookups and decodes to another
// registry (another id space); the other channels use the serde's.
func TestChannelClient(t *testing.T) {
	ctx := context.Background()
	own, corp := srtest.New(), srtest.New()
	defer own.Close()
	defer corp.Close()
	oc, _ := sr.NewClient(sr.Config{URL: own.URL})
	cc, _ := sr.NewClient(sr.Config{URL: corp.URL})
	// Burn an id in corp, so the two registries frame with other ids.
	corp.Add("filler-value", sr.Schema{Schema: `syntax = "proto3"; package f; message F {}`, SchemaType: "PROTOBUF"})
	s, err := sr.NewSerde(oc, sr.AutoRegister(), sr.ChannelClient("corp.v1.Orders", cc))
	if err != nil {
		t.Fatal(err)
	}
	corpCh := events.Channel{Key: "corp.v1.Orders"}
	ownCh := events.Channel{Key: "orders.v1.OrderEvents"}
	var a, b events.Message
	if err := s.Encode(ctx, corpCh, "corp-orders", &ordersv1.OrderCreated{OrderId: "c"}, &a); err != nil {
		t.Fatal(err)
	}
	if err := s.Encode(ctx, ownCh, "own-orders", &ordersv1.OrderCreated{OrderId: "o"}, &b); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.SubjectVersion(ctx, "corp-orders-value", "latest", ""); err != nil {
		t.Errorf("corp channel not registered in its registry: %v", err)
	}
	if _, err := oc.SubjectVersion(ctx, "corp-orders-value", "latest", ""); !sr.IsNotFound(err) {
		t.Errorf("corp channel registered in the serde's registry: %v", err)
	}
	if _, err := oc.SubjectVersion(ctx, "own-orders-value", "latest", ""); err != nil {
		t.Errorf("own channel: %v", err)
	}
	aid, _, _, _ := sr.Unframe(a.Data)
	bid, _, _, _ := sr.Unframe(b.Data)
	if aid == bid {
		t.Fatalf("both framed with id %d; the test needs the id spaces apart", aid)
	}
	for _, tc := range []struct {
		ch events.Channel
		m  *events.Message
		id int
	}{{corpCh, &a, aid}, {ownCh, &b, bid}} {
		tc.m.Headers = map[string]string{}
		if err := s.Decode(ctx, tc.ch, "", tc.m); err != nil || tc.m.Headers[sr.HeaderSchemaID] != fmt.Sprint(tc.id) {
			t.Errorf("decode on %s: %v %v", tc.ch.Key, err, tc.m.Headers)
		}
	}
	// The corp channel's id is not in the serde's own registry.
	m := events.Message{Data: sr.Frame(aid, []int{0}, nil), Headers: map[string]string{}}
	if err := s.Decode(ctx, ownCh, "", &m); err == nil {
		t.Errorf("corp id %d decoded on the own channel: %v", aid, m.Headers)
	}
	for _, opt := range []sr.SerdeOption{sr.ChannelClient("", cc), sr.ChannelClient("corp.v1.Orders", nil)} {
		_, err := sr.NewSerde(oc, opt)
		if err == nil || !strings.HasPrefix(err.Error(), "schema registry: channel client for channel ") || !strings.HasSuffix(err.Error(), ": want a channel key and a client") {
			t.Errorf("bad channel client: %v", err)
		}
	}
}
