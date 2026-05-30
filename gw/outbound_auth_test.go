package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// capturingRT records the request it receives and returns an empty 200.
type capturingRT struct{ got *http.Request }

func (c *capturingRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.got = r
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}, nil
}

func mustReq(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "http://up/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestServiceAccountTransport_DefaultBearer(t *testing.T) {
	base := &capturingRT{}
	rt := ServiceAccountTransport{Token: StaticToken("abc123"), Base: base}
	if _, err := rt.RoundTrip(mustReq(t)); err != nil {
		t.Fatal(err)
	}
	if got := base.got.Header.Get("Authorization"); got != "Bearer abc123" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer abc123")
	}
}

func TestServiceAccountTransport_CustomHeaderRaw(t *testing.T) {
	base := &capturingRT{}
	rt := ServiceAccountTransport{Token: StaticToken("k"), Header: "X-Api-Key", Base: base}
	if _, err := rt.RoundTrip(mustReq(t)); err != nil {
		t.Fatal(err)
	}
	// Custom header → raw token, no scheme prefix.
	if got := base.got.Header.Get("X-Api-Key"); got != "k" {
		t.Errorf("X-Api-Key = %q, want %q", got, "k")
	}
}

func TestServiceAccountTransport_CustomScheme(t *testing.T) {
	base := &capturingRT{}
	rt := ServiceAccountTransport{Token: StaticToken("t"), Scheme: "Token", Base: base}
	if _, err := rt.RoundTrip(mustReq(t)); err != nil {
		t.Fatal(err)
	}
	if got := base.got.Header.Get("Authorization"); got != "Token t" {
		t.Errorf("Authorization = %q, want %q", got, "Token t")
	}
}

func TestServiceAccountTransport_TokenError(t *testing.T) {
	base := &capturingRT{}
	src := func(context.Context) (string, error) { return "", errors.New("mint failed") }
	rt := ServiceAccountTransport{Token: src, Base: base}
	if _, err := rt.RoundTrip(mustReq(t)); err == nil {
		t.Fatal("expected error from failing TokenSource")
	}
	if base.got != nil {
		t.Error("base transport should not be called when token mint fails")
	}
}

func TestServiceAccountTransport_NilTokenSource(t *testing.T) {
	if _, err := (ServiceAccountTransport{}).RoundTrip(mustReq(t)); err == nil {
		t.Fatal("expected error for nil TokenSource")
	}
}

func TestServiceAccountTransport_DoesNotMutateOriginal(t *testing.T) {
	base := &capturingRT{}
	rt := ServiceAccountTransport{Token: StaticToken("x"), Base: base}
	orig := mustReq(t)
	if _, err := rt.RoundTrip(orig); err != nil {
		t.Fatal(err)
	}
	if orig.Header.Get("Authorization") != "" {
		t.Error("original request was mutated; RoundTripper must clone")
	}
}

func TestServiceAccountClient(t *testing.T) {
	c := ServiceAccountClient(StaticToken("y"))
	rt, ok := c.Transport.(ServiceAccountTransport)
	if !ok {
		t.Fatalf("Transport type = %T", c.Transport)
	}
	tok, _ := rt.Token(context.Background())
	if tok != "y" {
		t.Errorf("token = %q", tok)
	}
}

// TestForwardTraceHeaders verifies trace-propagation headers ride along
// with the auth allowlist, and that an explicit empty allowlist opts out.
func TestForwardTraceHeaders(t *testing.T) {
	in, _ := http.NewRequest(http.MethodPost, "http://gw/api/graphql", nil)
	in.Header.Set("Authorization", "Bearer caller")
	in.Header.Set("traceparent", "00-abc-def-01")
	in.Header.Set("x-request-id", "req-7")
	in.Header.Set("x-b3-traceid", "b3trace")
	in.Header.Set("X-Secret", "nope") // not in any list → must not forward
	ctx := withHTTPRequest(context.Background(), in)

	t.Run("default-allowlist-carries-trace", func(t *testing.T) {
		out := mustReq(t)
		forwardOpenAPIHeaders(ctx, out, nil)
		for h, want := range map[string]string{
			"Authorization": "Bearer caller",
			"Traceparent":   "00-abc-def-01",
			"X-Request-Id":  "req-7",
			"X-B3-Traceid":  "b3trace",
		} {
			if got := out.Header.Get(h); got != want {
				t.Errorf("%s = %q, want %q", h, got, want)
			}
		}
		if out.Header.Get("X-Secret") != "" {
			t.Error("non-allowlisted header leaked")
		}
	})

	t.Run("empty-allowlist-opts-out", func(t *testing.T) {
		out := mustReq(t)
		forwardOpenAPIHeaders(ctx, out, []string{})
		if out.Header.Get("traceparent") != "" || out.Header.Get("Authorization") != "" {
			t.Error("empty allowlist must forward nothing, trace headers included")
		}
	})
}
