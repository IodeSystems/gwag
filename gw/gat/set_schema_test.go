package gat_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/IodeSystems/graphql-go/v2"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// hostSchema is a schema a host assembled itself: one field v that
// resolves to val.
func hostSchema(t testing.TB, val string) *graphql.Schema {
	t.Helper()
	s, err := graphql.NewSchema(graphql.SchemaConfig{
		Query: graphql.NewObject(graphql.ObjectConfig{
			Name: "Query",
			Fields: graphql.Fields{"v": &graphql.Field{
				Type:    graphql.String,
				Resolve: func(graphql.ResolveParams) (any, error) { return val, nil },
			}},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &s
}

func queryV(t testing.TB, h http.Handler) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ v }"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func TestSetSchema_NotReadyThenSwap(t *testing.T) {
	g, err := gat.New()
	if err != nil {
		t.Fatal(err)
	}
	h := g.Handler()
	if code, body := queryV(t, h); code != http.StatusServiceUnavailable || !strings.Contains(body, "NOT_READY") {
		t.Fatalf("before a schema: %d %s", code, body)
	}

	if err := g.SetSchema(hostSchema(t, "one"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, body := queryV(t, h); body != `{"data":{"v":"one"}}` {
		t.Fatalf("first schema: %s", body)
	}
	// The handler built before the swap serves the new schema.
	if err := g.SetSchema(hostSchema(t, "two"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, body := queryV(t, h); body != `{"data":{"v":"two"}}` {
		t.Fatalf("after swap: %s", body)
	}
	if g.Schema() == nil || !strings.Contains(g.GraphQLSDL(), "v: String") {
		t.Fatalf("Schema/GraphQLSDL do not follow the swap: %q", g.GraphQLSDL())
	}
	if err := g.SetSchema(nil, nil, nil); err == nil {
		t.Fatal("nil schema accepted")
	}
}

// Requests racing a swap each run against one whole schema.
func TestSetSchema_ConcurrentSwap(t *testing.T) {
	g, err := gat.New()
	if err != nil {
		t.Fatal(err)
	}
	schemas := []*graphql.Schema{hostSchema(t, "one"), hostSchema(t, "two")}
	if err := g.SetSchema(schemas[0], nil, nil); err != nil {
		t.Fatal(err)
	}
	h := g.Handler()
	var readers sync.WaitGroup
	stop, swapped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(swapped)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = g.SetSchema(schemas[i%2], nil, nil)
		}
	}()
	for w := 0; w < 4; w++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 200; i++ {
				_, body := queryV(t, h)
				var out struct {
					Data   map[string]string
					Errors []any
				}
				if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Errors) > 0 ||
					(out.Data["v"] != "one" && out.Data["v"] != "two") {
					t.Errorf("mid-swap response: %s", body)
					return
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	<-swapped
}
