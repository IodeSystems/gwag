package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// Admin roles. Admin auth only: none of this touches calls to
// services through the gateway.
//
//   - anonymous: no bearer. A few reads (adminAnonymousOps), answered
//     with public services only (adminView).
//   - dev: the dev token, or a delegate granting ADMIN_ROLE_DEV. Every
//     admin read, unfiltered, plus the external-dependent writes.
//   - admin: the boot token, or a delegate OK. Everything.
type adminRole int

const (
	adminRoleAnonymous adminRole = iota
	adminRoleDev
	adminRoleAdmin
)

func (r adminRole) String() string {
	switch r {
	case adminRoleAdmin:
		return "admin"
	case adminRoleDev:
		return "dev"
	}
	return "anonymous"
}

type adminRoleCtxKey struct{}

func withAdminRole(ctx context.Context, r adminRole) context.Context {
	return context.WithValue(ctx, adminRoleCtxKey{}, r)
}

// adminRoleFromContext returns the role AdminMiddleware resolved; ok
// is false when the request did not pass through it.
func adminRoleFromContext(ctx context.Context) (adminRole, bool) {
	r, ok := ctx.Value(adminRoleCtxKey{}).(adminRole)
	return r, ok
}

// adminFullView reports whether the admin handlers answer ctx with
// every service (dev and admin) or with public services only
// (anonymous). A request that did not pass AdminMiddleware gets the
// full view: the admin mux has no auth of its own and must be mounted
// behind AdminMiddleware.
func adminFullView(ctx context.Context) bool {
	r, ok := adminRoleFromContext(ctx)
	return !ok || r >= adminRoleDev
}

// adminAnonymousOps are the admin operations open to callers with no
// token. Each answers with public services only.
var adminAnonymousOps = map[string]bool{
	"whoami":          true,
	"listServices":    true,
	"servicesStats":   true,
	"servicesHistory": true,
	"serviceStats":    true,
	"deprecatedStats": true,
	"usageEdges":      true,
	"dependencyGraph": true,
}

// adminDevWriteOps are the non-GET admin operations the dev role may
// call: the external-dependent declarations (put, review, delete), and the MCP schema reads
// that take a POST body.
var adminDevWriteOps = map[string]bool{
	"putExternalDependent":    true,
	"deleteExternalDependent": true,
	"reviewExternalDependent": true,
	"mcpSchemaSearch":         true,
	"mcpSchemaExpand":         true,
}

// adminOpNeed is the role an admin operation needs. Operations not
// listed above: GET needs dev, anything else admin — so a new route
// is closed until someone opens it on purpose.
func adminOpNeed(op *huma.Operation) adminRole {
	switch {
	case adminAnonymousOps[op.OperationID]:
		return adminRoleAnonymous
	case adminDevWriteOps[op.OperationID], op.Method == http.MethodGet:
		return adminRoleDev
	}
	return adminRoleAdmin
}

// adminRouteTable maps "METHOD /path/{param}" patterns of the routes
// AdminHumaRouter registered to the role each needs. The mux only
// matches patterns; it never serves.
type adminRouteTable struct {
	mux  *http.ServeMux
	need map[string]adminRole
}

// indexAdminRoutes records the role of every operation in the admin
// OpenAPI document. Called by AdminHumaRouter; a later call replaces
// the table.
func (g *Gateway) indexAdminRoutes(oapi *huma.OpenAPI) {
	t := &adminRouteTable{mux: http.NewServeMux(), need: map[string]adminRole{}}
	noop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for path, item := range oapi.Paths {
		for method, op := range map[string]*huma.Operation{
			http.MethodGet: item.Get, http.MethodPut: item.Put, http.MethodPost: item.Post,
			http.MethodDelete: item.Delete, http.MethodPatch: item.Patch,
		} {
			if op == nil {
				continue
			}
			pattern := method + " " + path
			t.mux.Handle(pattern, noop)
			t.need[pattern] = adminOpNeed(op)
		}
	}
	g.adminRoutes.Store(t)
}

// adminRouteNeed is the role r needs (see AdminMiddleware).
func (g *Gateway) adminRouteNeed(r *http.Request) adminRole {
	need := adminRoleAdmin
	if isAdminPublicMethod(r.Method) {
		need = adminRoleAnonymous
	}
	if t := g.adminRoutes.Load(); t != nil {
		if _, pattern := t.mux.Handler(r); pattern != "" {
			if n, ok := t.need[pattern]; ok {
				need = n
			}
		}
	}
	if g.isDestructiveRead(r.URL.Path) {
		need = adminRoleAdmin
	}
	return need
}

// adminAuthRoleRecorder is implemented by Metrics sinks that take the
// role label on admin_auth_total. Optional, so Metrics implementations
// written before roles keep compiling; they get RecordAdminAuth.
type adminAuthRoleRecorder interface {
	RecordAdminAuthRole(method, outcome, role string)
}

func (g *Gateway) recordAdminAuth(method, outcome string, role adminRole) {
	if outcome == "" {
		return
	}
	if rr, ok := g.cfg.metrics.(adminAuthRoleRecorder); ok {
		rr.RecordAdminAuthRole(method, outcome, role.String())
		return
	}
	g.cfg.metrics.RecordAdminAuth(method, outcome)
}

// ---- dev token -----------------------------------------------------

// devTokenFilename is the leaf name under <data-dir> where the dev
// token is persisted, hex-encoded like the admin token.
const devTokenFilename = "dev-token"

// errDevTokenPinned is RotateDevToken's answer when WithDevToken set
// the token: the next boot would bring the pinned value back.
var errDevTokenPinned = errors.New("dev token is pinned with WithDevToken; change it there")

// DevToken returns the gateway's dev bearer token. A client presents
// it as `Authorization: Bearer <hex>` to read every admin route and to
// edit external-dependent declarations; it cannot drain, deprecate,
// sign tokens or change gateway config. Generated at New (persisted
// under WithAdminDataDir, like the admin token) unless WithDevToken
// pins it. Each gateway node has its own, like the admin token.
//
// Stability: stable
func (g *Gateway) DevToken() []byte {
	if p := g.devToken.Load(); p != nil {
		return *p
	}
	return nil
}

// DevTokenHex is DevToken hex-encoded: the form a client presents and
// what is persisted on disk.
//
// Stability: stable
func (g *Gateway) DevTokenHex() string { return hex.EncodeToString(g.DevToken()) }

// DevTokenPath returns where the dev token is persisted, or "" when no
// admin data dir was configured.
//
// Stability: stable
func (g *Gateway) DevTokenPath() string {
	if g.cfg.adminDataDir == "" {
		return ""
	}
	return filepath.Join(g.cfg.adminDataDir, devTokenFilename)
}

// RotateDevToken replaces the dev token with a fresh one, persists it
// (when an admin data dir is set) and returns it. The old token stops
// working at once on this node. Fails when WithDevToken pinned the
// token.
//
// Stability: stable
func (g *Gateway) RotateDevToken() ([]byte, error) {
	if len(g.cfg.devToken) > 0 {
		return nil, errDevTokenPinned
	}
	tok, err := generateAdminToken()
	if err != nil {
		return nil, err
	}
	if path := g.DevTokenPath(); path != "" {
		if err := writeTokenFile(path, tok); err != nil {
			return nil, err
		}
	}
	g.devToken.Store(&tok)
	return tok, nil
}

// WithDevToken pins the dev bearer token (raw bytes; DevTokenHex is
// the presentation form). Unset: generated at New and persisted to
// <adminDataDir>/dev-token when WithAdminDataDir is set.
//
// Stability: stable
func WithDevToken(token []byte) Option {
	return func(cfg *config) { cfg.devToken = token }
}

// ---- public services -----------------------------------------------

// WithPublicServices names the services admin readers without a token
// may see: "all" makes every service public; any other entry is a
// namespace. Adds to ServiceBinding.public and AsPublic, which mark
// services one by one. Admin visibility only; calls through the
// gateway are not affected. Gateway-internal namespaces stay hidden.
//
// Stability: stable
func WithPublicServices(names ...string) Option {
	return func(cfg *config) {
		for _, n := range names {
			n = strings.TrimSpace(n)
			switch n {
			case "":
			case "all":
				cfg.publicAll = true
			default:
				if cfg.publicNamespaces == nil {
					cfg.publicNamespaces = map[string]bool{}
				}
				cfg.publicNamespaces[n] = true
			}
		}
	}
}

// isPublicLocked reports whether (namespace ns, a line whose manifest
// says manifestPublic) is visible to admin readers without a token.
// Caller holds g.mu.
func (g *Gateway) isPublicLocked(ns string, manifestPublic bool) bool {
	if g.isInternal(ns) {
		return false
	}
	return manifestPublic || g.cfg.publicAll || g.cfg.publicNamespaces[ns] || g.publicNS[ns]
}

// ---- whoami / rotate -----------------------------------------------

type whoamiOut struct {
	Body struct {
		Role string `json:"role" enum:"anonymous,dev,admin" doc:"Role of the presented bearer. anonymous: no bearer; reads show public services only."`
	}
}

type rotateDevTokenOut struct {
	Body struct {
		DevToken string `json:"devToken" doc:"The new dev token, hex. The old one no longer works on this node."`
	}
}

func registerAdminRoleRoutes(api huma.API, g *Gateway) {
	huma.Register(api, huma.Operation{
		OperationID: "whoami",
		Method:      http.MethodGet,
		Path:        "/admin/whoami",
		Summary:     "Role of the presented bearer: anonymous (no bearer), dev or admin. A wrong bearer is 401.",
	}, func(ctx context.Context, _ *struct{}) (*whoamiOut, error) {
		out := &whoamiOut{}
		r, ok := adminRoleFromContext(ctx)
		if !ok {
			r = adminRoleAdmin // no AdminMiddleware: no auth at all
		}
		out.Body.Role = r.String()
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "rotateDevToken",
		Method:      http.MethodPost,
		Path:        "/admin/dev-token/rotate",
		Summary:     "Replace the dev token with a fresh one on this node and return it. The old token stops working at once. Admin only.",
	}, func(_ context.Context, _ *struct{}) (*rotateDevTokenOut, error) {
		tok, err := g.RotateDevToken()
		if err != nil {
			if errors.Is(err, errDevTokenPinned) {
				return nil, huma.Error409Conflict(err.Error())
			}
			return nil, err
		}
		out := &rotateDevTokenOut{}
		out.Body.DevToken = hex.EncodeToString(tok)
		return out, nil
	})
}

// adminRoleFromBearer resolves a bearer outside AdminMiddleware (the
// admin_events subscription). An invalid bearer is anonymous.
func (g *Gateway) adminRoleFromBearer(ctx context.Context, bearer, method, path string) adminRole {
	if bearer == "" {
		return adminRoleAnonymous
	}
	role, outcome, status := g.authenticateAdmin(ctx, bearer, method, path, "", false)
	g.recordAdminAuth(method, outcome, role)
	if status != 0 {
		return adminRoleAnonymous
	}
	return role
}

// adminRoleFromRequestContext resolves the role of a GraphQL request:
// the Authorization header of the HTTP request (or WebSocket
// handshake), else `authorization` in the graphql-transport-ws
// connection_init payload (browsers cannot set WebSocket headers).
func (g *Gateway) adminRoleFromRequestContext(ctx context.Context, method, path string) adminRole {
	bearer := ""
	if r := HTTPRequestFromContext(ctx); r != nil {
		bearer = bearerFromRequest(r)
	}
	if bearer == "" {
		if p := gat.ConnectionInitPayload(ctx); len(p) > 0 {
			var m map[string]any
			if json.Unmarshal(p, &m) == nil {
				for k, v := range m {
					if s, ok := v.(string); ok && strings.EqualFold(k, "authorization") && strings.HasPrefix(s, "Bearer ") {
						bearer = strings.TrimSpace(s[len("Bearer "):])
					}
				}
			}
		}
	}
	return g.adminRoleFromBearer(ctx, bearer, method, path)
}
