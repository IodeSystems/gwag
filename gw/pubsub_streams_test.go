package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func waitStreams(t *testing.T, g *Gateway, want int) {
	t.Helper()
	w := strconv.Itoa(want)
	waitFor(t, "ps stream gauges = "+w, func() bool {
		body := scrapeBody(t, g)
		health, _ := healthBody(t, g)["active_streams"].(float64)
		return series(body, mp+"streams_inflight_total") == w &&
			series(body, mp+`pool_streams_inflight{namespace="ps",version="v1"}`) == w &&
			int(health) == want
	})
}

func psStreamFixture(t *testing.T) (*Gateway, *httptest.Server) {
	g := metricsGateway(t, WithChannelAuth("events.>", ChannelAuthOpen))
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return g, srv
}

func openPSSub(t *testing.T, ctx context.Context, srv *httptest.Server, ch string) *websocket.Conn {
	t.Helper()
	conn := dialWS(t, ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/graphql")
	sub, _ := json.Marshal(subscribePayload{Query: `subscription { ps_sub(channel: "` + ch + `") { payload } }`})
	if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: "subscribe", Payload: sub}); err != nil {
		t.Fatal(err)
	}
	return conn
}

// ps_sub streams count in streams_inflight_total, health active_streams
// and pool_streams_inflight{ps,v1}; client close balances.
func TestPubSubStreamGauges_ClientClose(t *testing.T) {
	g, srv := psStreamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c1 := openPSSub(t, ctx, srv, "events.a")
	c2 := openPSSub(t, ctx, srv, "events.b")
	waitStreams(t, g, 2)
	c1.Close(websocket.StatusNormalClosure, "bye")
	waitStreams(t, g, 1)
	c2.Close(websocket.StatusNormalClosure, "bye")
	waitStreams(t, g, 0)
}

// Drain cancels the websockets and waits on streamGlobal; ps_sub
// streams must be part of that wait and end at zero.
func TestPubSubStreamGauges_Drain(t *testing.T) {
	g, srv := psStreamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := openPSSub(t, ctx, srv, "events.a")
	defer c.Close(websocket.StatusNormalClosure, "")
	waitStreams(t, g, 1)
	if err := g.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	waitStreams(t, g, 0)
}
