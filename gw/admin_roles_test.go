package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"

	aav1 "github.com/iodesystems/gwag/v2/gw/proto/adminauth/v1"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Admin roles (admin_roles.go, admin_view.go): anonymous, dev, admin.

const (
	rolesAdminTok = "tok"
	rolesDevTok   = "devtok"
)

// newRolesGateway: greeter/v1 is public (owner with contact, depends
// on billing [v1]); library/v1 and billing/v1 are not (library
// depends on greeter [v1]). Traffic on greeter/v1: 2 calls from
// library, 1 from billing/v1, 1 from greeter itself. External
// dependent acme pins greeter [v1].
func newRolesGateway(t *testing.T, opts ...Option) *Gateway {
	t.Helper()
	_, addr := startGreeterServer(t)
	g := New(append([]Option{
		WithoutMetrics(), WithoutBackpressure(),
		WithAdminToken([]byte(rolesAdminTok)), WithDevToken([]byte(rolesDevTok)),
		WithCallerHeaders("X-Caller-Service"),
	}, opts...)...)
	t.Cleanup(g.Close)

	billing := libraryBinding(t, "v1")
	billing.Namespace = "billing"
	mustRegister(t, g, "127.0.0.1:1", billing)

	gb := greeterBinding(t, "v1")
	gb.Public = true
	gb.Owner = &cpv1.Owner{Team: "catalog", Contact: "#catalog"}
	gb.Depends = []*cpv1.Dependency{{Service: "billing", Lines: []string{"v1"}}}
	registerOn(t, g, &cpv1.RegisterRequest{Addr: addr, InstanceId: "greeter-0", Services: []*cpv1.ServiceBinding{gb}})
	registerOn(t, g, consumerReq(t, "library", "v1"))

	recordCall(g, "greeter", "v1", "Hello", "library")
	recordCall(g, "greeter", "v1", "Hello", "library")
	recordCall(g, "greeter", "v1", "Hello", "billing/v1")
	recordCall(g, "greeter", "v1", "Bye", "greeter")
	recordCall(g, "library", "v1", "Get", "greeter")

	putExternal(t, g, "acme", acmeV1)
	return g
}

func bearerHex(tok string) string { return "Bearer " + hex.EncodeToString([]byte(tok)) }

// rolesDo sends one request through AdminMiddleware with the given
// Authorization header ("" = none).
func rolesDo(t *testing.T, h http.Handler, method, path, body, authz string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

type roleCase struct {
	op           string
	method, path string
	body         string
	min          adminRole
}

// rolesMatrix is every admin operation with the role it needs. The
// order matters only at the end: drain flips the gateway to draining.
var rolesMatrix = []roleCase{
	{"whoami", "GET", "/admin/whoami", "", adminRoleAnonymous},
	{"listServices", "GET", "/admin/services", "", adminRoleAnonymous},
	{"servicesStats", "GET", "/admin/services/stats?window=1m", "", adminRoleAnonymous},
	{"servicesHistory", "GET", "/admin/services/history?window=1m", "", adminRoleAnonymous},
	{"serviceStats", "GET", "/admin/services/greeter/v1/stats?window=1m", "", adminRoleAnonymous},
	{"deprecatedStats", "GET", "/admin/services/deprecated/stats", "", adminRoleAnonymous},
	{"usageEdges", "GET", "/admin/services/usage", "", adminRoleAnonymous},
	{"dependencyGraph", "GET", "/admin/graph", "", adminRoleAnonymous},

	{"listPeers", "GET", "/admin/peers", "", adminRoleDev},
	{"listChannels", "GET", "/admin/channels", "", adminRoleDev},
	{"listBindings", "GET", "/admin/bindings", "", adminRoleDev},
	{"listInjectors", "GET", "/admin/injectors", "", adminRoleDev},
	{"listInstances", "GET", "/admin/instances", "", adminRoleDev},
	{"listExternalDependents", "GET", "/admin/external-dependents", "", adminRoleDev},
	{"mcpList", "GET", "/admin/mcp", "", adminRoleDev},
	{"mcpSchemaList", "GET", "/admin/mcp/schema/list", "", adminRoleDev},
	{"mcpSchemaSearch", "POST", "/admin/mcp/schema/search", `{}`, adminRoleDev},
	{"mcpSchemaExpand", "POST", "/admin/mcp/schema/expand", `{"name":"greeter.hello"}`, adminRoleDev},
	{"putExternalDependent", "PUT", "/admin/external-dependents/zeta", `{"kind":"customer","reviewAt":"2099-01-01T00:00:00Z","depends":[{"service":"greeter","lines":["v1"]}]}`, adminRoleDev},
	{"reviewExternalDependent", "POST", "/admin/external-dependents/zeta/review", `{"reviewAt":"2099-06-01T00:00:00Z"}`, adminRoleDev},
	{"listNotifications", "GET", "/admin/notifications", "", adminRoleDev},
	{"dismissNotification", "POST", "/admin/notifications/x/dismiss", "", adminRoleDev},
	{"deleteExternalDependent", "DELETE", "/admin/external-dependents/zeta", "", adminRoleDev},

	{"forgetPeer", "POST", "/admin/peers/x/forget", "", adminRoleAdmin},
	{"evacuatePeer", "POST", "/admin/peers/x/evacuate", `{}`, adminRoleAdmin},
	{"finalizeCluster", "POST", "/admin/cluster/finalize", `{}`, adminRoleAdmin},
	{"signSubscriptionToken", "POST", "/admin/sign", `{"channel":"a","ttlSeconds":60}`, adminRoleAdmin},
	{"retractStable", "POST", "/admin/services/greeter/stable/retract", `{"targetVN":1}`, adminRoleAdmin},
	{"deprecate", "POST", "/admin/services/greeter/v1/deprecate", `{"reason":"x"}`, adminRoleAdmin},
	{"undeprecate", "POST", "/admin/services/greeter/v1/undeprecate", "", adminRoleAdmin},
	{"mcpInclude", "POST", "/admin/mcp/include", `{"path":"greeter.*"}`, adminRoleAdmin},
	{"mcpExclude", "POST", "/admin/mcp/exclude", `{"path":"greeter.*"}`, adminRoleAdmin},
	{"mcpIncludeRemove", "POST", "/admin/mcp/include/remove", `{"path":"greeter.*"}`, adminRoleAdmin},
	{"mcpExcludeRemove", "POST", "/admin/mcp/exclude/remove", `{"path":"greeter.*"}`, adminRoleAdmin},
	{"mcpSetAutoInclude", "POST", "/admin/mcp/auto-include", `{"autoInclude":false}`, adminRoleAdmin},
	{"mcpQuery", "POST", "/admin/mcp/query", `{"query":"{ __typename }"}`, adminRoleAdmin},
	{"rotateDevToken", "POST", "/admin/dev-token/rotate", "", adminRoleAdmin},
	{"listAppKeys", "GET", "/admin/app-keys", "", adminRoleAdmin},
	{"listReplayDiffs", "GET", "/admin/replay/diffs", "", adminRoleAdmin},
	{"getSwitchover", "GET", "/admin/switchover", "", adminRoleDev},
	{"setSwitchover", "PUT", "/admin/switchover", `{"percent":0}`, adminRoleAdmin},
	{"clearSwitchover", "DELETE", "/admin/switchover", "", adminRoleAdmin},
	{"createAppKey", "POST", "/admin/app-keys", `{"namespaces":["greeter"],"ttl":"30d"}`, adminRoleAdmin},
	{"revokeAppKey", "DELETE", "/admin/app-keys/nobody", "", adminRoleAdmin},
	{"createSystemKey", "POST", "/admin/system-keys", `{"ttl":"30d"}`, adminRoleAdmin},
	{"exportClusterState", "GET", "/admin/cluster/state?allowUnfrozen=true", "", adminRoleAdmin},
	{"importClusterState", "POST", "/admin/cluster/state", `{"state":{"v":1,"stable":[],"deprecated":[],"keys":[],"externalDependents":[]},"dryRun":true}`, adminRoleAdmin},
	{"getClusterFreeze", "GET", "/admin/cluster/freeze", "", adminRoleDev},
	{"freezeCluster", "POST", "/admin/cluster/freeze", `{"ttl":"1m"}`, adminRoleAdmin},
	{"unfreezeCluster", "POST", "/admin/cluster/unfreeze", "", adminRoleAdmin},
	{"drainInstance", "POST", "/admin/instances/nobody/drain", `{}`, adminRoleAdmin},
	{"drain", "POST", "/admin/drain", `{"timeoutSeconds":1}`, adminRoleAdmin},
}

// rolesSystemOps are the operations a system key may call
// (system_keys.go); every other one is 403 with a system key.
var rolesSystemOps = map[string]bool{
	"exportClusterState": true,
	"importClusterState": true,
}

func TestAdminRoles_OpMatrix(t *testing.T) {
	g := newRolesGateway(t)
	h := newAnonAdminRouter(t, g)
	_, sysKey, err := g.createSystemKey(context.Background(), time.Hour, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	roles := []struct {
		role  adminRole
		authz string
	}{
		{adminRoleAnonymous, ""},
		{adminRoleDev, bearerHex(rolesDevTok)},
		{adminRoleAdmin, bearerHex(rolesAdminTok)},
	}
	for _, c := range rolesMatrix {
		code, body := rolesDo(t, h, c.method, c.path, c.body, "Bearer "+sysKey)
		switch {
		case rolesSystemOps[c.op] && (code == http.StatusUnauthorized || code == http.StatusForbidden):
			t.Errorf("%s as system: %d, want allowed (%s)", c.op, code, body)
		case !rolesSystemOps[c.op] && code != http.StatusForbidden:
			t.Errorf("%s as system: %d, want 403 (%s)", c.op, code, body)
		}
		for _, r := range roles {
			code, body := rolesDo(t, h, c.method, c.path, c.body, r.authz)
			allowed := r.role >= c.min
			switch {
			case allowed && (code == http.StatusUnauthorized || code == http.StatusForbidden):
				t.Errorf("%s as %s: %d, want allowed (%s)", c.op, r.role, code, body)
			case !allowed && r.role == adminRoleAnonymous && code != http.StatusUnauthorized:
				t.Errorf("%s as anonymous: %d, want 401 (%s)", c.op, code, body)
			case !allowed && r.role != adminRoleAnonymous && code != http.StatusForbidden:
				t.Errorf("%s as %s: %d, want 403 (%s)", c.op, r.role, code, body)
			}
			if c.op == "whoami" {
				var out struct{ Role string }
				_ = json.Unmarshal([]byte(body), &out)
				if out.Role != r.role.String() {
					t.Errorf("whoami as %s: role=%q", r.role, out.Role)
				}
			}
		}
	}
}

// Every operation the admin router registers is in rolesMatrix with
// the role the route table gives it, so a new route cannot open (or
// close) without a test saying so.
func TestAdminRoles_EveryOpClassified(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	_, spec, err := g.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]adminRole{}
	for _, c := range rolesMatrix {
		want[c.op] = c.min
	}
	seen := 0
	for path, item := range doc.Paths {
		for method, op := range item {
			min, ok := want[op.OperationID]
			if !ok {
				t.Errorf("%s %s (%s) missing from rolesMatrix", strings.ToUpper(method), path, op.OperationID)
				continue
			}
			seen++
			req := httptest.NewRequest(strings.ToUpper(method), strings.NewReplacer("{namespace}", "a", "{version}", "v1", "{nodeId}", "n", "{instance}", "i", "{id}", "x").Replace(path), nil)
			if got := g.adminRouteNeed(req); got != min {
				t.Errorf("%s: route needs %s, matrix says %s", op.OperationID, got, min)
			}
			if got := g.adminRouteSystem(req); got != rolesSystemOps[op.OperationID] {
				t.Errorf("%s: system key allowed=%v, rolesSystemOps says %v", op.OperationID, got, rolesSystemOps[op.OperationID])
			}
		}
	}
	if seen != len(rolesMatrix) {
		t.Errorf("spec has %d ops, matrix %d", seen, len(rolesMatrix))
	}
}

func TestAdminRoles_WrongBearerOnRead(t *testing.T) {
	g := newRolesGateway(t)
	h := newAnonAdminRouter(t, g)
	if code, _ := rolesDo(t, h, "GET", "/admin/services", "", bearerHex("nope")); code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer on an open read: %d, want 401", code)
	}
}

type rolesServices struct {
	Services []struct {
		Namespace, Version string
		Public             bool
		Owner              *struct{ Team, Contact string }
		Depends            []struct{ Service string }
		Instances          []struct {
			Addr, State string
			Depends     []struct{ Service string }
		}
	}
}

func TestAdminRoles_AnonymousServices(t *testing.T) {
	g := newRolesGateway(t)
	h := newAnonAdminRouter(t, g)

	var anon rolesServices
	code, body := rolesDo(t, h, "GET", "/admin/services", "", "")
	if code != 200 || json.Unmarshal([]byte(body), &anon) != nil {
		t.Fatalf("%d %s", code, body)
	}
	if len(anon.Services) != 1 || anon.Services[0].Namespace != "greeter" || !anon.Services[0].Public {
		t.Fatalf("anonymous services=%+v, want greeter only", anon.Services)
	}
	s := anon.Services[0]
	if s.Owner == nil || s.Owner.Team != "catalog" || s.Owner.Contact != "" {
		t.Errorf("owner=%+v, want team without contact", s.Owner)
	}
	if len(s.Depends) != 0 {
		t.Errorf("depends=%+v, want billing (not public) dropped", s.Depends)
	}
	if len(s.Instances) != 1 || s.Instances[0].Addr != "" || s.Instances[0].State != "ready" || len(s.Instances[0].Depends) != 0 {
		t.Errorf("instances=%+v, want one ready, addr and hidden depends blanked", s.Instances)
	}

	var dev rolesServices
	_, body = rolesDo(t, h, "GET", "/admin/services", "", bearerHex(rolesDevTok))
	if err := json.Unmarshal([]byte(body), &dev); err != nil {
		t.Fatal(err)
	}
	if len(dev.Services) != 4 {
		t.Fatalf("dev services=%+v, want 4 (ps included)", dev.Services)
	}
	for _, s := range dev.Services {
		if s.Namespace == "greeter" && (s.Owner.Contact != "#catalog" || len(s.Depends) != 1 || s.Instances[0].Addr == "") {
			t.Errorf("dev greeter=%+v, want full", s)
		}
	}
}

func TestAdminRoles_AnonymousGraph(t *testing.T) {
	g := newRolesGateway(t)
	h := newAnonAdminRouter(t, g)
	type graph struct {
		Nodes []struct {
			ID           string
			Public       bool
			Owner        *struct{ Team, Contact string }
			TrafficCount uint64
			SunsetReason string
			Instances    []struct {
				Addr         string
				DrainReasons []string
			}
		}
		DesiredEdges []struct {
			ConsumerKind, ConsumerNamespace, Provider string
		}
		ActiveEdges []struct {
			Caller, ConsumerNamespace, Namespace string
			Count                                uint64
			Methods                              []struct {
				Method string
				Count  uint64
			}
		}
		ExternalDependents []struct{ ID string }
	}
	var anon graph
	code, body := rolesDo(t, h, "GET", "/admin/graph?window=1m", "", "")
	if code != 200 || json.Unmarshal([]byte(body), &anon) != nil {
		t.Fatalf("%d %s", code, body)
	}
	if len(anon.Nodes) != 1 || anon.Nodes[0].ID != "greeter/v1" || !anon.Nodes[0].Public {
		t.Fatalf("nodes=%+v, want greeter/v1 only", anon.Nodes)
	}
	n := anon.Nodes[0]
	if n.Owner.Contact != "" || n.Instances[0].Addr != "" || len(n.Instances[0].DrainReasons) != 0 {
		t.Errorf("node not scrubbed: %+v", n)
	}
	if n.TrafficCount != 4 {
		t.Errorf("traffic=%d, want 4 (hidden callers still count)", n.TrafficCount)
	}
	if strings.Contains(n.SunsetReason, "acme") || strings.Contains(n.SunsetReason, "library") || strings.Contains(n.SunsetReason, "billing") {
		t.Errorf("sunset reason names a hidden consumer: %q", n.SunsetReason)
	}
	if len(anon.DesiredEdges) != 0 {
		t.Errorf("desired=%+v, want none (every declared edge has a hidden end)", anon.DesiredEdges)
	}
	if len(anon.ExternalDependents) != 0 {
		t.Errorf("externalDependents=%+v, want none", anon.ExternalDependents)
	}
	got := map[string]uint64{}
	for _, e := range anon.ActiveEdges {
		if e.Namespace != "greeter" {
			t.Errorf("active edge into hidden %+v", e)
		}
		if e.Caller == hiddenCaller && e.ConsumerNamespace != "" {
			t.Errorf("hidden edge names consumer %+v", e)
		}
		got[e.Caller] += e.Count
	}
	if len(got) != 2 || got[hiddenCaller] != 3 || got["greeter"] != 1 {
		t.Errorf("active by caller=%v, want hidden=3 greeter=1", got)
	}

	var dev graph
	_, body = rolesDo(t, h, "GET", "/admin/graph?window=1m", "", bearerHex(rolesDevTok))
	if err := json.Unmarshal([]byte(body), &dev); err != nil {
		t.Fatal(err)
	}
	if len(dev.Nodes) != 4 || len(dev.ExternalDependents) != 1 || len(dev.DesiredEdges) != 3 {
		t.Errorf("dev graph nodes=%d externals=%d desired=%d, want 4/1/3", len(dev.Nodes), len(dev.ExternalDependents), len(dev.DesiredEdges))
	}

	// A second greeter line: v1 is held by library and acme, which
	// the anonymous reason names without naming them.
	_, addr := startGreeterServer(t)
	v2 := greeterBinding(t, "v2")
	v2.Public = true
	registerOn(t, g, &cpv1.RegisterRequest{Addr: addr, InstanceId: "greeter-1", Services: []*cpv1.ServiceBinding{v2}})
	anon = graph{}
	_, body = rolesDo(t, h, "GET", "/admin/graph?window=1m", "", "")
	if err := json.Unmarshal([]byte(body), &anon); err != nil {
		t.Fatal(err)
	}
	var reason string
	for _, n := range anon.Nodes {
		if n.ID == "greeter/v1" {
			reason = n.SunsetReason
		}
	}
	if !strings.Contains(reason, "a hidden service accepts greeter [v1]") || !strings.Contains(reason, "an external dependent accepts greeter [v1]") ||
		strings.Contains(reason, "library") || strings.Contains(reason, "acme") {
		t.Errorf("anonymous greeter/v1 reason=%q", reason)
	}
}

func TestAdminRoles_AnonymousStats(t *testing.T) {
	g := newRolesGateway(t)
	h := newAnonAdminRouter(t, g)
	for _, path := range []string{"/admin/services/stats?window=1m", "/admin/services/history?window=1m", "/admin/services/usage?window=1m"} {
		code, body := rolesDo(t, h, "GET", path, "", "")
		if code != 200 {
			t.Fatalf("%s: %d %s", path, code, body)
		}
		for _, hidden := range []string{`"library"`, `"billing"`, `billing/v1`} {
			if strings.Contains(body, hidden) {
				t.Errorf("%s names %s: %s", path, hidden, body)
			}
		}
		if !strings.Contains(body, `"greeter"`) {
			t.Errorf("%s misses greeter: %s", path, body)
		}
	}
	if code, _ := rolesDo(t, h, "GET", "/admin/services/library/v1/stats?window=1m", "", ""); code != http.StatusNotFound {
		t.Errorf("anonymous stats of a hidden service: %d, want 404", code)
	}
	if code, _ := rolesDo(t, h, "GET", "/admin/services/library/v1/stats?window=1m", "", bearerHex(rolesDevTok)); code != 200 {
		t.Errorf("dev stats of a hidden service: %d, want 200", code)
	}
	var stats struct {
		Methods []struct {
			Method, Caller string
			Count          uint64
		}
	}
	_, body := rolesDo(t, h, "GET", "/admin/services/greeter/v1/stats?window=1m", "", "")
	if err := json.Unmarshal([]byte(body), &stats); err != nil {
		t.Fatal(err)
	}
	got := map[string]uint64{}
	for _, m := range stats.Methods {
		got[m.Method+" "+m.Caller] += m.Count
	}
	if len(stats.Methods) != 2 || got["Hello "+hiddenCaller] != 3 || got["Bye greeter"] != 1 {
		t.Errorf("greeter stats=%+v, want hidden callers folded", stats.Methods)
	}

	// Deprecated: library/v1 hidden, greeter/v1 public.
	cp := g.ControlPlane()
	for _, ns := range []string{"greeter", "library"} {
		if _, err := cp.Deprecate(context.Background(), &cpv1.DeprecateRequest{Namespace: ns, Version: "v1", Reason: "old"}); err != nil {
			t.Fatal(err)
		}
	}
	_, body = rolesDo(t, h, "GET", "/admin/services/deprecated/stats?window=1m", "", "")
	if strings.Contains(body, `"library"`) || strings.Contains(body, "billing") || !strings.Contains(body, `"greeter"`) {
		t.Errorf("deprecated stats: %s", body)
	}
}

func TestAdminRoles_PublicServicesAll(t *testing.T) {
	g := newRolesGateway(t, WithPublicServices("all"))
	h := newAnonAdminRouter(t, g)
	var anon rolesServices
	_, body := rolesDo(t, h, "GET", "/admin/services", "", "")
	if err := json.Unmarshal([]byte(body), &anon); err != nil {
		t.Fatal(err)
	}
	if len(anon.Services) != 4 {
		t.Fatalf("services=%+v, want all 4 (ps included)", anon.Services)
	}
	for _, s := range anon.Services {
		if !s.Public {
			t.Errorf("%s public=false", s.Namespace)
		}
		if s.Owner != nil && s.Owner.Contact != "" {
			t.Errorf("%s contact shown to anonymous", s.Namespace)
		}
	}
	// External dependents stay hidden; the dev-only reads stay closed.
	_, body = rolesDo(t, h, "GET", "/admin/graph", "", "")
	if strings.Contains(body, "acme") {
		t.Errorf("graph shows acme: %s", body)
	}
	if code, _ := rolesDo(t, h, "GET", "/admin/external-dependents", "", ""); code != http.StatusUnauthorized {
		t.Errorf("external dependents anonymous: %d", code)
	}
}

func TestAdminRoles_PublicServicesByName(t *testing.T) {
	g := newRolesGateway(t, WithPublicServices("library"))
	h := newAnonAdminRouter(t, g)
	var anon rolesServices
	_, body := rolesDo(t, h, "GET", "/admin/services", "", "")
	if err := json.Unmarshal([]byte(body), &anon); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range anon.Services {
		got[s.Namespace] = true
	}
	if len(got) != 2 || !got["greeter"] || !got["library"] {
		t.Fatalf("services=%v, want greeter + library", got)
	}
}

func TestAdminRoles_AsPublicBootTime(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	if err := g.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), To(nopGRPCConn{}), As("shown"), AsPublic()); err != nil {
		t.Fatal(err)
	}
	if err := g.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), To(nopGRPCConn{}), As("hidden")); err != nil {
		t.Fatal(err)
	}
	var anon rolesServices
	_, body := rolesDo(t, newAnonAdminRouter(t, g), "GET", "/admin/services", "", "")
	if err := json.Unmarshal([]byte(body), &anon); err != nil {
		t.Fatal(err)
	}
	if len(anon.Services) != 1 || anon.Services[0].Namespace != "shown" {
		t.Fatalf("services=%+v, want shown only", anon.Services)
	}
}

// The self-ingested admin GraphQL surface reaches /admin over HTTP
// with the caller's Authorization header, so the same rules hold
// there: anonymous fields are filtered, dev-only fields error, dev
// writes external dependents, admin-only mutations refuse dev.
func TestAdminRoles_GraphQLPath(t *testing.T) {
	g := newRolesGateway(t)
	mux, spec, err := g.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.AdminMiddleware(mux))
	t.Cleanup(srv.Close)
	if err := g.AddOpenAPIBytes(spec, As("admin"), To(srv.URL)); err != nil {
		t.Fatal(err)
	}
	gql := func(q, authz string) (data map[string]any, errs []any) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"query": q})
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rr := httptest.NewRecorder()
		g.Handler().ServeHTTP(rr, req)
		var out struct {
			Data   map[string]any `json:"data"`
			Errors []any          `json:"errors"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("%v: %s", err, rr.Body.String())
		}
		return out.Data, out.Errors
	}
	str := func(v any) string { b, _ := json.Marshal(v); return string(b) }

	const services = `{ admin { listServices { services { namespace owner { team contact } } } } }`
	data, errs := gql(services, "")
	if len(errs) > 0 || strings.Contains(str(data), "library") || strings.Contains(str(data), "#catalog") || !strings.Contains(str(data), "greeter") {
		t.Errorf("anonymous listServices: %s %v", str(data), errs)
	}
	data, errs = gql(services, bearerHex(rolesDevTok))
	if len(errs) > 0 || !strings.Contains(str(data), "library") || !strings.Contains(str(data), "#catalog") {
		t.Errorf("dev listServices: %s %v", str(data), errs)
	}

	const graph = `{ admin { dependencyGraph(window: "1m") { nodes { id } activeEdges { caller } externalDependents { id } } } }`
	data, errs = gql(graph, "")
	if len(errs) > 0 || strings.Contains(str(data), "acme") || strings.Contains(str(data), "library") || strings.Contains(str(data), "billing") {
		t.Errorf("anonymous graph: %s %v", str(data), errs)
	}

	// Dev-only reads, including the schema export of the admin MCP
	// surface, are errors without a token.
	for _, q := range []string{
		`{ admin { listExternalDependents { externalDependents { id } } } }`,
		`{ admin { mcpSchemaList { entries { path } } } }`,
		`{ admin { listInstances { instances { addr } } } }`,
	} {
		if _, errs := gql(q, ""); len(errs) == 0 {
			t.Errorf("anonymous %s: no error", q)
		}
		if _, errs := gql(q, bearerHex(rolesDevTok)); len(errs) > 0 {
			t.Errorf("dev %s: %v", q, errs)
		}
	}

	const put = `mutation { admin { putExternalDependent(id: "beta", body: {kind: "partner", reviewAt: "2099-01-01T00:00:00Z", depends: [{service: "greeter", lines: ["v1"]}]}) { externalDependent { id } } } }`
	if _, errs := gql(put, ""); len(errs) == 0 {
		t.Error("anonymous putExternalDependent: no error")
	}
	if _, errs := gql(put, bearerHex(rolesDevTok)); len(errs) > 0 {
		t.Errorf("dev putExternalDependent: %v", errs)
	}
	const deprecate = `mutation { admin { deprecate(namespace: "greeter", version: "v1", body: {reason: "x"}) { __typename } } }`
	if _, errs := gql(deprecate, bearerHex(rolesDevTok)); len(errs) == 0 {
		t.Error("dev deprecate: no error")
	}
	if _, errs := gql(deprecate, bearerHex(rolesAdminTok)); len(errs) > 0 {
		t.Errorf("admin deprecate: %v", errs)
	}
}

// admin_events_watchServices applies the same view: a subscriber with
// no token gets events for public services only (addr blanked); the
// dev token, in the connection_init payload, gets every event.
func TestAdminEvents_Roles(t *testing.T) {
	f := newSubFixture(t, WithDevToken([]byte(rolesDevTok)))
	defer f.close()
	if err := f.gw.AddAdminEvents(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	subscribe := func(init map[string]any) *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.Dial(ctx, f.wsURL, &websocket.DialOptions{Subprotocols: []string{"graphql-transport-ws"}})
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(init)
		if err := writeWS(conn, ctx, wsMessage{Type: "connection_init", Payload: payload}); err != nil {
			t.Fatal(err)
		}
		if ack, err := readWS(conn, ctx); err != nil || ack.Type != "connection_ack" {
			t.Fatalf("ack: %v %+v", err, ack)
		}
		sub, _ := json.Marshal(subscribePayload{Query: `subscription { admin_events_watchServices(namespace: "") { namespace addr } }`})
		if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: "subscribe", Payload: sub}); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	anon := subscribe(map[string]any{})
	defer anon.Close(websocket.StatusNormalClosure, "")
	dev := subscribe(map[string]any{"authorization": "Bearer " + hex.EncodeToString([]byte(rolesDevTok))})
	defer dev.Close(websocket.StatusNormalClosure, "")
	waitUntil(t, 5*time.Second, "broker subjects", func() bool { return f.gw.subscriptionBroker().activeSubjectCount() > 0 })
	// Both subscribers share one subject; wait for both consumers.
	waitUntil(t, 5*time.Second, "two consumers", func() bool {
		for _, s := range f.gw.activeSubjects() {
			if s.Consumers >= 2 {
				return true
			}
		}
		return false
	})

	if err := f.gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), To(nopGRPCConn{}), As("secret")); err != nil {
		t.Fatal(err)
	}
	if err := f.gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), To(nopGRPCConn{}), As("shown"), AsPublic()); err != nil {
		t.Fatal(err)
	}
	next := func(c *websocket.Conn) map[string]any {
		t.Helper()
		msg, err := readWS(c, ctx)
		if err != nil || msg.Type != "next" {
			t.Fatalf("read: %v %+v", err, msg)
		}
		var r struct {
			Data struct {
				E map[string]any `json:"admin_events_watchServices"`
			} `json:"data"`
		}
		_ = json.Unmarshal(msg.Payload, &r)
		return r.Data.E
	}
	if e := next(anon); e["namespace"] != "shown" || e["addr"] != "" {
		t.Errorf("anonymous first event=%v, want shown with addr blanked (secret filtered)", e)
	}
	if e := next(dev); e["namespace"] != "secret" {
		t.Errorf("dev first event=%v, want secret", e)
	}
	if e := next(dev); e["namespace"] != "shown" {
		t.Errorf("dev second event=%v, want shown", e)
	}
}

func TestAdminRoles_DevTokenPersistAndRotate(t *testing.T) {
	dir := t.TempDir()
	g := New(WithoutMetrics(), WithoutBackpressure(), WithAdminDataDir(dir))
	first := g.DevTokenHex()
	g.Close()
	if b, err := os.ReadFile(filepath.Join(dir, devTokenFilename)); err != nil || strings.TrimSpace(string(b)) != first {
		t.Fatalf("dev-token file=%q %v, want %s", b, err, first)
	}
	if first == "" || first == hex.EncodeToString(New(WithoutMetrics(), WithoutBackpressure(), WithAdminDataDir(dir)).AdminToken()) {
		t.Fatal("dev token empty or equal to the admin token")
	}

	g = New(WithoutMetrics(), WithoutBackpressure(), WithAdminDataDir(dir))
	t.Cleanup(g.Close)
	if g.DevTokenHex() != first {
		t.Fatalf("dev token not reloaded: %s vs %s", g.DevTokenHex(), first)
	}
	h := newAnonAdminRouter(t, g)
	devAuth := "Bearer " + first
	adminAuth := "Bearer " + g.AdminTokenHex()
	if code, _ := rolesDo(t, h, "POST", "/admin/dev-token/rotate", "", devAuth); code != http.StatusForbidden {
		t.Fatalf("dev rotating: %d, want 403", code)
	}
	code, body := rolesDo(t, h, "POST", "/admin/dev-token/rotate", "", adminAuth)
	var out struct{ DevToken string }
	if code != 200 || json.Unmarshal([]byte(body), &out) != nil || out.DevToken == "" || out.DevToken == first {
		t.Fatalf("rotate: %d %s", code, body)
	}
	if code, _ := rolesDo(t, h, "GET", "/admin/peers", "", devAuth); code != http.StatusUnauthorized {
		t.Errorf("old dev token after rotate: %d, want 401", code)
	}
	if code, _ := rolesDo(t, h, "GET", "/admin/peers", "", "Bearer "+out.DevToken); code != 200 {
		t.Errorf("new dev token: %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, devTokenFilename)); strings.TrimSpace(string(b)) != out.DevToken {
		t.Errorf("rotated token not persisted: %q", b)
	}

	pinned := New(WithoutMetrics(), WithoutBackpressure(), WithDevToken([]byte("x")))
	t.Cleanup(pinned.Close)
	if _, err := pinned.RotateDevToken(); err == nil {
		t.Error("rotating a pinned dev token: no error")
	}
}

// A delegate grants a role: unset = admin (delegates written before
// roles), DEV = dev.
func TestAdminRoles_DelegateRole(t *testing.T) {
	for _, tc := range []struct {
		role       aav1.AdminRole
		wantWrite  int
		wantDevOps int
	}{
		{aav1.AdminRole_ADMIN_ROLE_UNSPECIFIED, 200, 200},
		{aav1.AdminRole_ADMIN_ROLE_ADMIN, 200, 200},
		{aav1.AdminRole_ADMIN_ROLE_DEV, 403, 200},
	} {
		g := newAdminAuthGateway(t, []byte("tok"))
		withDelegate(t, g, &fakeAdminAuthConn{handler: func(*aav1.AuthorizeRequest) (*aav1.AuthorizeResponse, error) {
			return &aav1.AuthorizeResponse{Code: aav1.AdminAuthCode_ADMIN_AUTH_CODE_OK, Role: tc.role}, nil
		}})
		h := newAnonAdminRouter(t, g)
		if code, body := rolesDo(t, h, "POST", "/admin/mcp/auto-include", `{"autoInclude":false}`, "Bearer anything"); code != tc.wantWrite {
			t.Errorf("%s: admin write %d, want %d (%s)", tc.role, code, tc.wantWrite, body)
		}
		if code, _ := rolesDo(t, h, "GET", "/admin/peers", "", "Bearer anything"); code != tc.wantDevOps {
			t.Errorf("%s: dev read %d, want %d", tc.role, code, tc.wantDevOps)
		}
	}
}

type roleMetrics struct {
	noopMetrics
	mu  sync.Mutex
	got map[string]int
}

func (m *roleMetrics) RecordAdminAuthRole(method, outcome, role string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.got == nil {
		m.got = map[string]int{}
	}
	m.got[method+" "+outcome+" "+role]++
}

func TestAdminRoles_MetricsRoleLabel(t *testing.T) {
	m := &roleMetrics{}
	g := New(WithMetrics(m), WithoutBackpressure(), WithAdminToken([]byte(rolesAdminTok)), WithDevToken([]byte(rolesDevTok)))
	t.Cleanup(g.Close)
	h := newAnonAdminRouter(t, g)
	rolesDo(t, h, "GET", "/admin/services", "", "")                                                     // open read, no bearer: not recorded
	rolesDo(t, h, "GET", "/admin/peers", "", bearerHex(rolesDevTok))                                    // ok_bearer dev
	rolesDo(t, h, "POST", "/admin/drain", `{"timeoutSeconds":1}`, bearerHex(rolesDevTok))               // forbidden_role dev
	rolesDo(t, h, "POST", "/admin/mcp/auto-include", `{"autoInclude":false}`, bearerHex(rolesAdminTok)) // ok_bearer admin
	rolesDo(t, h, "GET", "/admin/peers", "", "")                                                        // denied_bearer anonymous
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[string]int{
		"GET ok_bearer dev":           1,
		"POST forbidden_role dev":     1,
		"POST ok_bearer admin":        1,
		"GET denied_bearer anonymous": 1,
	}
	for k, n := range want {
		if m.got[k] != n {
			t.Errorf("%s = %d, want %d (all: %v)", k, m.got[k], n, m.got)
		}
	}
	if len(m.got) != len(want) {
		t.Errorf("recorded %v, want exactly %v", m.got, want)
	}
}
