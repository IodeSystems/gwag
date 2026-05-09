package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPprofMux_NilWhenDisabled(t *testing.T) {
	g := New(WithoutMetrics())
	defer g.Close()
	if mux := g.PprofMux(); mux != nil {
		t.Fatalf("PprofMux without WithPprof: got %v, want nil", mux)
	}
}

func TestPprofMux_ServesIndexWhenEnabled(t *testing.T) {
	g := New(WithPprof(), WithoutMetrics())
	defer g.Close()
	mux := g.PprofMux()
	if mux == nil {
		t.Fatal("PprofMux returned nil with WithPprof set")
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/debug/pprof/ status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// pprof.Index renders an HTML table listing the named profiles —
	// "goroutine" is always present, so the body is a stable smoke
	// signal that we routed to the real handler rather than 404.
	if !strings.Contains(rr.Body.String(), "goroutine") {
		t.Fatalf("/debug/pprof/ body missing 'goroutine' link; got: %s", rr.Body.String())
	}
}

func TestPprofMux_NamedProfileRoutes(t *testing.T) {
	g := New(WithPprof(), WithoutMetrics())
	defer g.Close()
	mux := g.PprofMux()
	// /debug/pprof/heap is dispatched by pprof.Index via the catch-all
	// /debug/pprof/ route. Hit it to confirm the prefix mount works.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap?debug=1", nil)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/debug/pprof/heap status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}
