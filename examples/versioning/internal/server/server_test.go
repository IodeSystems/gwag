package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/gw/contract"
)

func newService(t *testing.T, o Options) *Service {
	t.Helper()
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The v1 adapter answers the requests recorded from the legacy service
// (cmd/legacy -record) the way the legacy service did.
func TestV1Contract(t *testing.T) {
	contract.Run(t, newService(t, Options{}).Handler, "../../testdata/contract/v1")
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Body)
	return w.Code, string(bytes.TrimSpace(b))
}

// Data v1 cannot express: a EUR order is a 409 naming v2 on get and
// left out of the list; a refunded order shows as closed.
func TestV1Lossy(t *testing.T) {
	h := newService(t, Options{}).Handler
	if code, body := do(t, h, "GET", "/v1/orders/o-3", ""); code != 409 || !strings.Contains(body, "use v2") {
		t.Errorf("v1 get o-3: %d %s", code, body)
	}
	if _, body := do(t, h, "GET", "/v1/orders", ""); strings.Contains(body, "o-3") {
		t.Errorf("v1 list has o-3: %s", body)
	}
	if code, body := do(t, h, "POST", "/v2/orders/o-1/refund", `{"amountCents":1250}`); code != 200 || !strings.Contains(body, `"status":"refunded"`) {
		t.Fatalf("v2 refund: %d %s", code, body)
	}
	if _, body := do(t, h, "GET", "/v1/orders/o-1", ""); !strings.Contains(body, `"status":"closed"`) {
		t.Errorf("v1 get of a refunded order: %s", body)
	}
}

// After the sunset date applyDiscount answers 410 with the reason.
func TestDiscountSunset(t *testing.T) {
	sunset := time.Date(2027, 6, 30, 0, 0, 0, 0, time.UTC)
	now := sunset.Add(-time.Second)
	h := newService(t, Options{DiscountSunset: sunset, Now: func() time.Time { return now }}).Handler
	if code, body := do(t, h, "POST", "/v1/orders/o-1/discount", `{"percent":10}`); code != 200 {
		t.Fatalf("before sunset: %d %s", code, body)
	}
	now = sunset
	code, body := do(t, h, "POST", "/v1/orders/o-1/discount", `{"percent":10}`)
	if code != 410 || !strings.Contains(body, "applyDiscount was removed on 2027-06-30") {
		t.Errorf("at sunset: %d %s", code, body)
	}
}

// One GraphQL schema serves both versions over one store.
func TestGraphQLBothVersions(t *testing.T) {
	h := newService(t, Options{}).Handler
	q := `{"query":"{ orders { v1 { getOrder(id: \"o-1\") { amount customerName } } getOrder(id: \"o-1\") { amountCents currency customer } } }"}`
	code, body := do(t, h, "POST", "/api/graphql", q)
	want := `{"data":{"orders":{"v1":{"getOrder":{"amount":12.5,"customerName":"Ada Lovelace"}},"getOrder":{"amountCents":1250,"currency":"USD","customer":"Ada Lovelace"}}}}`
	if code != 200 || body != want {
		t.Errorf("got %d %s\nwant %s", code, body, want)
	}
}

// Built without v1 (after v1 is deleted), only v2 is served.
func TestNoV1(t *testing.T) {
	s := newService(t, Options{NoV1: true})
	if code, _ := do(t, s.Handler, "GET", "/v1/orders/o-1", ""); code != 404 {
		t.Errorf("v1 route without v1: %d", code)
	}
	if _, err := s.Gat.OpenAPISpec("v1"); err == nil {
		t.Error("OpenAPISpec(v1) without v1: no error")
	}
}
