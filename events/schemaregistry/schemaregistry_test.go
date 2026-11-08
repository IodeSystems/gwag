package schemaregistry_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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
	bus := events.NewCore(topicBackend{b, topic}, events.WithSerde(sr.NewSerde(c, sr.AutoRegister())))
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
	s := sr.NewSerde(c, sr.AutoRegister())
	out := &events.Message{Headers: map[string]string{}}
	if err := s.Encode(ctx, orderEvents, topic, &ordersv1.OrderCreated{OrderId: "o-9", Customer: "bo"}, out); err != nil {
		t.Fatal(err)
	}
	if out.Data[0] != sr.MagicByte {
		t.Fatalf("not framed: %v", out.Data[:6])
	}
	in := &events.Message{Headers: map[string]string{}, Data: out.Data}
	if err := sr.NewSerde(c).Decode(ctx, orderEvents, topic, in); err != nil {
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
	if err := sr.NewSerde(c, sr.TypeNames(map[string]string{"orders.v1.OrderCreated": "shop.v1.Created"})).Decode(ctx, orderEvents, topic, in); err != nil || in.Type() != "shop.v1.Created" {
		t.Fatalf("renamed type %q: %v", in.Type(), err)
	}
	// Plain proto passes through.
	plain, _ := proto.Marshal(&oc)
	in = &events.Message{Headers: map[string]string{events.HeaderType: "orders.v1.OrderCreated"}, Data: plain}
	if err := sr.NewSerde(c).Decode(ctx, orderEvents, topic, in); err != nil || !bytes.Equal(in.Data, plain) {
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
	err := sr.NewSerde(c).Encode(ctx, orderEvents, topic, &ordersv1.OrderCreated{}, out)
	if !errors.Is(err, sr.ErrNotRegistered) {
		t.Fatalf("lookup of unregistered: %v", err)
	}
	if _, err := c.EnsureFile(ctx, topic+"-value", (&ordersv1.OrderCreated{}).ProtoReflect().Descriptor().ParentFile(), sr.RegisterOptions{Register: true}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*sr.Serde{sr.NewSerde(c), sr.NewSerde(c, sr.UseLatest())} {
		if err := s.Encode(ctx, orderEvents, topic, &ordersv1.OrderCreated{}, out); err != nil {
			t.Fatal(err)
		}
		if _, idx, _, err := sr.Unframe(out.Data); err != nil || len(idx) != 1 || idx[0] != 4 {
			t.Fatalf("indexes %v: %v", idx, err)
		}
	}
	if sub := sr.NewSerde(c, sr.WithSubjectNameStrategy(sr.TopicRecordNameStrategy)).Subject("t", (&ordersv1.OrderCreated{}).ProtoReflect().Descriptor()); sub != "t-orders.v1.OrderCreated" {
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
