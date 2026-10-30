package gat_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IodeSystems/graphql-go/v2"
	"nhooyr.io/websocket"

	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/internal/gqlguard"
)

type hookKey struct{}

// hookSchema has `who`, which reads hookKey from the resolver context,
// and `echo(s)`, which returns its argument.
func hookSchema(t testing.TB) *graphql.Schema {
	t.Helper()
	s, err := graphql.NewSchema(graphql.SchemaConfig{
		Query: graphql.NewObject(graphql.ObjectConfig{
			Name: "Query",
			Fields: graphql.Fields{
				"who": &graphql.Field{
					Type: graphql.String,
					Resolve: func(p graphql.ResolveParams) (any, error) {
						v, _ := p.Context.Value(hookKey{}).(string)
						return v, nil
					},
				},
				"echo": &graphql.Field{
					Type: graphql.String,
					Args: graphql.FieldConfigArgument{"s": &graphql.ArgumentConfig{Type: graphql.String}},
					Resolve: func(p graphql.ResolveParams) (any, error) {
						return p.Args["s"], nil
					},
				},
			},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &s
}

func newHookGat(t *testing.T, configure func(*gat.Gateway)) http.Handler {
	t.Helper()
	g, err := gat.New()
	if err != nil {
		t.Fatal(err)
	}
	configure(g)
	if err := g.SetSchema(hookSchema(t), nil, nil); err != nil {
		t.Fatal(err)
	}
	return g.Handler()
}

func serve(h http.Handler, method, target, contentType, body string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestPreRequest(t *testing.T) {
	var done atomic.Int32
	h := newHookGat(t, func(g *gat.Gateway) {
		g.PreRequest(func(w http.ResponseWriter, r *http.Request) (context.Context, func(context.Context), bool) {
			if r.Header.Get("X-Refuse") != "" {
				http.Error(w, "refused", http.StatusTeapot)
				return nil, nil, false
			}
			return context.WithValue(r.Context(), hookKey{}, "hooked"), func(ctx context.Context) {
				if ctx.Value(hookKey{}) == "hooked" {
					done.Add(1)
				}
			}, true
		})
	})
	rr := serve(h, http.MethodPost, "/graphql", "application/json", `{"query":"{ who }"}`)
	if rr.Body.String() != `{"data":{"who":"hooked"}}` {
		t.Fatalf("hook context not seen by the resolver: %d %s", rr.Code, rr.Body)
	}
	// A batch runs with the hook's context too.
	rr = serve(h, http.MethodPost, "/graphql", "application/json", `[{"query":"{ who }"}]`)
	if rr.Body.String() != `[{"data":{"who":"hooked"}}]` {
		t.Fatalf("batch: %s", rr.Body)
	}
	if done.Load() != 2 {
		t.Fatalf("done ran %d times, want 2", done.Load())
	}
	rr = serve(h, http.MethodPost, "/graphql", "application/json", `{"query":"{ who }"}`, "X-Refuse", "1")
	if rr.Code != http.StatusTeapot || done.Load() != 2 {
		t.Fatalf("refused request: %d %s done=%d", rr.Code, rr.Body, done.Load())
	}
}

func TestParseContentType(t *testing.T) {
	h := newHookGat(t, func(g *gat.Gateway) {
		g.ParseContentType("application/graphql", 64, func(r *http.Request) (gat.Request, error) {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				return gat.Request{}, err
			}
			if len(b) == 0 {
				return gat.Request{}, errors.New("empty document")
			}
			return gat.Request{Query: string(b)}, nil
		})
	})
	rr := serve(h, http.MethodPost, "/graphql", "Application/GraphQL; charset=utf-8", `{ echo(s: "hi") }`)
	if rr.Body.String() != `{"data":{"echo":"hi"}}` {
		t.Fatalf("parsed body: %d %s", rr.Code, rr.Body)
	}
	rr = serve(h, http.MethodPost, "/graphql", "application/graphql", ``)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "empty document") {
		t.Fatalf("parser error: %d %s", rr.Code, rr.Body)
	}
	rr = serve(h, http.MethodPost, "/graphql", "application/graphql", `{ echo(s: "`+strings.Repeat("x", 80)+`") }`)
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), `"code":"BAD_REQUEST"`) {
		t.Fatalf("over the parser's cap: %d %s", rr.Code, rr.Body)
	}
	// Other types still read as JSON, under MaxRequestBytes.
	rr = serve(h, http.MethodPost, "/graphql", "application/json", `{"query":"{ echo(s: \"json\") }"}`)
	if rr.Body.String() != `{"data":{"echo":"json"}}` {
		t.Fatalf("JSON body: %s", rr.Body)
	}
}

func TestAcceptStringVariables(t *testing.T) {
	body := `{"query":"query($s: String) { echo(s: $s) }","variables":"{\"s\":\"str\"}"}`
	h := newHookGat(t, func(*gat.Gateway) {})
	if rr := serve(h, http.MethodPost, "/graphql", "application/json", body); rr.Code != http.StatusBadRequest {
		t.Fatalf("string variables accepted by default: %d %s", rr.Code, rr.Body)
	}
	h = newHookGat(t, func(g *gat.Gateway) { g.AcceptStringVariables(true) })
	if rr := serve(h, http.MethodPost, "/graphql", "application/json", body); rr.Body.String() != `{"data":{"echo":"str"}}` {
		t.Fatalf("string variables: %d %s", rr.Code, rr.Body)
	}
	if rr := serve(h, http.MethodPost, "/graphql", "application/json", "["+body+"]"); rr.Body.String() != `[{"data":{"echo":"str"}}]` {
		t.Fatalf("string variables in a batch: %d %s", rr.Code, rr.Body)
	}
	if rr := serve(h, http.MethodPost, "/graphql", "application/json", `{"query":"{ who }","variables":"[1]"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("string holding a non-object: %d %s", rr.Code, rr.Body)
	}
	if rr := serve(h, http.MethodPost, "/graphql", "application/json", `{"query":"{ who }","variables":3}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("number variables: %d %s", rr.Code, rr.Body)
	}
}

func TestGraphiQLHook(t *testing.T) {
	h := newHookGat(t, func(g *gat.Gateway) {
		g.GraphiQL(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "<html>page</html>")
		}))
	})
	if rr := serve(h, http.MethodGet, "/graphql?query={who}", "", "", "Accept", "text/html"); rr.Body.String() != "<html>page</html>" {
		t.Fatalf("browser GET: %s", rr.Body)
	}
	if rr := serve(h, http.MethodGet, "/graphql?query={who}&raw", "", "", "Accept", "text/html"); rr.Body.String() != `{"data":{"who":""}}` {
		t.Fatalf("?raw: %s", rr.Body)
	}
	if rr := serve(h, http.MethodGet, "/graphql?query={who}", "", "", "Accept", "text/html, application/json"); rr.Body.String() != `{"data":{"who":""}}` {
		t.Fatalf("accepts JSON: %s", rr.Body)
	}
	// Without the hook a browser gets JSON.
	h = newHookGat(t, func(*gat.Gateway) {})
	if rr := serve(h, http.MethodGet, "/graphql?query={who}", "", "", "Accept", "text/html"); rr.Body.String() != `{"data":{"who":""}}` {
		t.Fatalf("no hook: %s", rr.Body)
	}
}

func TestUsePlanCache(t *testing.T) {
	cache := graphql.NewPlanCache(graphql.PlanCacheOptions{Rules: []graphql.ValidationRuleFn{gqlguard.NoIntrospectionRule}})
	// The host cache's rules replace gat's: MaxOperations(1) does not apply.
	h := newHookGat(t, func(g *gat.Gateway) { g.MaxOperations(1).UsePlanCache(cache) })
	if rr := serve(h, http.MethodPost, "/graphql", "application/json", `{"query":"{ a: who b: who }"}`); strings.Contains(rr.Body.String(), "errors") {
		t.Fatalf("gat's own rules ran: %s", rr.Body)
	}
	if rr := serve(h, http.MethodPost, "/graphql", "application/json", `{"query":"{ __schema { queryType { name } } }"}`); !strings.Contains(rr.Body.String(), "introspection is disabled") {
		t.Fatalf("host cache's rule did not run: %s", rr.Body)
	}
	if _, misses := cache.HitsMisses(); misses == 0 {
		t.Fatal("host cache never consulted")
	}
}

func TestWSUpgradeHook(t *testing.T) {
	var (
		refuse  atomic.Bool
		ended   = make(chan struct{}, 1)
		cancels = make(chan context.CancelFunc, 1)
	)
	h := newHookGat(t, func(g *gat.Gateway) {
		g.WSUpgrade(func(w http.ResponseWriter, r *http.Request) (context.Context, func(context.Context), bool) {
			if refuse.Load() {
				http.Error(w, "limit", http.StatusTooManyRequests)
				return nil, nil, false
			}
			ctx, cancel := context.WithCancel(r.Context())
			cancels <- cancel
			return ctx, func(context.Context) { ended <- struct{}{} }, true
		})
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := dialGraphQLWS(t, ctx, srv.URL, "")
	// Cancelling the hook's context closes the connection.
	(<-cancels)()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("connection still open after its context was cancelled")
	}
	select {
	case <-ended:
	case <-ctx.Done():
		t.Fatal("done not called when the connection ended")
	}

	refuse.Store(true)
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{
		Subprotocols: []string{"graphql-transport-ws"},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("refused upgrade: err=%v resp=%v", err, resp)
	}
}

func TestRequestBodyTooLargeCarriesCode(t *testing.T) {
	h := newHookGat(t, func(g *gat.Gateway) { g.MaxRequestBytes(16) })
	rr := serve(h, http.MethodPost, "/graphql", "application/json", `{"query":"{ who who who }"}`)
	var out struct {
		Errors []struct {
			Extensions map[string]any `json:"extensions"`
		} `json:"errors"`
	}
	if rr.Code != http.StatusRequestEntityTooLarge || json.Unmarshal(rr.Body.Bytes(), &out) != nil ||
		len(out.Errors) != 1 || out.Errors[0].Extensions["code"] != "BAD_REQUEST" {
		t.Fatalf("413: %d %s", rr.Code, rr.Body)
	}
}

// tickSchema has a subscription that sends one tick, then waits for
// its context.
func tickSchema(t testing.TB) *graphql.Schema {
	t.Helper()
	s, err := graphql.NewSchema(graphql.SchemaConfig{
		Query: graphql.NewObject(graphql.ObjectConfig{Name: "Query", Fields: graphql.Fields{
			"v": &graphql.Field{Type: graphql.String},
		}}),
		Subscription: graphql.NewObject(graphql.ObjectConfig{Name: "Subscription", Fields: graphql.Fields{
			"tick": &graphql.Field{
				Type: graphql.String,
				Subscribe: func(p graphql.ResolveParams) (any, error) {
					ch := make(chan any, 1)
					ch <- "tick"
					go func() {
						<-p.Context.Done()
						close(ch)
					}()
					return ch, nil
				},
				Resolve: func(p graphql.ResolveParams) (any, error) { return p.Source, nil },
			},
		}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &s
}

func TestEchoComplete(t *testing.T) {
	for _, echo := range []bool{false, true} {
		g, err := gat.New()
		if err != nil {
			t.Fatal(err)
		}
		g.EchoComplete(echo)
		if err := g.SetSchema(tickSchema(t), nil, nil); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(g.Handler())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn := dialGraphQLWS(t, ctx, srv.URL, "")
		wsSend(t, ctx, conn, subscribeMsg("1", `subscription { tick }`))
		if m := wsRecv(t, ctx, conn); m.Type != "next" {
			t.Fatalf("echo=%v: want next, got %+v", echo, m)
		}
		wsSend(t, ctx, conn, wsMsg{ID: "1", Type: "complete"})
		if echo {
			if m := wsRecv(t, ctx, conn); m.Type != "complete" || m.ID != "1" {
				t.Fatalf("echo=true: want complete, got %+v", m)
			}
		}
		wsSend(t, ctx, conn, wsMsg{Type: "ping"})
		if m := wsRecv(t, ctx, conn); m.Type != "pong" {
			t.Fatalf("echo=%v: want pong, got %+v", echo, m)
		}
		cancel()
		srv.Close()
	}
}
