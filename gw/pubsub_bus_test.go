package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"nhooyr.io/websocket"

	"github.com/iodesystems/gwag/v2/gw/gat/bustest"
	"github.com/iodesystems/gwag/v2/gw/ir"
)

// startNATSOn boots a standalone NATS server on port (-1 = random).
// Shutdown is registered for cleanup; calling it earlier is fine.
func startNATSOn(t *testing.T, port int) *natsd.Server {
	t.Helper()
	srv, err := natsd.NewServer(&natsd.Options{Host: "127.0.0.1", Port: port, NoSigs: true, NoLog: true})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func connectNATS(t *testing.T, url string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url, nats.MaxReconnects(-1), nats.ReconnectWait(20*time.Millisecond))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func TestFailoverBus_Conformance(t *testing.T) {
	bustest.Run(t, func(t *testing.T) bustest.Harness {
		nc := connectNATS(t, startNATSOn(t, -1).ClientURL())
		b := newFailoverBus(nc, nil)
		t.Cleanup(b.Close)
		return bustest.Harness{Bus: b, Sync: func() { _ = nc.Flush() }}
	})
}

func TestFailoverBus_NilConnIsLocal(t *testing.T) {
	var changes []bool
	b := newFailoverBus(nil, func(d bool) { changes = append(changes, d) })
	defer b.Close()
	if !b.Degraded() || len(changes) != 1 || !changes[0] {
		t.Fatalf("Degraded=%v changes=%v; want degraded, one change to true", b.Degraded(), changes)
	}
	bustest.Run(t, func(t *testing.T) bustest.Harness {
		b := newFailoverBus(nil, nil)
		return bustest.Harness{Bus: b}
	})
}

// counter records deliveries per payload.
type counter struct {
	mu sync.Mutex
	n  map[string]int
}

func newCounter() *counter { return &counter{n: map[string]int{}} }

func (c *counter) fn(_ string, p []byte) {
	c.mu.Lock()
	c.n[string(p)]++
	c.mu.Unlock()
}

func (c *counter) get(p string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[p]
}

func (c *counter) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int{}
	for k, v := range c.n {
		out[k] = v
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestFailoverBus_DegradeAndRecover stops and restarts the NATS
// server under a failoverBus and checks every event reaches a
// subscriber exactly once across both transitions: NATS-delivered
// before, local-delivered while down (including a subscription opened
// while down), NATS-delivered again after reconnect (remote
// publishes reach both subscriptions, so both were re-bound).
func TestFailoverBus_DegradeAndRecover(t *testing.T) {
	srv := startNATSOn(t, -1)
	port := srv.Addr().(*net.TCPAddr).Port
	url := srv.ClientURL()
	nc := connectNATS(t, url)
	remote := connectNATS(t, url) // another node on the cluster

	changes := make(chan bool, 8)
	b := newFailoverBus(nc, func(d bool) { changes <- d })
	defer b.Close()
	if b.Degraded() {
		t.Fatal("degraded at start with a connected conn")
	}
	waitChange := func(want bool) {
		t.Helper()
		select {
		case got := <-changes:
			if got != want {
				t.Fatalf("transition to degraded=%v, want %v", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no transition to degraded=%v", want)
		}
	}

	a := newCounter()
	unA, err := b.Subscribe("t.>", a.fn)
	if err != nil {
		t.Fatal(err)
	}
	defer unA()
	_ = nc.Flush()

	// Healthy: remote publish and own publish both arrive via NATS.
	_ = remote.Publish("t.a", []byte("r1"))
	_ = remote.Flush()
	if err := b.Publish("t.a", []byte("l1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "r1,l1", func() bool { return a.get("r1") == 1 && a.get("l1") == 1 })

	// NATS down → degraded; publish delivers locally, synchronously.
	srv.Shutdown()
	waitChange(true)
	if err := b.Publish("t.a", []byte("d1")); err != nil {
		t.Fatal(err)
	}
	if a.get("d1") != 1 {
		t.Fatalf("d1 delivered %d times while degraded, want 1", a.get("d1"))
	}
	bb := newCounter()
	unB, err := b.Subscribe("t.b", bb.fn)
	if err != nil {
		t.Fatalf("Subscribe while degraded: %v", err)
	}
	defer unB()
	_ = b.Publish("t.b", []byte("d2"))
	if bb.get("d2") != 1 || a.get("d2") != 1 {
		t.Fatalf("d2: b=%d a=%d, want 1/1", bb.get("d2"), a.get("d2"))
	}

	// NATS back on the same port → recovered.
	startNATSOn(t, port)
	waitChange(false)
	waitFor(t, "remote reconnect", remote.IsConnected)
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush after reconnect: %v", err)
	}
	_ = remote.Publish("t.a", []byte("r2"))
	_ = remote.Publish("t.b", []byte("r3"))
	_ = remote.Flush()
	_ = b.Publish("t.a", []byte("l2"))
	waitFor(t, "r2,r3,l2", func() bool {
		return a.get("r2") == 1 && a.get("r3") == 1 && bb.get("r3") == 1 && a.get("l2") == 1
	})
	_ = nc.Flush()
	time.Sleep(200 * time.Millisecond) // let any duplicate land

	wantA := map[string]int{"r1": 1, "l1": 1, "d1": 1, "d2": 1, "r2": 1, "r3": 1, "l2": 1}
	if got := a.snapshot(); !sameCounts(got, wantA) {
		t.Errorf("subscriber t.> got %v, want %v", got, wantA)
	}
	wantB := map[string]int{"d2": 1, "r3": 1}
	if got := bb.snapshot(); !sameCounts(got, wantB) {
		t.Errorf("subscriber t.b got %v, want %v", got, wantB)
	}
}

func sameCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestPubSub_SingleNodeE2E: no cluster; ps_sub over graphql-transport-ws
// receives a ps.pub sent over HTTP.
func TestPubSub_SingleNodeE2E(t *testing.T) {
	g := New(
		WithoutMetrics(),
		WithoutBackpressure(),
		WithChannelAuth("events.>", ChannelAuthOpen),
	)
	t.Cleanup(g.Close)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn := dialWS(t, ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/graphql")
	defer conn.Close(websocket.StatusNormalClosure, "done")
	sub, _ := json.Marshal(subscribePayload{
		Query: `subscription { ps_sub(channel: "events.single.>") { channel payload ts } }`,
	})
	if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: "subscribe", Payload: sub}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "broker subject", func() bool { return len(g.activeSubjects()) == 1 })

	body := postGQL(t, g.Handler(), `{ ps { pub(channel: "events.single.1", payload: "hi") { _void } } }`)
	if strings.Contains(body, `"errors"`) {
		t.Fatalf("pub: %s", body)
	}

	msg, err := readWS(conn, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != "next" {
		t.Fatalf("frame %s: %s", msg.Type, msg.Payload)
	}
	var res struct {
		Data struct {
			PsSub map[string]any `json:"ps_sub"`
		} `json:"data"`
	}
	if err := json.Unmarshal(msg.Payload, &res); err != nil {
		t.Fatal(err)
	}
	if res.Data.PsSub["channel"] != "events.single.1" || res.Data.PsSub["payload"] != "hi" {
		t.Fatalf("event = %v", res.Data.PsSub)
	}
	if h := healthBody(t, g); h["pubsub"] != pubSubModeLocal || h["degraded"] != nil {
		t.Errorf("health = %v, want pubsub=local and no degraded flag", h)
	}
}

// TestPubSub_ClusterDegradeHealthAndMetric: shutting the embedded NATS
// server down flips health to local_degraded (status code unchanged),
// sets the gauge, and ps.pub / ps.sub keep working on this node.
func TestPubSub_ClusterDegradeHealthAndMetric(t *testing.T) {
	cluster, err := StartCluster(ClusterOptions{
		NodeName:      "degrade-test",
		ClientListen:  freeAddr(t),
		ClusterListen: "127.0.0.1:0",
		DataDir:       t.TempDir(),
		StartTimeout:  10 * time.Second,
		LogLevel:      "silent",
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cluster.Close)
	g := New(
		WithCluster(cluster),
		WithoutBackpressure(),
		WithChannelAuth("events.>", ChannelAuthOpen),
	)
	t.Cleanup(g.Close)
	if h := healthBody(t, g); h["pubsub"] != pubSubModeNATS {
		t.Fatalf("health before = %v, want pubsub=nats", h)
	}
	if v := gaugeValue(t, g, "go_api_gateway_pubsub_bus_degraded"); v != "0" {
		t.Fatalf("gauge before = %q, want 0", v)
	}

	cluster.Server.Shutdown()
	waitFor(t, "degraded", func() bool { return g.pubSubBusMode() == pubSubModeDegraded })

	rec := httptest.NewRecorder()
	g.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("health code = %d while degraded, want 200", rec.Code)
	}
	if h := healthBody(t, g); h["pubsub"] != pubSubModeDegraded || h["degraded"] != true {
		t.Errorf("health = %v, want pubsub=local_degraded degraded=true", h)
	}
	if v := gaugeValue(t, g, "go_api_gateway_pubsub_bus_degraded"); v != "1" {
		t.Errorf("gauge = %q, want 1", v)
	}

	g.mu.Lock()
	if err := g.assembleLocked(); err != nil {
		g.mu.Unlock()
		t.Fatal(err)
	}
	g.mu.Unlock()
	pub := g.dispatchers.Get(ir.MakeSchemaID(psNamespace, psVersion, "Pub"))
	sub := g.dispatchers.Get(ir.MakeSchemaID(psNamespace, psVersion, "Sub"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src, err := sub.Dispatch(ctx, map[string]any{"channel": "events.deg"})
	if err != nil {
		t.Fatalf("Sub while degraded: %v", err)
	}
	if _, err := pub.Dispatch(context.Background(), map[string]any{"channel": "events.deg", "payload": "x"}); err != nil {
		t.Fatalf("Pub while degraded: %v", err)
	}
	select {
	case raw := <-src.(chan any):
		if ev := raw.(map[string]any); ev["payload"] != "x" {
			t.Errorf("event = %v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no local delivery while degraded")
	}
}

func healthBody(t *testing.T, g *Gateway) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	g.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("health body: %v", err)
	}
	return m
}

// gaugeValue scrapes MetricsHandler and returns the sample value of
// an unlabelled metric, or "" when absent.
func gaugeValue(t *testing.T, g *Gateway, name string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	g.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return strconv.FormatFloat(f, 'f', -1, 64)
			}
			return v
		}
	}
	return ""
}
