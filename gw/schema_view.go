package gateway

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/IodeSystems/graphql-go/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/iodesystems/gwag/v2/gw/ir"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Schema views. The schema exports (SchemaHandler, SchemaProtoHandler,
// SchemaOpenAPIHandler), GraphQL __schema / __type introspection and
// the control plane's ListServices describe every service to the dev
// and admin roles, and public services only to callers with no role
// token (see WithPublicServices for which services are public).
//
// This hides the schema; it is not access control. A query naming a
// field of a non-public service still runs for anyone. To refuse
// documents a client did not ship with, use WithTrustedDocuments with
// enforce.

// schemaViewHeader names, on a schema export response, the view it
// answered with: "full" or "public".
const schemaViewHeader = "X-Gwag-Schema-View"

// publicSchemaView is the GraphQL schema of the public services, for
// one schema generation.
type publicSchemaView struct {
	full        *graphql.Schema // the schema generation it was built from
	gen         uint64          // publicGen at build
	schema      *graphql.Schema // full itself when every service is public
	annotations *ir.AnnotationIndex
}

// publicLinesLocked returns the public pools, and whether every pool
// that renders into the schema is public. Caller holds g.mu.
func (g *Gateway) publicLinesLocked() (lines map[poolKey]bool, all bool) {
	lines = map[poolKey]bool{}
	all = true
	for k, s := range g.slots {
		if g.isPublicLocked(k.namespace, s.manifest.Public) {
			lines[k] = true
		} else if len(s.ir) > 0 {
			all = false
		}
	}
	return lines, all
}

// publicSchema returns the public view of the current schema, building
// it on first use after each schema rebuild (or public-flag change).
// The build reuses the dispatchers of the full schema.
func (g *Gateway) publicSchema() (*publicSchemaView, error) {
	if v := g.publicView.Load(); v != nil && v.full == g.schema.Load() && v.gen == g.publicGen.Load() {
		return v, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	full := g.schema.Load()
	if full == nil {
		if err := g.assembleLocked(); err != nil {
			return nil, err
		}
		full = g.schema.Load()
	}
	gen := g.publicGen.Load()
	if v := g.publicView.Load(); v != nil && v.full == full && v.gen == gen {
		return v, nil
	}
	v := &publicSchemaView{full: full, gen: gen}
	lines, all := g.publicLinesLocked()
	if all {
		v.schema, v.annotations = full, g.schemaAnnotations.Load()
	} else {
		schema, ann, err := g.buildSchemaLocked(schemaFilter{lines: lines, reuseDispatchers: true})
		if err != nil {
			return nil, err
		}
		v.schema, v.annotations = schema, ann
	}
	g.publicView.Store(v)
	return v, nil
}

// introspectionSchema is the gat IntrospectionView hook: nil (the
// full schema) for the dev and admin roles, else the public view.
func (g *Gateway) introspectionSchema(ctx context.Context) *graphql.Schema {
	if g.introspectionRole(ctx) >= adminRoleDev {
		return nil
	}
	v, err := g.publicSchema()
	if err != nil {
		// Fail closed: describe nothing rather than everything.
		log.Printf("gateway: public schema view: %v", err)
		return emptySchema
	}
	return v.schema
}

// emptySchema answers introspection when the public view cannot be
// built.
var emptySchema = func() *graphql.Schema {
	s, err := graphql.NewSchema(graphql.SchemaConfig{Query: graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{"_status": &graphql.Field{
			Type:    graphql.String,
			Resolve: func(graphql.ResolveParams) (any, error) { return "schema unavailable", nil },
		}},
	})})
	if err != nil {
		panic(err)
	}
	return &s
}()

// introspectionRole resolves the role token of an introspecting
// GraphQL request (roleTokenFromContext). A token that is not a gwag
// one is anonymous, never an error: Authorization on the GraphQL
// endpoint usually carries the end user's token for upstreams. Only a
// RoleTokenHeader that fails is recorded as a denied admin auth.
func (g *Gateway) introspectionRole(ctx context.Context) adminRole {
	tok, dedicated := roleTokenFromContext(ctx)
	if tok == "" {
		return adminRoleAnonymous
	}
	remote := ""
	if r := HTTPRequestFromContext(ctx); r != nil {
		remote = r.RemoteAddr
	}
	role, outcome, status := g.authenticateAdmin(ctx, tok, "INTROSPECT", "graphql", remote, false)
	if status != 0 {
		if dedicated {
			g.recordAdminAuth("INTROSPECT", outcome, role)
		}
		return adminRoleAnonymous
	}
	g.recordAdminAuth("INTROSPECT", outcome, role)
	return role
}

// schemaExportFull resolves the view of a schema export request: true
// for dev and admin, false for no token. A token that fails is 401
// (written to w; ok false), as on the admin routes. A request that
// passed AdminMiddleware keeps the role it resolved.
func (g *Gateway) schemaExportFull(w http.ResponseWriter, r *http.Request) (full, ok bool) {
	w.Header().Add("Vary", "Authorization, "+RoleTokenHeader)
	role, resolved := adminRoleFromContext(r.Context())
	if !resolved {
		tok, _ := roleTokenFromRequest(r)
		if tok != "" {
			var outcome string
			var status int
			role, outcome, status = g.authenticateAdmin(r.Context(), tok, r.Method, r.URL.Path, r.RemoteAddr, false)
			g.recordAdminAuth(r.Method, outcome, role)
			if status != 0 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
				http.Error(w, http.StatusText(status), status)
				return false, false
			}
		}
	}
	full = role >= adminRoleDev
	if full {
		w.Header().Set(schemaViewHeader, "full")
	} else {
		w.Header().Set(schemaViewHeader, "public")
	}
	return full, true
}

// schemaViewMetadata is schemaViewHeader as gRPC response metadata.
const schemaViewMetadata = "x-gwag-schema-view"

// controlPlaneRole resolves the role of a control-plane call from its
// metadata: `x-gwag-token`, else `authorization: Bearer`. No token is
// anonymous; a token that fails is Unauthenticated.
func (g *Gateway) controlPlaneRole(ctx context.Context) (adminRole, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	tok := ""
	if v := md.Get(strings.ToLower(RoleTokenHeader)); len(v) > 0 {
		tok = trimBearer(strings.TrimSpace(v[0]))
	}
	if tok == "" {
		tok = bearerFromMetadata(md)
	}
	if tok == "" {
		return adminRoleAnonymous, nil
	}
	remote := ""
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		remote = p.Addr.String()
	}
	const method = "/gateway.controlplane.v1.ControlPlane/ListServices"
	role, outcome, st := g.authenticateAdmin(ctx, tok, "GRPC", method, remote, false)
	g.recordAdminAuth("GRPC", outcome, role)
	if st != 0 {
		return adminRoleAnonymous, status.Error(codes.Unauthenticated, "invalid role token")
	}
	return role, nil
}

// publicServicesResponse is resp as an anonymous caller may see it:
// the rows adminView keeps, scrubbed the same way.
func (g *Gateway) publicServicesResponse(ctx context.Context, resp *cpv1.ListServicesResponse) *cpv1.ListServicesResponse {
	view := g.adminViewFor(withAdminRole(ctx, adminRoleAnonymous), resp)
	out := &cpv1.ListServicesResponse{Environment: resp.GetEnvironment(), Services: []*cpv1.ServiceInfo{}}
	for _, s := range resp.GetServices() {
		if !view.line(s.GetNamespace(), s.GetVersion()) {
			continue
		}
		var deps []*cpv1.Dependency
		for _, d := range s.GetDepends() {
			if view.namespace(d.GetService()) {
				deps = append(deps, d)
			}
		}
		s.Depends = deps
		if o := s.GetOwner(); o != nil {
			s.Owner = &cpv1.Owner{Team: o.GetTeam()}
		}
		for _, inst := range s.GetInstances() {
			inst.Addr = ""
			var kept []*cpv1.DependencyStatus
			for _, d := range inst.GetDepends() {
				if view.namespace(d.GetService()) {
					kept = append(kept, d)
				}
			}
			inst.Depends = kept
		}
		out.Services = append(out.Services, s)
	}
	for ns, vN := range resp.GetStableVn() {
		if view.namespace(ns) {
			if out.StableVn == nil {
				out.StableVn = map[string]uint32{}
			}
			out.StableVn[ns] = vN
		}
	}
	return out
}
