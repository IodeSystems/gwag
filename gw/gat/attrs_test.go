package gat_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

type sessionKey struct{}
type opCountKey struct{}

// attrsCtx returns a context carrying a fresh bag, the way
// AttrsMiddleware scopes a request.
func attrsCtx(t *testing.T) context.Context {
	t.Helper()
	var ctx context.Context
	gat.AttrsMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctx = r.Context()
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	return ctx
}

func TestAttrs_LockIfAbsentAndFill_FillsOnceUnderContention(t *testing.T) {
	ctx := attrsCtx(t)
	var fills atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	got := make([]string, 20)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := gat.LockIfAbsentAndFill(ctx, sessionKey{}, func() (string, error) {
				fills.Add(1)
				<-release // hold the key so the others queue up behind it
				return "alice", nil
			})
			if err != nil {
				t.Errorf("fill: %v", err)
			}
			got[i] = v
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := fills.Load(); n != 1 {
		t.Errorf("fill ran %d times, want 1", n)
	}
	for i, v := range got {
		if v != "alice" {
			t.Errorf("caller %d got %q", i, v)
		}
	}
}

func TestAttrs_ErrorIsKeptForTheRequest(t *testing.T) {
	ctx := attrsCtx(t)
	errNoSession := errors.New("no session")
	var fills int
	for range 3 {
		_, err := gat.LockIfAbsentAndFill(ctx, sessionKey{}, func() (string, error) {
			fills++
			return "", errNoSession
		})
		if !errors.Is(err, errNoSession) {
			t.Errorf("err = %v, want %v", err, errNoSession)
		}
	}
	if fills != 1 {
		t.Errorf("fill ran %d times, want 1", fills)
	}
	if _, ok := gat.AttrsFrom(ctx).Get(sessionKey{}); ok {
		t.Error("Get reported a failed fill as present")
	}
}

func TestAttrs_PanickingFillReleasesWaiters(t *testing.T) {
	ctx := attrsCtx(t)
	started := make(chan struct{})
	go func() {
		defer func() { _ = recover() }()
		_, _ = gat.LockIfAbsentAndFill(ctx, sessionKey{}, func() (string, error) {
			close(started)
			time.Sleep(20 * time.Millisecond)
			panic("boom")
		})
	}()
	<-started
	done := make(chan error, 1)
	go func() {
		_, err := gat.LockIfAbsentAndFill(ctx, sessionKey{}, func() (string, error) { return "unused", nil })
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("waiter got no error from a panicked fill")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter still blocked after the fill panicked")
	}
}

func TestAttrs_WaiterHonoursItsContext(t *testing.T) {
	ctx := attrsCtx(t)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	go func() {
		_, _ = gat.LockIfAbsentAndFill(ctx, sessionKey{}, func() (string, error) {
			close(started)
			<-release
			return "alice", nil
		})
	}()
	<-started
	wctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err := gat.LockIfAbsentAndFill(wctx, sessionKey{}, func() (string, error) { return "unused", nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestAttrs_PutIfAbsentAndTypeMismatch(t *testing.T) {
	ctx := attrsCtx(t)
	a := gat.AttrsFrom(ctx)
	if v, loaded := a.PutIfAbsent(sessionKey{}, "alice"); loaded || v != "alice" {
		t.Errorf("first put = %v, %v", v, loaded)
	}
	if v, loaded := a.PutIfAbsent(sessionKey{}, "bob"); !loaded || v != "alice" {
		t.Errorf("second put = %v, %v; want alice, true", v, loaded)
	}
	if v, ok := a.Get(sessionKey{}); !ok || v != "alice" {
		t.Errorf("Get = %v, %v", v, ok)
	}
	if _, err := gat.LockIfAbsentAndFill(ctx, sessionKey{}, func() (int, error) { return 1, nil }); err == nil {
		t.Error("string stored, int requested: want an error")
	}
}

func TestAttrs_NoBagFillsEveryTime(t *testing.T) {
	ctx := context.Background()
	if gat.AttrsFrom(ctx) != nil {
		t.Fatal("bag in a bare context")
	}
	var fills int
	for range 2 {
		if _, err := gat.LockIfAbsentAndFill(ctx, sessionKey{}, func() (string, error) {
			fills++
			return "alice", nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if fills != 2 {
		t.Errorf("fill ran %d times, want 2 (nothing to share without a bag)", fills)
	}
}

// newAttrsGat registers two ops behind an API middleware that resolves
// the session through the bag and counts operations — the adopter
// shape: one lookup and one counter per request, however many ops it
// fuses.
func newAttrsGat(t *testing.T) (*httptest.Server, *atomic.Int32, chan int64) {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	var lookups atomic.Int32
	counts := make(chan int64, 16)
	api.UseMiddleware(func(hctx huma.Context, next func(huma.Context)) {
		ctx := hctx.Context()
		if _, err := gat.LockIfAbsentAndFill(ctx, sessionKey{}, func() (string, error) {
			lookups.Add(1)
			return "alice", nil
		}); err != nil {
			_ = huma.WriteErr(api, hctx, http.StatusUnauthorized, err.Error())
			return
		}
		n, _ := gat.LockIfAbsentAndFill(ctx, opCountKey{}, func() (*atomic.Int64, error) {
			return new(atomic.Int64), nil
		})
		counts <- n.Add(1)
		next(hctx)
	})
	g := mustNewGat(t)
	for _, id := range []string{"opA", "opB"} {
		gat.Register(api, g, huma.Operation{OperationID: id, Method: http.MethodGet, Path: "/" + id + "/{id}"},
			func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
				user, _ := gat.AttrsFrom(ctx).Get(sessionKey{})
				name, _ := user.(string)
				return &getProjectOutput{Body: project{ID: in.ID, Name: name}}, nil
			})
	}
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	srv := httptest.NewServer(gat.AttrsMiddleware(mux))
	t.Cleanup(srv.Close)
	return srv, &lookups, counts
}

func TestAttrs_FusedGraphQLSharesOneBag(t *testing.T) {
	srv, lookups, counts := newAttrsGat(t)
	resp := mustGraphQL(t, srv.URL+"/api/graphql",
		`query { Demo { a: opA(id: "1") { name } b: opB(id: "2") { name } c: opA(id: "3") { name } } }`)
	for _, alias := range []string{"a", "b", "c"} {
		if got := digPath(resp, "data", "Demo", alias, "name"); got != "alice" {
			t.Errorf("%s.name = %v, want alice (handler read the session from the bag)", alias, got)
		}
	}
	if n := lookups.Load(); n != 1 {
		t.Errorf("session looked up %d times for 3 fused ops, want 1", n)
	}
	var last int64
	for len(counts) > 0 {
		last = <-counts
	}
	if last != 3 {
		t.Errorf("op counter reached %d, want 3", last)
	}

	// A second request gets a fresh bag.
	mustGraphQL(t, srv.URL+"/api/graphql", `query { Demo { opA(id: "1") { name } } }`)
	if n := lookups.Load(); n != 2 {
		t.Errorf("lookups after a second request = %d, want 2", n)
	}
}

func TestAttrs_RESTThroughAttrsMiddleware(t *testing.T) {
	srv, lookups, _ := newAttrsGat(t)
	body := mustGET(t, srv.URL+"/opA/p1")
	if want := `"name":"alice"`; !strings.Contains(string(body), want) {
		t.Errorf("body = %s, want %s (middleware and handler share the bag)", body, want)
	}
	if n := lookups.Load(); n != 1 {
		t.Errorf("lookups = %d, want 1", n)
	}
}
