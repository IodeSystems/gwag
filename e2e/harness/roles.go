//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"nhooyr.io/websocket"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
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

// Schema GETs a schema export (path relative to /api, e.g.
// "/schema/graphql") presenting the token in X-Gwag-Token. It returns
// the status, the X-Gwag-Schema-View header ("full" or "public") and
// the body.
func (a *AdminAs) Schema(ctx context.Context, path string) (code int, view, body string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.g.BaseURL()+"/api"+path, nil)
	if err != nil {
		return 0, "", "", err
	}
	if a.token != "" {
		req.Header.Set("X-Gwag-Token", a.token)
	}
	resp, err := a.g.http.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("X-Gwag-Schema-View"), string(b), err
}

// ListServices calls the control plane's ListServices over gRPC with
// the token in `authorization: Bearer` metadata ("" = none), and
// returns the x-gwag-schema-view response header.
func (a *AdminAs) ListServices(ctx context.Context) (*cpv1.ListServicesResponse, string, error) {
	if a.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+a.token)
	}
	var header metadata.MD
	resp, err := a.g.ControlPlane().ListServices(ctx, &cpv1.ListServicesRequest{}, grpc.Header(&header))
	view := ""
	if v := header.Get("x-gwag-schema-view"); len(v) > 0 {
		view = v[0]
	}
	return resp, view, err
}

// WSFirstFrame opens a graphql-transport-ws socket, sends the token as
// `x-gwag-token` in the connection_init payload, subscribes with doc
// and operationName, and returns the first frame after subscribe as
// raw JSON, or "" when none arrives within wait.
func (a *AdminAs) WSFirstFrame(ctx context.Context, doc, operationName string, wait time.Duration) (string, error) {
	conn, _, err := websocket.Dial(ctx, "ws://"+strings.TrimPrefix(a.g.BaseURL(), "http://")+"/api/graphql", &websocket.DialOptions{
		Subprotocols: []string{"graphql-transport-ws"},
	})
	if err != nil {
		return "", err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	init := map[string]string{}
	if a.token != "" {
		init["x-gwag-token"] = a.token
	}
	write := func(f WSFrame) error {
		b, _ := json.Marshal(f)
		return conn.Write(ctx, websocket.MessageText, b)
	}
	payload, _ := json.Marshal(init)
	if err := write(WSFrame{Type: "connection_init", Payload: payload}); err != nil {
		return "", err
	}
	if _, _, err := conn.Read(ctx); err != nil { // connection_ack
		return "", err
	}
	sub, _ := json.Marshal(map[string]any{"query": doc, "operationName": operationName})
	if err := write(WSFrame{ID: "1", Type: "subscribe", Payload: sub}); err != nil {
		return "", err
	}
	rctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	_, b, err := conn.Read(rctx)
	if err != nil && rctx.Err() != nil && ctx.Err() == nil {
		return "", nil
	}
	return string(b), err
}
