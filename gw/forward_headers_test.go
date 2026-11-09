package gateway

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// ForwardHeaders() with no names forwards nothing; without the option
// Authorization is forwarded.
func TestForwardHeaders_EmptyForwardsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []ServiceOption
		want string
	}{
		{"default", nil, "Bearer caller"},
		{"empty", []ServiceOption{ForwardHeaders()}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got atomic.Value
			got.Store("")
			var hits atomic.Int32
			be := countingBackend(t, &hits, func(w http.ResponseWriter, r *http.Request) {
				got.Store(r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"abc"}`))
			})
			gw := testGateway(t)
			if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), append([]ServiceOption{To(be.URL), As("test")}, tc.opts...)...); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/things/1", nil)
			req.Header.Set("Authorization", "Bearer caller")
			rr := httptest.NewRecorder()
			gw.IngressHandler().ServeHTTP(rr, req)
			if rr.Code != 200 {
				t.Fatalf("status %d %s", rr.Code, rr.Body)
			}
			if g := got.Load().(string); g != tc.want {
				t.Errorf("upstream Authorization %q, want %q", g, tc.want)
			}
		})
	}
}
