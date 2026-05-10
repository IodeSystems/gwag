package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// requestLogBuffer is a thread-safe sink for the per-request JSON
// lines so the test can read the captured output after the
// gateway-served goroutine writes to it.
type requestLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *requestLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *requestLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRequestLog_GraphQLEmitsOnePerRequest covers the WithRequestLog
// option end-to-end through gw.Handler(): one JSON line per request
// with ingress=graphql, populated total/self/dispatch_count fields.
// Uses an openapi backend so dispatch_count > 0 — the request hits
// the backend through a synthesised dispatcher.
func TestRequestLog_GraphQLEmitsOnePerRequest(t *testing.T) {
	sink := &requestLogBuffer{}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(backend.Close)

	gw := New(
		WithoutMetrics(),
		WithoutBackpressure(),
		WithAdminToken([]byte("test-token")),
		WithRequestLog(sink),
	)
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(backend.URL), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/graphql",
		strings.NewReader(`{"query":"{ test { getThing(id:\"42\") { id name } } }"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	out := sink.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("want 1 log line, got %d: %q", len(lines), out)
	}

	var line requestLogLine
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatalf("decode log line: %v\nline: %s", err, lines[0])
	}
	if line.Ingress != "graphql" {
		t.Errorf("ingress=%q want graphql", line.Ingress)
	}
	if line.Path != "/graphql" {
		t.Errorf("path=%q want /graphql", line.Path)
	}
	if line.TotalUS <= 0 {
		t.Errorf("total_us=%d want > 0", line.TotalUS)
	}
	if line.SelfUS < 0 || line.SelfUS > line.TotalUS {
		t.Errorf("self_us=%d outside [0, total_us=%d]", line.SelfUS, line.TotalUS)
	}
	if line.DispatchCount != 1 {
		t.Errorf("dispatch_count=%d want 1 (one openapi call)", line.DispatchCount)
	}
	if line.TS == "" {
		t.Errorf("ts unset")
	}
}

// TestRequestLog_NotAutoEnabled confirms the default config doesn't
// produce any log output — operators have to opt in via
// WithRequestLog.
func TestRequestLog_NotAutoEnabled(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(gw.Close)

	if gw.cfg.requestLog != nil {
		t.Fatal("requestLog set without WithRequestLog")
	}

	// Hitting Handler() with no backend / no register still exercises
	// the request-log codepath. logRequestLine should be a no-op.
	req := httptest.NewRequest(http.MethodPost, "/graphql",
		strings.NewReader(`{"query":"{ __typename }"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rr, req)
	// No assertion on output; the contract is: no log writer = no
	// allocation past the cfg.requestLog == nil short-circuit. Test
	// passes by returning.
	_ = rr
}
