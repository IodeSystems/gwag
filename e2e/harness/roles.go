//go:build e2e

package harness

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Admin roles: anonymous (no bearer), dev (DevToken), admin (Token).

// DevToken returns the gateway's dev bearer token, read from
// <DataDir>/dev-token (it changes when an admin rotates it).
func (g *Gateway) DevToken() string {
	b, err := os.ReadFile(filepath.Join(g.DataDir, "dev-token"))
	if err != nil {
		g.env.t.Fatalf("harness: %s dev token: %v", g.Name, err)
	}
	return strings.TrimSpace(string(b))
}

// AdminAs is the admin surface as one role: token "" is anonymous,
// otherwise the bearer sent.
type AdminAs struct {
	g     *Gateway
	token string
}

// As returns the admin client that presents token ("" = none).
func (a *Admin) As(token string) *AdminAs { return &AdminAs{g: a.g, token: token} }

// Do sends an admin request (path relative to /api, e.g.
// "/admin/services") and returns the HTTP status. A non-2xx status is
// not an error here; out is decoded only on 2xx.
func (a *AdminAs) Do(ctx context.Context, method, path string, body, out any) (int, error) {
	var opts []ReqOpt
	if a.token != "" {
		opts = append(opts, Bearer(a.token))
	}
	code, err := a.g.do(ctx, method, "/api"+path, body, out, opts)
	if _, isHTTP := err.(*HTTPError); isHTTP {
		err = nil
	}
	return code, err
}

// Whoami is GET /admin/whoami: "anonymous", "dev" or "admin".
func (a *AdminAs) Whoami(ctx context.Context) (string, error) {
	var out struct {
		Role string `json:"role"`
	}
	_, err := a.Do(ctx, http.MethodGet, "/admin/whoami", nil, &out)
	return out.Role, err
}

// Services is GET /admin/services as this role.
func (a *AdminAs) Services(ctx context.Context) (*Services, int, error) {
	var out Services
	code, err := a.Do(ctx, http.MethodGet, "/admin/services", nil, &out)
	return &out, code, err
}

// Graph is GET /admin/graph as this role.
func (a *AdminAs) Graph(ctx context.Context, window string) (*Graph, int, error) {
	path := "/admin/graph"
	if window != "" {
		path += "?window=" + url.QueryEscape(window)
	}
	var out Graph
	code, err := a.Do(ctx, http.MethodGet, path, nil, &out)
	return &out, code, err
}

// GraphQL runs query on /api/graphql as this role.
func (a *AdminAs) GraphQL(ctx context.Context, query string) (*GQLResponse, error) {
	var opts []ReqOpt
	if a.token != "" {
		opts = append(opts, Bearer(a.token))
	}
	return a.g.GraphQL(ctx, query, nil, opts...)
}
