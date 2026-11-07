//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// DefaultCaller is the X-Caller-Id the clients send unless a request
// sets its own with Caller. It shows up as the caller label in
// dispatch metrics and as an external caller in the admin graph.
const DefaultCaller = "e2e"

// ReqOpt adjusts one HTTP request.
type ReqOpt func(*http.Request)

// Caller sets X-Caller-Id, e.g. "orders/v1".
func Caller(id string) ReqOpt { return func(r *http.Request) { r.Header.Set("X-Caller-Id", id) } }

// Header sets one request header.
func Header(k, v string) ReqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// Bearer sets Authorization: Bearer <token>.
func Bearer(token string) ReqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

// HTTPError is a non-2xx answer.
type HTTPError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

// do sends a JSON request to the gateway. body nil = no body; out nil
// = discard. A non-2xx status returns *HTTPError (out is not filled).
func (g *Gateway) do(ctx context.Context, method, path string, body, out any, opts []ReqOpt) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.BaseURL()+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Caller-Id", DefaultCaller)
	for _, o := range opts {
		o(req)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, &HTTPError{Method: method, Path: path, Status: resp.StatusCode, Body: string(b)}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: decode %q: %w", method, path, b, err)
		}
	}
	return resp.StatusCode, nil
}

// Post sends a JSON POST to path (e.g. "/api/ingress/...") and decodes
// a 2xx JSON answer into out.
func (g *Gateway) Post(ctx context.Context, path string, body, out any, opts ...ReqOpt) error {
	_, err := g.do(ctx, http.MethodPost, path, body, out, opts)
	return err
}

// Get sends a GET to path and decodes a 2xx JSON answer into out.
func (g *Gateway) Get(ctx context.Context, path string, out any, opts ...ReqOpt) error {
	_, err := g.do(ctx, http.MethodGet, path, nil, out, opts)
	return err
}

// ---- GraphQL --------------------------------------------------------

// GQLError is one entry of a GraphQL errors array.
type GQLError struct {
	Message    string         `json:"message"`
	Path       []any          `json:"path,omitempty"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

// GQLResponse is a GraphQL-over-HTTP answer.
type GQLResponse struct {
	Status int
	Data   json.RawMessage `json:"data"`
	Errors []GQLError      `json:"errors"`
}

// Err returns the GraphQL errors as one error, or nil.
func (r *GQLResponse) Err() error {
	if len(r.Errors) == 0 {
		return nil
	}
	msgs := make([]string, len(r.Errors))
	for i, e := range r.Errors {
		msgs[i] = e.Message
		if len(e.Path) > 0 {
			msgs[i] += fmt.Sprintf(" (path %v)", e.Path)
		}
	}
	return fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
}

// Decode unmarshals data into v.
func (r *GQLResponse) Decode(v any) error { return json.Unmarshal(r.Data, v) }

// Path returns the value at a dotted path in data, e.g.
// "pfx.v1.echo.msg"; nil when absent.
func (r *GQLResponse) Path(path string) any {
	var v any
	if json.Unmarshal(r.Data, &v) != nil {
		return nil
	}
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// GraphQL posts a query to /api/graphql. The error is a transport or
// HTTP failure only; GraphQL errors are in the response (see Err).
func (g *Gateway) GraphQL(ctx context.Context, query string, vars map[string]any, opts ...ReqOpt) (*GQLResponse, error) {
	body := map[string]any{"query": query}
	if vars != nil {
		body["variables"] = vars
	}
	var out GQLResponse
	status, err := g.do(ctx, http.MethodPost, "/api/graphql", body, &out, opts)
	out.Status = status
	if he, ok := err.(*HTTPError); ok {
		// A GraphQL error answer can carry a 4xx/5xx status; keep its
		// errors array when the body is one.
		if json.Unmarshal([]byte(he.Body), &out) == nil && len(out.Errors) > 0 {
			return &out, nil
		}
	}
	return &out, err
}

// MustGraphQL runs GraphQL and fails the test on a transport error or
// any GraphQL error.
func (g *Gateway) MustGraphQL(t testing.TB, query string, vars map[string]any, opts ...ReqOpt) *GQLResponse {
	t.Helper()
	resp, err := g.GraphQL(Ctx(t, defaultCallTimeout), query, vars, opts...)
	if err != nil {
		t.Fatalf("graphql %s: %v", oneLine(query), err)
	}
	if err := resp.Err(); err != nil {
		t.Fatalf("graphql %s: %v", oneLine(query), err)
	}
	return resp
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}
