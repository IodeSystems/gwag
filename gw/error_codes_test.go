package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type gqlErrorBody struct {
	Errors []struct {
		Message    string         `json:"message"`
		Extensions map[string]any `json:"extensions"`
	} `json:"errors"`
}

func postGraphQLQuery(t *testing.T, h http.Handler, query string) gqlErrorBody {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"query": query})
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var body gqlErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if len(body.Errors) == 0 {
		t.Fatalf("no errors in %s", rr.Body.String())
	}
	return body
}

// An upstream error status reaches GraphQL as Reject's upper-case Code
// by default, and as gat's {status, code} shape (lower-case Connect
// code, the one the gRPC and REST ingresses use) under
// WithConnectErrorCodes.
func TestConnectErrorCodes_UpstreamStatus(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var code int
		_, _ = fmt.Sscanf(r.URL.Path, "/things/%d", &code)
		w.WriteHeader(code)
	}))
	defer backend.Close()
	for _, tc := range []struct {
		name     string
		opts     []Option
		upstream int
		want     map[string]any
	}{
		{"default 404", nil, 404, map[string]any{"code": "NOT_FOUND"}},
		{"default 409", nil, 409, map[string]any{"code": "INVALID_ARGUMENT"}},
		{"connect 404", []Option{WithConnectErrorCodes()}, 404, map[string]any{"code": "not_found", "status": float64(404)}},
		{"connect 409", []Option{WithConnectErrorCodes()}, 409, map[string]any{"code": "already_exists", "status": float64(409)}},
		{"connect 503", []Option{WithConnectErrorCodes()}, 503, map[string]any{"code": "unavailable", "status": float64(503)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := New(append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test"))}, tc.opts...)...)
			defer gw.Close()
			if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(backend.URL), As("test")); err != nil {
				t.Fatal(err)
			}
			// Both resolver paths: id + name (append) and __typename.
			for _, q := range []string{
				fmt.Sprintf(`{ test { getThing(id:"%d") { id name } } }`, tc.upstream),
				fmt.Sprintf(`{ test { getThing(id:"%d") { __typename id } } }`, tc.upstream),
			} {
				got := postGraphQLQuery(t, gw.Handler(), q).Errors[0].Extensions
				if fmt.Sprint(got) != fmt.Sprint(tc.want) {
					t.Errorf("%s: extensions %v, want %v", q, got, tc.want)
				}
			}
		})
	}
}

// Pub/sub auth failures carry PERMISSION_DENIED (was no code), lower
// case under WithConnectErrorCodes.
func TestConnectErrorCodes_ChannelAuth(t *testing.T) {
	cluster, err := StartCluster(ClusterOptions{
		NodeName:      "errcodes-test",
		ClientListen:  "127.0.0.1:0",
		ClusterListen: "127.0.0.1:0",
		DataDir:       t.TempDir(),
		StartTimeout:  10 * time.Second,
		LogLevel:      "silent",
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cluster.Close)
	for _, tc := range []struct {
		name string
		opts []Option
		want map[string]any
	}{
		{"default", nil, map[string]any{"code": "PERMISSION_DENIED"}},
		{"connect", []Option{WithConnectErrorCodes()}, map[string]any{"code": "permission_denied", "status": float64(403)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := New(append([]Option{
				WithCluster(cluster),
				WithoutMetrics(),
				WithoutBackpressure(),
				WithSubscriptionAuth(SubscriptionAuthOptions{Secret: []byte("channel-secret-32-bytes-of-padding!!")}),
				WithChannelAuth("events.>", ChannelAuthHMAC),
			}, tc.opts...)...)
			defer gw.Close()
			gw.mu.Lock()
			if err := gw.assembleLocked(); err != nil {
				gw.mu.Unlock()
				t.Fatal(err)
			}
			gw.mu.Unlock()
			body := postGraphQLQuery(t, gw.Handler(), `{ ps { pub(channel:"events.orders.1", payload:"x") { __typename } } }`)
			e := body.Errors[0]
			if !strings.Contains(e.Message, "ps: hmac required") {
				t.Errorf("message %q", e.Message)
			}
			if fmt.Sprint(e.Extensions) != fmt.Sprint(tc.want) {
				t.Errorf("extensions %v, want %v", e.Extensions, tc.want)
			}
		})
	}
}
