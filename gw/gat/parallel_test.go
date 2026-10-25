package gat_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

const slowOp = 100 * time.Millisecond

// newParallelGat registers two slow queries, a slow mutation and a
// panicking query on a gateway with ParallelQueries(limit). An API
// middleware resolves the session through the bag and counts lookups.
func newParallelGat(t *testing.T, limit int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	var lookups atomic.Int32
	api.UseMiddleware(func(hctx huma.Context, next func(huma.Context)) {
		_, _ = gat.LockIfAbsentAndFill(hctx.Context(), sessionKey{}, func() (string, error) {
			lookups.Add(1)
			time.Sleep(10 * time.Millisecond) // long enough for siblings to queue
			return "alice", nil
		})
		next(hctx)
	})
	g := mustNewGat(t).ParallelQueries(limit)
	slow := func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
		time.Sleep(slowOp)
		return &getProjectOutput{Body: project{ID: in.ID}}, nil
	}
	gat.Register(api, g, huma.Operation{OperationID: "opA", Method: http.MethodGet, Path: "/a/{id}"}, slow)
	gat.Register(api, g, huma.Operation{OperationID: "opB", Method: http.MethodGet, Path: "/b/{id}"}, slow)
	gat.Register(api, g, huma.Operation{OperationID: "setA", Method: http.MethodPost, Path: "/a/{id}"}, slow)
	gat.Register(api, g, huma.Operation{OperationID: "boom", Method: http.MethodGet, Path: "/boom/{id}"},
		func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
			panic("handler bug")
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &lookups
}

func timedGraphQL(t *testing.T, url, query string) (map[string]any, time.Duration) {
	t.Helper()
	start := time.Now()
	resp := mustGraphQL(t, url, query)
	return resp, time.Since(start)
}

func TestParallelQueries_FusedQueriesOverlap(t *testing.T) {
	srv, lookups := newParallelGat(t, 8)
	resp, took := timedGraphQL(t, srv.URL+"/api/graphql",
		`query { Demo { a: opA(id: "1") { id } b: opB(id: "2") { id } c: opA(id: "3") { id } } }`)
	for _, k := range []string{"a", "b", "c"} {
		if digPath(resp, "data", "Demo", k, "id") == nil {
			t.Fatalf("missing %s: %v", k, resp)
		}
	}
	// Serial would be 3 × 100ms; parallel ≈ one op plus the session fill.
	if took >= 2*slowOp {
		t.Errorf("3 fused ops took %v, want < %v (parallel)", took, 2*slowOp)
	}
	if n := lookups.Load(); n != 1 {
		t.Errorf("session looked up %d times, want 1 (siblings wait on the fill)", n)
	}
}

func TestParallelQueries_CapRunsTheRestInline(t *testing.T) {
	srv, _ := newParallelGat(t, 1)
	_, took := timedGraphQL(t, srv.URL+"/api/graphql",
		`query { Demo { a: opA(id: "1") { id } b: opA(id: "2") { id } c: opA(id: "3") { id } d: opA(id: "4") { id } } }`)
	// At most one goroutine plus the inline op run at once: a goroutine
	// that finishes frees its slot for a later sibling, so 4 ops take
	// about 2 × slowOp. All four overlapping (≈ 1 × slowOp) means the
	// cap was ignored.
	if took < 2*slowOp {
		t.Errorf("4 ops at limit 1 took %v, want >= %v", took, 2*slowOp)
	}
}

func TestParallelQueries_MutationsStaySerial(t *testing.T) {
	srv, _ := newParallelGat(t, 8)
	_, took := timedGraphQL(t, srv.URL+"/api/graphql",
		`mutation { Demo { a: setA(id: "1") { id } b: setA(id: "2") { id } } }`)
	if took < 2*slowOp {
		t.Errorf("2 mutations took %v, want >= %v (serial)", took, 2*slowOp)
	}
}

func TestParallelQueries_PanicBecomesFieldError(t *testing.T) {
	srv, _ := newParallelGat(t, 8)
	resp := mustGraphQL(t, srv.URL+"/api/graphql",
		`query { Demo { a: opA(id: "1") { id } b: boom(id: "2") { id } } }`)
	if digPath(resp, "data", "Demo", "a", "id") != "1" {
		t.Errorf("sibling of the panicking op lost its data: %v", resp)
	}
	if resp["errors"] == nil {
		t.Errorf("panic produced no error: %v", resp)
	}
}

func TestParallelQueries_OffByDefault(t *testing.T) {
	srv, _ := newParallelGat(t, 0)
	_, took := timedGraphQL(t, srv.URL+"/api/graphql",
		`query { Demo { a: opA(id: "1") { id } b: opB(id: "2") { id } } }`)
	if took < 2*slowOp {
		t.Errorf("default gateway ran 2 ops in %v, want >= %v (serial)", took, 2*slowOp)
	}
}

// BenchmarkParallelQueries_OneOp is the overhead ParallelQueries adds
// to a request that fuses nothing: one goroutine and one result per
// query operation.
func BenchmarkParallelQueries_OneOp(b *testing.B) {
	for _, limit := range []int{0, 8} {
		b.Run(map[int]string{0: "off", 8: "on"}[limit], func(b *testing.B) {
			mux := http.NewServeMux()
			api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
			g, _ := gat.New()
			g.ParallelQueries(limit)
			gat.Register(api, g, huma.Operation{OperationID: "opA", Method: http.MethodGet, Path: "/a/{id}"},
				func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
					return &getProjectOutput{Body: project{ID: in.ID}}, nil
				})
			if err := gat.RegisterHuma(api, g, "/api"); err != nil {
				b.Fatal(err)
			}
			body := `{"query":"query { Demo { opA(id: \"1\") { id } } }"}`
			b.ReportAllocs()
			for b.Loop() {
				req := httptest.NewRequest(http.MethodPost, "/api/graphql", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				mux.ServeHTTP(httptest.NewRecorder(), req)
			}
		})
	}
}
