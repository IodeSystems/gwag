package gat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractSetCookies_ValueAndPointerAndSlices(t *testing.T) {
	type valOut struct {
		SetCookie http.Cookie `header:"Set-Cookie"`
		Body      struct{ Code string }
	}
	type ptrOut struct {
		SetCookie *http.Cookie `header:"Set-Cookie"`
	}
	type sliceOut struct {
		SetCookie []http.Cookie `header:"Set-Cookie"`
	}
	type noneOut struct {
		Other string `header:"X-Other"`
	}

	vo := &valOut{SetCookie: http.Cookie{Name: "token", Value: "abc"}}
	if got := extractSetCookies(vo); len(got) != 1 || got[0].Name != "token" || got[0].Value != "abc" {
		t.Fatalf("value output: %+v", got)
	}

	po := &ptrOut{SetCookie: &http.Cookie{Name: "token", Value: "xyz"}}
	if got := extractSetCookies(po); len(got) != 1 || got[0].Value != "xyz" {
		t.Fatalf("pointer output: %+v", got)
	}
	if got := extractSetCookies(&ptrOut{}); len(got) != 0 {
		t.Fatalf("nil pointer cookie: %+v", got)
	}

	so := &sliceOut{SetCookie: []http.Cookie{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}}}
	if got := extractSetCookies(so); len(got) != 2 || got[1].Name != "b" {
		t.Fatalf("slice output: %+v", got)
	}

	if got := extractSetCookies(&noneOut{Other: "x"}); len(got) != 0 {
		t.Fatalf("non-cookie header field: %+v", got)
	}
}

// TestCookieSink_RoundTrip — the dispatcher path (addFromOutput) feeds the sink
// and emit() writes a real Set-Cookie header.
func TestCookieSink_RoundTrip(t *testing.T) {
	type loginOut struct {
		SetCookie http.Cookie `header:"Set-Cookie"`
		Body      struct{ Code string }
	}
	ctx, sink := withCookieSink(context.Background())
	if cookieSinkFrom(ctx) != sink {
		t.Fatal("sink not retrievable from ctx")
	}
	out := &loginOut{SetCookie: http.Cookie{Name: "token", Value: "sess", Path: "/", HttpOnly: true}}
	sink.addFromOutput(out)

	rec := httptest.NewRecorder()
	sink.emit(rec)
	got := rec.Result().Cookies()
	if len(got) != 1 || got[0].Name != "token" || got[0].Value != "sess" {
		t.Fatalf("emitted cookies = %+v", got)
	}
	if !got[0].HttpOnly {
		t.Errorf("HttpOnly not preserved")
	}
}

// TestCookieSink_EmptyCookieSkipped — an output whose cookie was never set
// (zero value, empty name) writes no header.
func TestCookieSink_EmptyCookieSkipped(t *testing.T) {
	type out struct {
		SetCookie http.Cookie `header:"Set-Cookie"`
	}
	_, sink := withCookieSink(context.Background())
	sink.addFromOutput(&out{})
	rec := httptest.NewRecorder()
	sink.emit(rec)
	if h := rec.Header().Get("Set-Cookie"); h != "" {
		t.Fatalf("expected no Set-Cookie, got %q", h)
	}
}
