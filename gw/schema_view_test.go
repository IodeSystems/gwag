package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"nhooyr.io/websocket"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Schema views (schema_view.go), on newRolesGateway: greeter/v1 is
// public; library/v1 and billing/v1 are not.

// viewReq is one way a caller presents itself.
type viewReq struct {
	name          string
	header, value string // "" = no token
	full          bool
}

var viewCallers = []viewReq{
	{"anonymous", "", "", false},
	{"dev Authorization", "Authorization", bearerHex(rolesDevTok), true},
	{"dev X-Gwag-Token", RoleTokenHeader, hex.EncodeToString([]byte(rolesDevTok)), true},
	{"admin X-Gwag-Token Bearer", RoleTokenHeader, bearerHex(rolesAdminTok), true},
}

func viewGet(t *testing.T, h http.Handler, path, header, value string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if header != "" {
		req.Header.Set(header, value)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestSchemaView_Exports(t *testing.T) {
	g := newRolesGateway(t)
	exports := []struct {
		name string
		h    http.Handler
		path string
		// hidden / shown are substrings of the body.
		hidden, shown string
	}{
		{"graphql sdl", g.SchemaHandler(), "/schema/graphql", "billing", "greeter"},
		{"graphql json", g.SchemaHandler(), "/schema/graphql?format=json", "billing", "greeter"},
		{"proto sdl", g.SchemaProtoHandler(), "/schema/proto?format=sdl", "library", "greeter"},
		{"proto fds", g.SchemaProtoHandler(), "/schema/proto", "library", "greeter"},
		{"openapi", g.SchemaOpenAPIHandler(), "/schema/openapi", `"billing"`, `"greeter"`},
	}
	for _, e := range exports {
		for _, c := range viewCallers {
			t.Run(e.name+"/"+c.name, func(t *testing.T) {
				rr := viewGet(t, e.h, e.path, c.header, c.value)
				if rr.Code != http.StatusOK {
					t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
				}
				body := rr.Body.String()
				wantView := "public"
				if c.full {
					wantView = "full"
				}
				if got := rr.Header().Get(schemaViewHeader); got != wantView {
					t.Errorf("%s=%q, want %q", schemaViewHeader, got, wantView)
				}
				if !strings.Contains(body, e.shown) {
					t.Errorf("body lacks %q", e.shown)
				}
				if strings.Contains(body, e.hidden) != c.full {
					t.Errorf("body contains %q = %v, want %v", e.hidden, !c.full, c.full)
				}
			})
		}
		t.Run(e.name+"/wrong token", func(t *testing.T) {
			if rr := viewGet(t, e.h, e.path, RoleTokenHeader, "nope"); rr.Code != http.StatusUnauthorized {
				t.Errorf("status %d, want 401", rr.Code)
			}
			if rr := viewGet(t, e.h, e.path, "Authorization", "Bearer nope"); rr.Code != http.StatusUnauthorized {
				t.Errorf("Authorization: status %d, want 401", rr.Code)
			}
		})
	}

	// A selector does not widen the anonymous view.
	for _, path := range []string{"/schema/graphql?service=billing", "/schema/openapi?service=billing", "/schema/proto?service=library&format=sdl"} {
		h := g.SchemaHandler()
		switch {
		case strings.HasPrefix(path, "/schema/openapi"):
			h = g.SchemaOpenAPIHandler()
		case strings.HasPrefix(path, "/schema/proto"):
			h = g.SchemaProtoHandler()
		}
		anon := viewGet(t, h, path, "", "").Body.String()
		dev := viewGet(t, h, path, "Authorization", bearerHex(rolesDevTok)).Body.String()
		hidden := "billing"
		if strings.Contains(path, "proto") {
			hidden = "library"
		}
		if strings.Contains(anon, hidden) || !strings.Contains(dev, hidden) {
			t.Errorf("%s: anonymous has %q=%v (want false), dev=%v (want true)", path, hidden, strings.Contains(anon, hidden), strings.Contains(dev, hidden))
		}
	}

	// Behind AdminMiddleware the resolved role is kept.
	mw := g.AdminMiddleware(g.SchemaHandler())
	if rr := viewGet(t, mw, "/schema/graphql", "Authorization", bearerHex(rolesDevTok)); !strings.Contains(rr.Body.String(), "billing") {
		t.Errorf("behind AdminMiddleware, dev: %d", rr.Code)
	}
}

// gqlPost posts query to h with one header ("" = none) and returns
// the decoded response.
func gqlPost(t *testing.T, h http.Handler, query, header, value string) (data map[string]any, errs []map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"query": query})
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header.Set(header, value)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var out struct {
		Data   map[string]any   `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, rr.Body.String())
	}
	return out.Data, out.Errors
}

// rootFieldNames is the Query fields of an introspection answer to
// `{ __schema { queryType { fields { name } } } }`.
func rootFieldNames(data map[string]any) []string {
	var names []string
	s, _ := data["__schema"].(map[string]any)
	qt, _ := s["queryType"].(map[string]any)
	fs, _ := qt["fields"].([]any)
	for _, f := range fs {
		if m, ok := f.(map[string]any); ok {
			names = append(names, m["name"].(string))
		}
	}
	sort.Strings(names)
	return names
}

func typeNames(data map[string]any) map[string]bool {
	out := map[string]bool{}
	s, _ := data["__schema"].(map[string]any)
	ts, _ := s["types"].([]any)
	for _, tt := range ts {
		if m, ok := tt.(map[string]any); ok {
			out[m["name"].(string)] = true
		}
	}
	return out
}

func TestSchemaView_IntrospectionHTTP(t *testing.T) {
	g := newRolesGateway(t)
	h := g.Handler()
	const q = `{ __schema { queryType { fields { name } } types { name } } }`

	fullData, errs := gqlPost(t, h, q, "Authorization", bearerHex(rolesAdminTok))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	full := typeNames(fullData)
	for _, c := range viewCallers {
		t.Run(c.name, func(t *testing.T) {
			data, errs := gqlPost(t, h, q, c.header, c.value)
			if len(errs) > 0 {
				t.Fatal(errs)
			}
			want := []string{"greeter"}
			if c.full {
				want = []string{"billing", "greeter", "library", "ps"}
			}
			if got := rootFieldNames(data); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("root fields %v, want %v", got, want)
			}
			if got := typeNames(data); c.full != (len(got) == len(full)) {
				t.Errorf("%d types, full schema has %d", len(got), len(full))
			}
		})
	}

	anon := typeNames(func() map[string]any { d, _ := gqlPost(t, h, q, "", ""); return d }())
	var hidden []string
	for name := range full {
		if !anon[name] {
			hidden = append(hidden, name)
		}
	}
	if len(hidden) == 0 {
		t.Fatal("public view hides no type")
	}
	for name := range anon {
		if !full[name] {
			t.Errorf("public view has %q, full does not", name)
		}
	}
	for _, s := range []string{"Upload", "String", "__Schema"} {
		if !anon[s] {
			t.Errorf("public view lacks %q", s)
		}
	}
	tq := `{ __type(name: "` + hidden[0] + `") { name } }`
	if data, errs := gqlPost(t, h, tq, "", ""); len(errs) > 0 || data["__type"] != nil {
		t.Errorf("anonymous __type(%s) = %v %v, want null", hidden[0], data, errs)
	}
	if data, _ := gqlPost(t, h, tq, RoleTokenHeader, hex.EncodeToString([]byte(rolesDevTok))); data["__type"] == nil {
		t.Errorf("dev __type(%s) = null", hidden[0])
	}

	// An end user's Authorization (not a gwag token) is anonymous, not
	// an error.
	if data, errs := gqlPost(t, h, q, "Authorization", "Bearer end-user-jwt"); len(errs) > 0 || len(rootFieldNames(data)) != 1 {
		t.Errorf("end-user bearer: %v %v", rootFieldNames(data), errs)
	}
	// RoleTokenHeader wins over Authorization.
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ __schema { queryType { fields { name } } } }"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer end-user-jwt")
	req.Header.Set(RoleTokenHeader, hex.EncodeToString([]byte(rolesDevTok)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "library") {
		t.Errorf("X-Gwag-Token with an end-user Authorization: %s", rr.Body.String())
	}

	// Data plane unchanged: a hidden field still validates and runs
	// for anyone (its upstream is not reachable, so it errors at
	// dispatch, not in validation).
	_, errs = gqlPost(t, h, `{ library { listBooks { books { id } } } }`, "", "")
	for _, e := range errs {
		if msg, _ := e["message"].(string); strings.Contains(msg, "Cannot query field") {
			t.Errorf("anonymous data query refused in validation: %s", msg)
		}
	}

	// A string that only looks like introspection keeps the full
	// schema.
	if _, errs := gqlPost(t, h, `{ library { listBooks(author: "__schema") { books { id } } } }`, "", ""); hasValidationError(errs) {
		t.Errorf("string literal __schema: %v", errs)
	}

	// Batches: one introspecting document runs the batch on the view.
	b := `[{"query":"{ __schema { queryType { fields { name } } } }"},{"query":"{ __typename }"}]`
	req = httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if body := rr.Body.String(); strings.Contains(body, "library") || !strings.Contains(body, "greeter") {
		t.Errorf("anonymous batch: %s", body)
	}
}

func hasValidationError(errs []map[string]any) bool {
	for _, e := range errs {
		if msg, _ := e["message"].(string); strings.Contains(msg, "Cannot query field") || strings.Contains(msg, "Unknown argument") {
			return true
		}
	}
	return false
}

// The public view is built once per schema generation and reused;
// a rebuild or a public-flag change makes a new one.
func TestSchemaView_CachedPerGeneration(t *testing.T) {
	g := newRolesGateway(t)
	h := g.Handler()
	const q = `{ __schema { queryType { fields { name } } } }`
	gqlPost(t, h, q, "", "")
	v1 := g.publicView.Load()
	if v1 == nil || v1.schema == g.schema.Load() {
		t.Fatalf("view %v: want a separate public schema", v1)
	}
	gqlPost(t, h, q, "", "")
	viewGet(t, g.SchemaHandler(), "/schema/graphql", "", "")
	if g.publicView.Load() != v1 {
		t.Error("public view rebuilt without a schema change")
	}

	// library/v1 re-registers public: same hash, no schema rebuild,
	// but a new view.
	req := consumerReq(t, "library", "v1")
	req.InstanceId = "library-1"
	req.Services[0].Public = true
	registerOn(t, g, req)
	data, _ := gqlPost(t, h, q, "", "")
	if got := strings.Join(rootFieldNames(data), ","); got != "greeter,library" {
		t.Errorf("after library went public: %s", got)
	}
	if g.publicView.Load() == v1 {
		t.Error("public view not rebuilt after a public-flag change")
	}
}

func TestSchemaView_PublicServicesAll(t *testing.T) {
	g := newRolesGateway(t, WithPublicServices("all"))
	data, errs := gqlPost(t, g.Handler(), `{ __schema { queryType { fields { name } } } }`, "", "")
	if len(errs) > 0 || strings.Join(rootFieldNames(data), ",") != "billing,greeter,library,ps" {
		t.Errorf("anonymous root fields: %v %v", rootFieldNames(data), errs)
	}
	if v := g.publicView.Load(); v == nil || v.schema != g.schema.Load() {
		t.Error("every service public: the view should be the full schema, not a rebuild")
	}
	if body := viewGet(t, g.SchemaHandler(), "/schema/graphql", "", "").Body.String(); !strings.Contains(body, "billing") {
		t.Error("anonymous SDL lacks billing")
	}
	resp, _ := listServicesWire(t, g, nil, "public")
	if len(resp.GetServices()) != 4 {
		t.Errorf("anonymous ListServices: %d services, want 4", len(resp.GetServices()))
	}
}

func TestSchemaView_WithoutIntrospectionStillRefuses(t *testing.T) {
	g := newRolesGateway(t, WithoutIntrospection())
	for _, c := range viewCallers {
		if _, errs := gqlPost(t, g.Handler(), `{ __schema { types { name } } }`, c.header, c.value); len(errs) == 0 {
			t.Errorf("%s: introspection allowed", c.name)
		}
	}
}

// listServicesWire calls ListServices over a real gRPC connection with
// md as outgoing metadata, and checks the view header.
func listServicesWire(t *testing.T, g *Gateway, md metadata.MD, wantView string) (*cpv1.ListServicesResponse, error) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(srv, g.ControlPlane())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if md != nil {
		ctx = metadata.NewOutgoingContext(ctx, md)
	}
	var header metadata.MD
	resp, err := cpv1.NewControlPlaneClient(conn).ListServices(ctx, &cpv1.ListServicesRequest{}, grpc.Header(&header))
	if err == nil {
		if v := header.Get(schemaViewMetadata); len(v) != 1 {
			t.Errorf("%s header: %v", schemaViewMetadata, v)
		} else if v[0] != wantView {
			t.Errorf("%s=%q, want %q", schemaViewMetadata, v[0], wantView)
		}
	}
	return resp, err
}

func TestSchemaView_ListServicesGRPC(t *testing.T) {
	g := newRolesGateway(t)
	names := func(r *cpv1.ListServicesResponse) string {
		var out []string
		for _, s := range r.GetServices() {
			out = append(out, s.GetNamespace())
		}
		return strings.Join(out, ",")
	}

	resp, err := listServicesWire(t, g, nil, "public")
	if err != nil || names(resp) != "greeter" {
		t.Fatalf("anonymous: %q %v", names(resp), err)
	}
	s := resp.GetServices()[0]
	if s.GetOwner().GetContact() != "" || s.GetOwner().GetTeam() != "catalog" || len(s.GetDepends()) != 0 {
		t.Errorf("anonymous greeter owner=%v depends=%v, want team only, no depends", s.GetOwner(), s.GetDepends())
	}
	for _, inst := range s.GetInstances() {
		if inst.GetAddr() != "" {
			t.Errorf("anonymous instance addr %q", inst.GetAddr())
		}
	}

	for _, md := range []metadata.MD{
		metadata.Pairs("authorization", bearerHex(rolesDevTok)),
		metadata.Pairs("x-gwag-token", hex.EncodeToString([]byte(rolesAdminTok))),
	} {
		resp, err := listServicesWire(t, g, md, "full")
		if err != nil || names(resp) != "billing,greeter,library,ps" {
			t.Errorf("%v: %q %v", md, names(resp), err)
		}
	}
	if _, err := listServicesWire(t, g, metadata.Pairs("authorization", "Bearer nope"), ""); status.Code(err) != codes.Unauthenticated {
		t.Errorf("wrong token: %v, want Unauthenticated", err)
	}

	// In process: every service; the admin handlers filter on their own.
	resp, err = g.ControlPlane().ListServices(context.Background(), &cpv1.ListServicesRequest{})
	if err != nil || names(resp) != "billing,greeter,library,ps" {
		t.Errorf("in process: %q %v", names(resp), err)
	}
}

// graphql-transport-ws: a subscribe message whose document introspects
// is validated against the public view for an anonymous connection.
func TestSchemaView_IntrospectionWS(t *testing.T) {
	g := newRolesGateway(t)
	// A server-streaming RPC under a non-public namespace: its
	// Subscription field exists in the full schema only.
	if err := g.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), To(nopGRPCConn{}), As("secret")); err != nil {
		t.Fatal(err)
	}
	full, _ := gqlPost(t, g.Handler(), `{ __schema { subscriptionType { fields { name } } } }`, "Authorization", bearerHex(rolesAdminTok))
	field := ""
	st, _ := full["__schema"].(map[string]any)["subscriptionType"].(map[string]any)
	fs, _ := st["fields"].([]any)
	for _, f := range fs {
		if n := f.(map[string]any)["name"].(string); strings.HasPrefix(n, "secret") {
			field = n
		}
	}
	if field == "" {
		t.Fatalf("no secret subscription field in %v", full)
	}

	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/graphql"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// The document introspects (query Q) and runs subscription S.
	doc := `query Q { __schema { queryType { name } } } subscription S { ` + field + ` { __typename } }`
	// firstFrame returns the first frame after subscribe, or "" when
	// none came within wait (the subscription is running).
	firstFrame := func(init map[string]any, wait time.Duration) string {
		t.Helper()
		conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Subprotocols: []string{"graphql-transport-ws"}})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		payload, _ := json.Marshal(init)
		if err := writeWS(conn, ctx, wsMessage{Type: "connection_init", Payload: payload}); err != nil {
			t.Fatal(err)
		}
		if ack, err := readWS(conn, ctx); err != nil || ack.Type != "connection_ack" {
			t.Fatalf("ack: %v %+v", err, ack)
		}
		sub, _ := json.Marshal(map[string]any{"query": doc, "operationName": "S"})
		if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: "subscribe", Payload: sub}); err != nil {
			t.Fatal(err)
		}
		rctx, rcancel := context.WithTimeout(ctx, wait)
		defer rcancel()
		_, raw, err := conn.Read(rctx)
		if err != nil {
			if rctx.Err() != nil {
				return ""
			}
			t.Fatal(err)
		}
		return string(raw)
	}
	if got := firstFrame(map[string]any{}, 5*time.Second); !strings.Contains(got, "Cannot query field") {
		t.Errorf("anonymous: %s, want a validation error on %s", got, field)
	}
	for _, init := range []map[string]any{
		{"authorization": bearerHex(rolesDevTok)},
		{"x-gwag-token": hex.EncodeToString([]byte(rolesDevTok))},
	} {
		if got := firstFrame(init, time.Second); strings.Contains(got, "Cannot query field") {
			t.Errorf("dev %v: %s", init, got)
		}
	}
}
