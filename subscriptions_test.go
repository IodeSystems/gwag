package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"

	greeterv1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/greeter/v1"
)

// nopGRPCConn is a stub grpc.ClientConnInterface attached to the
// greeter pool. The subscription path never invokes it (NATS pub/sub
// is the streaming transport); unary calls would fail, but tests
// don't exercise unary on the subscription fixture.
type nopGRPCConn struct{}

func (nopGRPCConn) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	return fmt.Errorf("nopGRPCConn: unary not supported in subscription tests")
}

func (nopGRPCConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, fmt.Errorf("nopGRPCConn: streams not supported")
}

type subFixture struct {
	gw      *Gateway
	cluster *Cluster
	server  *httptest.Server
	wsURL   string
}

func (f *subFixture) close() {
	f.server.Close()
	f.gw.Close()
	f.cluster.Close()
}

// newSubFixture spins an embedded NATS cluster, registers greeter, and
// exposes gw.Handler() over httptest. opts apply to gateway.New (use
// WithSubscriptionAuth / WithoutSubscriptionAuth to flip auth modes).
func newSubFixture(t *testing.T, opts ...Option) *subFixture {
	t.Helper()
	dir := t.TempDir()
	// :0 lets the OS pick free ports — avoids collision when tests run
	// in parallel or alongside other gateway processes on dev machines.
	cluster, err := StartCluster(ClusterOptions{
		NodeName:      "test",
		ClientListen:  "127.0.0.1:0",
		ClusterListen: "127.0.0.1:0",
		DataDir:       dir,
		StartTimeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cluster.Close)

	allOpts := append([]Option{
		WithCluster(cluster),
		WithoutMetrics(),
		WithoutBackpressure(),
	}, opts...)
	gw := New(allOpts...)
	t.Cleanup(gw.Close)

	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(nopGRPCConn{}),
		As("greeter"),
	); err != nil {
		t.Fatalf("AddProtoDescriptor greeter: %v", err)
	}

	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/graphql"
	return &subFixture{gw: gw, cluster: cluster, server: srv, wsURL: wsURL}
}

// dialWS opens a graphql-transport-ws connection and returns it after
// completing the connection_init / connection_ack handshake.
func dialWS(t *testing.T, ctx context.Context, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols: []string{"graphql-transport-ws"},
	})
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	if err := writeWS(conn, ctx, wsMessage{Type: "connection_init"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	ack, err := readWS(conn, ctx)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack.Type != "connection_ack" {
		t.Fatalf("expected connection_ack, got %s", ack.Type)
	}
	return conn
}

func writeWS(conn *websocket.Conn, ctx context.Context, m wsMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

func readWS(conn *websocket.Conn, ctx context.Context) (wsMessage, error) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, raw, err := conn.Read(rctx)
	if err != nil {
		return wsMessage{}, err
	}
	var m wsMessage
	return m, json.Unmarshal(raw, &m)
}

// publishGreeting marshals a Greeting and publishes it to subject.
func publishGreeting(t *testing.T, c *Cluster, subject, greeting, forName string) {
	t.Helper()
	g := &greeterv1.Greeting{Greeting: greeting, ForName: forName}
	b, err := proto.Marshal(g)
	if err != nil {
		t.Fatalf("marshal greeting: %v", err)
	}
	if err := c.Conn.Publish(subject, b); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Flush so the gateway's subscriber actually sees it before we read.
	if err := c.Conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func TestSubscriptionE2E_HappyPath(t *testing.T) {
	f := newSubFixture(t, WithoutSubscriptionAuth())
	defer f.close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := dialWS(t, ctx, f.wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	subPayload, _ := json.Marshal(subscribePayload{
		Query: `subscription { greeter_greetings(name:"alice", hmac:"x", timestamp:0) { greeting forName } }`,
	})
	if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: "subscribe", Payload: subPayload}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Give the gateway a moment to register the NATS subscription.
	deadline := time.Now().Add(2 * time.Second)
	for f.gw.subscriptionBroker().activeSubjectCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("broker never registered subject")
		}
		time.Sleep(20 * time.Millisecond)
	}

	publishGreeting(t, f.cluster, "events.greeter.Greetings.alice", "hi alice", "alice")

	msg, err := readWS(conn, ctx)
	if err != nil {
		t.Fatalf("read next: %v", err)
	}
	if msg.Type != "next" || msg.ID != "1" {
		t.Fatalf("unexpected frame: %s id=%s payload=%s", msg.Type, msg.ID, msg.Payload)
	}
	var result struct {
		Data struct {
			GreeterGreetings map[string]any `json:"greeter_greetings"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(msg.Payload, &result); err != nil {
		t.Fatalf("decode payload: %v: %s", err, msg.Payload)
	}
	if len(result.Errors) > 0 {
		t.Fatalf("graphql errors: %v", result.Errors)
	}
	if got := result.Data.GreeterGreetings["greeting"]; got != "hi alice" {
		t.Fatalf("greeting=%v want %q", got, "hi alice")
	}
	if got := result.Data.GreeterGreetings["forName"]; got != "alice" {
		t.Fatalf("forName=%v want %q", got, "alice")
	}
}

func TestSubscriptionE2E_HMACMismatch(t *testing.T) {
	secret := []byte("test-secret-32-bytes-long-padding")
	f := newSubFixture(t, WithSubscriptionAuth(SubscriptionAuthOptions{Secret: secret}))
	defer f.close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dialWS(t, ctx, f.wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// Send a wrong hmac with a current timestamp.
	subPayload, _ := json.Marshal(subscribePayload{
		Query: fmt.Sprintf(
			`subscription { greeter_greetings(name:"alice", hmac:"AAAA", timestamp:%d) { greeting } }`,
			time.Now().Unix(),
		),
	})
	if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: "subscribe", Payload: subPayload}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	msg, err := readWS(conn, ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msg.Type != "error" {
		t.Fatalf("expected error frame, got %s payload=%s", msg.Type, msg.Payload)
	}
	if !strings.Contains(string(msg.Payload), "SIGNATURE_MISMATCH") {
		t.Fatalf("expected SIGNATURE_MISMATCH in payload, got %s", msg.Payload)
	}
}

func TestSubscriptionE2E_HMACTooOld(t *testing.T) {
	secret := []byte("test-secret-32-bytes-long-padding")
	f := newSubFixture(t, WithSubscriptionAuth(SubscriptionAuthOptions{
		Secret:     secret,
		SkewWindow: 30 * time.Second,
	}))
	defer f.close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dialWS(t, ctx, f.wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// Sign with the right secret but a timestamp well outside the skew.
	old := time.Now().Add(-1 * time.Hour).Unix()
	hmacB64, _ := SignSubscribeToken(secret, "events.greeter.Greetings.alice", 60)
	subPayload, _ := json.Marshal(subscribePayload{
		Query: fmt.Sprintf(
			`subscription { greeter_greetings(name:"alice", hmac:%q, timestamp:%d) { greeting } }`,
			hmacB64, old,
		),
	})
	if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: "subscribe", Payload: subPayload}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	msg, err := readWS(conn, ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msg.Type != "error" {
		t.Fatalf("expected error frame, got %s payload=%s", msg.Type, msg.Payload)
	}
	if !strings.Contains(string(msg.Payload), "TOO_OLD") {
		t.Fatalf("expected TOO_OLD in payload, got %s", msg.Payload)
	}
}

func TestSubscriptionE2E_NotConfigured(t *testing.T) {
	// No WithSubscriptionAuth and no WithoutSubscriptionAuth → secret is
	// empty, Insecure is false → NOT_CONFIGURED.
	f := newSubFixture(t)
	defer f.close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dialWS(t, ctx, f.wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	subPayload, _ := json.Marshal(subscribePayload{
		Query: `subscription { greeter_greetings(name:"alice", hmac:"x", timestamp:0) { greeting } }`,
	})
	if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: "subscribe", Payload: subPayload}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	msg, err := readWS(conn, ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msg.Type != "error" {
		t.Fatalf("expected error frame, got %s payload=%s", msg.Type, msg.Payload)
	}
	if !strings.Contains(string(msg.Payload), "NOT_CONFIGURED") {
		t.Fatalf("expected NOT_CONFIGURED in payload, got %s", msg.Payload)
	}
}

func TestSubscriptionE2E_AdminEventsWatchServices(t *testing.T) {
	// End-to-end test of the admin_events_watchServices Subscription
	// field: register a service, observe a ServiceChange frame on the
	// WS, including round-tripping the proto enum through graphql-go's
	// JSON serialiser (action should land as "ACTION_REGISTERED", not
	// the numeric enum value).
	f := newSubFixture(t, WithoutSubscriptionAuth())
	defer f.close()
	if err := f.gw.AddAdminEvents(); err != nil {
		t.Fatalf("AddAdminEvents: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := dialWS(t, ctx, f.wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	subPayload, _ := json.Marshal(subscribePayload{
		Query: `subscription {
			admin_events_watchServices(namespace: "watched", hmac: "x", timestamp: 0) {
				action namespace version replicaCount
			}
		}`,
	})
	if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: "subscribe", Payload: subPayload}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Wait for the broker to register the subject before triggering
	// the publish — otherwise the first event slips past.
	deadline := time.Now().Add(2 * time.Second)
	for f.gw.subscriptionBroker().activeSubjectCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("broker never registered admin_events subject")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Trigger a register on the watched namespace → publishes a
	// ServiceChange to events.admin_events.WatchServices.watched.
	if err := f.gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(nopGRPCConn{}),
		As("watched"),
	); err != nil {
		t.Fatalf("AddProtoDescriptor watched: %v", err)
	}

	msg, err := readWS(conn, ctx)
	if err != nil {
		t.Fatalf("read next: %v", err)
	}
	if msg.Type != "next" {
		t.Fatalf("unexpected frame type %s payload=%s", msg.Type, msg.Payload)
	}
	var result struct {
		Data struct {
			AdminEvents map[string]any `json:"admin_events_watchServices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(msg.Payload, &result); err != nil {
		t.Fatalf("decode payload: %v: %s", err, msg.Payload)
	}
	change := result.Data.AdminEvents
	if change["action"] != "ACTION_REGISTERED" {
		t.Errorf("action = %v (%T), want %q (graphql-go should serialise the enum NAME, not the number)",
			change["action"], change["action"], "ACTION_REGISTERED")
	}
	if change["namespace"] != "watched" {
		t.Errorf("namespace = %v, want watched", change["namespace"])
	}
	if change["version"] != "v1" {
		t.Errorf("version = %v, want v1", change["version"])
	}
	if n, ok := change["replicaCount"].(float64); !ok || int(n) != 1 {
		t.Errorf("replicaCount = %v (%T), want 1", change["replicaCount"], change["replicaCount"])
	}
}

func TestSubscriptionE2E_ClientCompleteCleansUp(t *testing.T) {
	f := newSubFixture(t, WithoutSubscriptionAuth())
	defer f.close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := dialWS(t, ctx, f.wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	subPayload, _ := json.Marshal(subscribePayload{
		Query: `subscription { greeter_greetings(name:"alice", hmac:"x", timestamp:0) { greeting } }`,
	})
	if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: "subscribe", Payload: subPayload}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Wait for the subject to register.
	deadline := time.Now().Add(2 * time.Second)
	for f.gw.subscriptionBroker().activeSubjectCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("broker never registered subject")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Client says complete; gateway must release the broker entry.
	if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: "complete"}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for f.gw.subscriptionBroker().activeSubjectCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("broker still has %d active subjects", f.gw.subscriptionBroker().activeSubjectCount())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
