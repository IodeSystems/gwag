package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
)

// exchangeServer is a fake RFC 8693 token endpoint that records calls
// and the form values of the most recent one.
type exchangeServer struct {
	srv      *httptest.Server
	calls    atomic.Int64
	lastForm map[string]string
	respJSON string
	status   int
}

func newExchangeServer(t *testing.T) *exchangeServer {
	t.Helper()
	es := &exchangeServer{
		respJSON: `{"access_token":"UPSTREAM-TOKEN","token_type":"Bearer","expires_in":3600}`,
		status:   200,
	}
	es.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		es.calls.Add(1)
		_ = r.ParseForm()
		es.lastForm = map[string]string{}
		for k := range r.Form {
			es.lastForm[k] = r.Form.Get(k)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(es.status)
		_, _ = w.Write([]byte(es.respJSON))
	}))
	t.Cleanup(es.srv.Close)
	return es
}

// outboundWithInboundToken builds an outbound request whose context
// carries an inbound HTTP request bearing tok.
func outboundWithInboundToken(t *testing.T, tok string) *http.Request {
	t.Helper()
	in, _ := http.NewRequest(http.MethodPost, "http://gw/api/graphql", nil)
	if tok != "" {
		in.Header.Set("Authorization", "Bearer "+tok)
	}
	return mustReq(t).WithContext(withHTTPRequest(context.Background(), in))
}

func TestTokenExchange_HappyPath(t *testing.T) {
	es := newExchangeServer(t)
	base := &capturingRT{}
	rt, err := NewTokenExchangeTransport(TokenExchangeConfig{
		TokenURL: es.srv.URL,
		Audience: "billing",
		Scope:    "read",
		Base:     base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RoundTrip(outboundWithInboundToken(t, "CALLER")); err != nil {
		t.Fatal(err)
	}
	if got := base.got.Header.Get("Authorization"); got != "Bearer UPSTREAM-TOKEN" {
		t.Errorf("outbound Authorization = %q", got)
	}
	for k, want := range map[string]string{
		"grant_type":         tokenExchangeGrant,
		"subject_token":      "CALLER",
		"subject_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"audience":           "billing",
		"scope":              "read",
	} {
		if es.lastForm[k] != want {
			t.Errorf("exchange form[%s] = %q, want %q", k, es.lastForm[k], want)
		}
	}
}

func TestTokenExchange_CachesByExpiry(t *testing.T) {
	es := newExchangeServer(t)
	clock := time.Unix(1000, 0)
	rt, _ := NewTokenExchangeTransport(TokenExchangeConfig{
		TokenURL: es.srv.URL,
		Base:     &capturingRT{},
		now:      func() time.Time { return clock },
	})
	for i := 0; i < 3; i++ {
		if _, err := rt.RoundTrip(outboundWithInboundToken(t, "CALLER")); err != nil {
			t.Fatal(err)
		}
	}
	if n := es.calls.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times, want 1 (cached)", n)
	}
	// Advance past expiry (3600s - 30s skew) → re-exchange.
	clock = clock.Add(2 * time.Hour)
	if _, err := rt.RoundTrip(outboundWithInboundToken(t, "CALLER")); err != nil {
		t.Fatal(err)
	}
	if n := es.calls.Load(); n != 2 {
		t.Errorf("after expiry, endpoint hit %d times, want 2", n)
	}
}

func TestTokenExchange_NoInboundToken(t *testing.T) {
	es := newExchangeServer(t)
	rt, _ := NewTokenExchangeTransport(TokenExchangeConfig{TokenURL: es.srv.URL, Base: &capturingRT{}})
	if _, err := rt.RoundTrip(outboundWithInboundToken(t, "")); err == nil {
		t.Fatal("expected error when no inbound token present")
	}
	if es.calls.Load() != 0 {
		t.Error("exchange should not be attempted without an inbound token")
	}
}

func TestTokenExchange_EndpointError(t *testing.T) {
	es := newExchangeServer(t)
	es.status = 401
	es.respJSON = `{"error":"invalid_grant"}`
	rt, _ := NewTokenExchangeTransport(TokenExchangeConfig{TokenURL: es.srv.URL, Base: &capturingRT{}})
	base := rt.cfg.Base.(*capturingRT)
	if _, err := rt.RoundTrip(outboundWithInboundToken(t, "CALLER")); err == nil {
		t.Fatal("expected error on non-200 from token endpoint")
	}
	if base.got != nil {
		t.Error("upstream must not be called when the exchange fails")
	}
}

func TestTokenExchange_InboundFromGRPCMetadata(t *testing.T) {
	es := newExchangeServer(t)
	base := &capturingRT{}
	rt, _ := NewTokenExchangeTransport(TokenExchangeConfig{TokenURL: es.srv.URL, Base: base})
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer GRPC-CALLER"))
	out := mustReq(t).WithContext(ctx)
	if _, err := rt.RoundTrip(out); err != nil {
		t.Fatal(err)
	}
	if es.lastForm["subject_token"] != "GRPC-CALLER" {
		t.Errorf("subject_token = %q, want GRPC-CALLER", es.lastForm["subject_token"])
	}
	if got := base.got.Header.Get("Authorization"); got != "Bearer UPSTREAM-TOKEN" {
		t.Errorf("outbound Authorization = %q", got)
	}
}

func TestTokenExchange_RequiresTokenURL(t *testing.T) {
	if _, err := NewTokenExchangeTransport(TokenExchangeConfig{}); err == nil {
		t.Fatal("expected error for empty TokenURL")
	}
}

func TestTokenExchange_BoundedCache(t *testing.T) {
	es := newExchangeServer(t)
	rt, _ := NewTokenExchangeTransport(TokenExchangeConfig{
		TokenURL:        es.srv.URL,
		Base:            &capturingRT{},
		MaxCachedTokens: 2,
	})
	// Four distinct callers (distinct subject tokens → distinct keys); the
	// cache must stay within the cap rather than growing unbounded.
	for _, sub := range []string{"a", "b", "c", "d"} {
		if _, err := rt.RoundTrip(outboundWithInboundToken(t, sub)); err != nil {
			t.Fatal(err)
		}
	}
	rt.mu.Lock()
	n := len(rt.cache)
	rt.mu.Unlock()
	if n > 2 {
		t.Errorf("cache size = %d, want <= 2 (bounded)", n)
	}
}
