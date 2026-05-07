package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// petsWithSubsIntrospection extends petsIntrospection with a Tick
// subscription field, so we exercise the subscription mirror.
//
//	type Subscription { tick(every: Int): Int! }
const petsWithSubsIntrospection = `{
  "data": {
    "__schema": {
      "queryType": {"name": "Query"},
      "mutationType": null,
      "subscriptionType": {"name": "Subscription"},
      "types": [
        {
          "kind": "OBJECT", "name": "Query", "fields": [
            {
              "name": "users",
              "args": [],
              "type": {"kind": "NON_NULL", "ofType": {"kind": "LIST", "ofType": {"kind": "NON_NULL", "ofType": {"kind": "OBJECT", "name": "User"}}}}
            }
          ]
        },
        {
          "kind": "OBJECT", "name": "Subscription", "fields": [
            {
              "name": "tick",
              "args": [{"name": "every", "type": {"kind": "SCALAR", "name": "Int"}}],
              "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "Int"}}
            }
          ]
        },
        {
          "kind": "OBJECT", "name": "User", "fields": [
            {"name": "id", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "ID"}}}
          ]
        }
      ]
    }
  }
}`

// fakeUpstreamGraphQLWSServer is a minimal graphql-transport-ws
// server for testing. It accepts one connection_init, replies with
// connection_ack, then on subscribe emits N `next` frames followed
// by `complete`.
func fakeUpstreamGraphQLWSServer(t *testing.T, frames []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Non-WS GET / POST: serve introspection.
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal(body, &req)
			if strings.Contains(req.Query, "IntrospectionQuery") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(petsWithSubsIntrospection))
				return
			}
			http.Error(w, "expected introspection", http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{wsSubprotocol},
		})
		if err != nil {
			t.Logf("upstream ws accept: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// 1. connection_init → connection_ack
		var m wsMessage
		if err := wsReadJSON(ctx, conn, &m); err != nil || m.Type != msgConnInit {
			return
		}
		if err := wsWriteJSON(ctx, conn, wsMessage{Type: msgConnAck}); err != nil {
			return
		}

		// 2. subscribe → emit each frame as `next` → complete
		if err := wsReadJSON(ctx, conn, &m); err != nil || m.Type != msgSubscribe {
			return
		}
		subID := m.ID
		for _, f := range frames {
			payload, _ := json.Marshal(map[string]any{"data": f})
			if err := wsWriteJSON(ctx, conn, wsMessage{ID: subID, Type: msgNext, Payload: payload}); err != nil {
				return
			}
		}
		_ = wsWriteJSON(ctx, conn, wsMessage{ID: subID, Type: msgComplete})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGraphQLIngest_SubscriptionForwarding subscribes through the
// gateway WS, expects frames forwarded from the upstream, then
// `complete` when the upstream finishes.
func TestGraphQLIngest_SubscriptionForwarding(t *testing.T) {
	upstream := fakeUpstreamGraphQLWSServer(t, []map[string]any{
		{"tick": 1},
		{"tick": 2},
		{"tick": 3},
	})

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithoutSubscriptionAuth(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	if err := gw.AddGraphQL(upstream.URL, As("pets")); err != nil {
		t.Fatalf("AddGraphQL: %v", err)
	}

	// Stand the gateway up and dial its WS.
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/graphql"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{wsSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial gateway ws: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	if err := wsWriteJSON(ctx, conn, wsMessage{Type: msgConnInit}); err != nil {
		t.Fatalf("connection_init: %v", err)
	}
	var m wsMessage
	if err := wsReadJSON(ctx, conn, &m); err != nil || m.Type != msgConnAck {
		t.Fatalf("expected connection_ack, got %+v err=%v", m, err)
	}

	subPayload, _ := json.Marshal(subscribePayload{
		Query: `subscription { pets_tick }`,
	})
	if err := wsWriteJSON(ctx, conn, wsMessage{ID: "1", Type: msgSubscribe, Payload: subPayload}); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}

	// Read frames until complete (or timeout).
	var (
		muF      sync.Mutex
		gotTicks []int
		gotDone  bool
	)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if err := wsReadJSON(ctx, conn, &m); err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch m.Type {
		case msgNext:
			var payload struct {
				Data struct {
					Tick int `json:"pets_tick"`
				} `json:"data"`
			}
			if err := json.Unmarshal(m.Payload, &payload); err != nil {
				t.Fatalf("decode next payload %s: %v", m.Payload, err)
			}
			muF.Lock()
			gotTicks = append(gotTicks, payload.Data.Tick)
			muF.Unlock()
		case msgComplete:
			gotDone = true
		case msgError:
			t.Fatalf("unexpected error frame: %s", m.Payload)
		}
		if gotDone {
			break
		}
	}
	muF.Lock()
	defer muF.Unlock()
	if !gotDone {
		t.Fatal("never received complete")
	}
	if got, want := fmt.Sprintf("%v", gotTicks), "[1 2 3]"; got != want {
		t.Errorf("ticks = %s, want %s", got, want)
	}
}
