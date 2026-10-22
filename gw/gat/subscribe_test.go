package gat_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"nhooyr.io/websocket"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

type sessionEventsInput struct {
	Session string `query:"session" required:"true"`
	After   int64  `query:"after"`
}

type sessionEvent struct {
	Seq  int64  `json:"seq"`
	Kind string `json:"kind"`
	By   string `json:"by"`
}

// newSubscribeGat registers a dund-shaped subscription: events of a
// session after a seq, guarded by requireUser. The source replays
// seqs (after, 3] from "history", then forwards live events from
// the returned channel until ctx is done.
func newSubscribeGat(t *testing.T) (*httptest.Server, chan sessionEvent, chan struct{}) {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t)
	live := make(chan sessionEvent, 4)
	stopped := make(chan struct{}, 4) // one send per source goroutine exit

	// One plain op so the Query root is non-empty.
	gat.Register(api, g, huma.Operation{
		OperationID: "getProject",
		Method:      http.MethodGet,
		Path:        "/projects/{id}",
	}, func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
		return &getProjectOutput{Body: project{ID: in.ID}}, nil
	})

	gat.Subscribe(api, g, huma.Operation{
		OperationID: "sessionEvents",
		Summary:     "Events of a session after a seq.",
		Middlewares: huma.Middlewares{requireUser(api)},
	}, func(ctx context.Context, in *sessionEventsInput) (<-chan *sessionEvent, error) {
		user, _ := ctx.Value(principalKey{}).(string)
		if user == "" {
			t.Errorf("source reached without a principal")
		}
		if in.Session != "s1" {
			return nil, huma.Error404NotFound("no session " + in.Session)
		}
		out := make(chan *sessionEvent)
		go func() {
			defer func() { stopped <- struct{}{} }()
			defer close(out)
			for seq := in.After + 1; seq <= 3; seq++ {
				select {
				case out <- &sessionEvent{Seq: seq, Kind: "replay", By: user}:
				case <-ctx.Done():
					return
				}
			}
			for {
				select {
				case ev, ok := <-live:
					if !ok {
						return
					}
					ev.By = user
					select {
					case out <- &ev:
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
		return out, nil
	})

	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, live, stopped
}

type wsMsg struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func dialGraphQLWS(t *testing.T, ctx context.Context, url, user string) *websocket.Conn {
	t.Helper()
	h := http.Header{}
	if user != "" {
		h.Set("X-User", user)
	}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http"), &websocket.DialOptions{
		Subprotocols: []string{"graphql-transport-ws"},
		HTTPHeader:   h,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	wsSend(t, ctx, conn, wsMsg{Type: "connection_init"})
	if m := wsRecv(t, ctx, conn); m.Type != "connection_ack" {
		t.Fatalf("want connection_ack, got %+v", m)
	}
	return conn
}

func wsSend(t *testing.T, ctx context.Context, c *websocket.Conn, m wsMsg) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func wsRecv(t *testing.T, ctx context.Context, c *websocket.Conn) wsMsg {
	t.Helper()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m wsMsg
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

func subscribeMsg(id, query string) wsMsg {
	p, _ := json.Marshal(map[string]any{"query": query})
	return wsMsg{ID: id, Type: "subscribe", Payload: p}
}

const sessionEventsQuery = `subscription { Demo_sessionEvents(session: "s1", after: 1) { seq kind by } }`

func TestSubscribe_ReplayThenLive(t *testing.T) {
	srv, live, stopped := newSubscribeGat(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := dialGraphQLWS(t, ctx, srv.URL+"/api/graphql", "alice")
	wsSend(t, ctx, conn, subscribeMsg("1", sessionEventsQuery))

	next := func() map[string]any {
		t.Helper()
		m := wsRecv(t, ctx, conn)
		if m.Type != "next" || m.ID != "1" {
			t.Fatalf("want next for 1, got %+v %s", m, m.Payload)
		}
		var res map[string]any
		_ = json.Unmarshal(m.Payload, &res)
		ev, _ := digPath(res, "data", "Demo_sessionEvents").(map[string]any)
		if ev == nil {
			t.Fatalf("no event in %s", m.Payload)
		}
		return ev
	}
	// Long renders as a string unless LongAsNumber is set.
	for _, want := range []string{"2", "3"} {
		ev := next()
		if fmt.Sprint(ev["seq"]) != want || ev["kind"] != "replay" || ev["by"] != "alice" {
			t.Errorf("replay event = %v, want seq %v by alice", ev, want)
		}
	}
	live <- sessionEvent{Seq: 4, Kind: "live"}
	if ev := next(); fmt.Sprint(ev["seq"]) != "4" || ev["kind"] != "live" {
		t.Errorf("live event = %v", ev)
	}

	// Client complete ends the subscription; the connection stays up.
	wsSend(t, ctx, conn, wsMsg{ID: "1", Type: "complete"})
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("source ctx not cancelled after complete")
	}
	wsSend(t, ctx, conn, wsMsg{Type: "ping"})
	if m := wsRecv(t, ctx, conn); m.Type != "pong" {
		t.Errorf("after complete: want pong, got %+v", m)
	}
}

func TestSubscribe_MiddlewareRejects(t *testing.T) {
	srv, _, _ := newSubscribeGat(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := dialGraphQLWS(t, ctx, srv.URL+"/api/graphql", "")
	wsSend(t, ctx, conn, subscribeMsg("1", sessionEventsQuery))
	m := wsRecv(t, ctx, conn)
	if m.Type != "error" || !strings.Contains(string(m.Payload), "login required") {
		t.Fatalf("want error with login required, got %+v %s", m, m.Payload)
	}
	if !strings.Contains(string(m.Payload), `"status":401`) {
		t.Errorf("error lacks status 401 extension: %s", m.Payload)
	}
}

func TestSubscribe_SourceError(t *testing.T) {
	srv, _, _ := newSubscribeGat(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := dialGraphQLWS(t, ctx, srv.URL+"/api/graphql", "alice")
	wsSend(t, ctx, conn, subscribeMsg("1", `subscription { Demo_sessionEvents(session: "nope") { seq } }`))
	m := wsRecv(t, ctx, conn)
	if m.Type != "error" || !strings.Contains(string(m.Payload), "no session nope") {
		t.Fatalf("want source error, got %+v %s", m, m.Payload)
	}
}

func TestSubscribe_SchemaAndREST(t *testing.T) {
	srv, _, _ := newSubscribeGat(t)
	sdl := string(mustGET(t, srv.URL+"/api/schema/graphql"))
	for _, want := range []string{"type Subscription", "Demo_sessionEvents(", "session: String!", "after: Long"} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL missing %q:\n%s", want, sdl)
		}
	}
	spec := string(mustGET(t, srv.URL+"/openapi.json"))
	if strings.Contains(spec, "_gat/subscriptions") || strings.Contains(spec, "sessionEvents") {
		t.Errorf("subscription leaked into the REST OpenAPI paths")
	}
	resp, err := http.Get(srv.URL + "/_gat/subscriptions/sessionEvents")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("synthetic path is routed: %d", resp.StatusCode)
	}
}
