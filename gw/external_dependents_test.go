package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// External dependents (external_dependents.go).

func adminDo(t *testing.T, g *Gateway, method, path, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	newAdminRouter(t, g).ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func putExternal(t *testing.T, g *Gateway, id, body string) string {
	t.Helper()
	code, out := adminDo(t, g, http.MethodPut, "/admin/external-dependents/"+id, body)
	if code != http.StatusOK {
		t.Fatalf("PUT %s: %d %s", id, code, out)
	}
	return out
}

const acmeV1 = `{"name":"Acme Corp","kind":"customer","owner":{"team":"sales-eng","contact":"acme@example.com"},"depends":[{"service":"greeter","lines":["v1"]}],"note":"contract 2026-14"}`

func newExternalGateway(t *testing.T, opts ...Option) *Gateway {
	t.Helper()
	g := New(append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok"))}, opts...)...)
	t.Cleanup(g.Close)
	return g
}

type graphExternalEdge struct {
	ConsumerKind, ConsumerNamespace, ConsumerVersion, Provider string
	Lines, PresentLines                                        []string
	Met                                                        bool
}

func graphExternalEdges(t *testing.T, g *Gateway) []graphExternalEdge {
	t.Helper()
	code, body := adminDo(t, g, http.MethodGet, "/admin/graph", "")
	if code != http.StatusOK {
		t.Fatalf("graph: %d %s", code, body)
	}
	var out struct {
		DesiredEdges []graphExternalEdge `json:"desiredEdges"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	var ext []graphExternalEdge
	for _, e := range out.DesiredEdges {
		if e.ConsumerKind == "external" {
			ext = append(ext, e)
		}
	}
	return ext
}

func TestExternalDependents_SunsetRule(t *testing.T) {
	_, a1 := startGreeterServer(t)
	_, a2 := startGreeterServer(t)
	g := newExternalGateway(t)
	registerOn(t, g, greeterReq(t, a1, "greeter-v1", "v1"))
	registerOn(t, g, greeterReq(t, a2, "greeter-v2", "v2"))

	// No dependents: both lines sunset-able (the rule is unchanged).
	if n := getGraph(t, g, "").node(t, "greeter/v1"); !n.Sunsetable {
		t.Fatalf("greeter/v1 before acme: %s", n.SunsetReason)
	}

	putExternal(t, g, "acme", acmeV1)
	n := getGraph(t, g, "").node(t, "greeter/v1")
	if n.Sunsetable || !strings.Contains(n.SunsetReason, "external acme (Acme Corp) accepts greeter [v1]") {
		t.Fatalf("greeter/v1 with acme [v1]=%+v", n)
	}
	if n := getGraph(t, g, "").node(t, "greeter/v2"); !n.Sunsetable {
		t.Errorf("greeter/v2: acme does not accept it, want sunsetable: %s", n.SunsetReason)
	}
	edges := graphExternalEdges(t, g)
	if len(edges) != 1 || edges[0].ConsumerNamespace != "acme" || edges[0].ConsumerVersion != "" ||
		edges[0].Provider != "greeter" || !edges[0].Met {
		t.Fatalf("external edges=%+v", edges)
	}

	// Acme accepts v2 too, and v2 is ready: v1 may go.
	putExternal(t, g, "acme", strings.Replace(acmeV1, `["v1"]`, `["v1","v2"]`, 1))
	if n := getGraph(t, g, "").node(t, "greeter/v1"); !n.Sunsetable {
		t.Errorf("greeter/v1 with acme [v1,v2]: %s", n.SunsetReason)
	}

	// Deleted: no edge, no block.
	putExternal(t, g, "acme", acmeV1)
	if code, body := adminDo(t, g, http.MethodDelete, "/admin/external-dependents/acme", ""); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, _ := adminDo(t, g, http.MethodDelete, "/admin/external-dependents/acme", ""); code != http.StatusNotFound {
		t.Errorf("second delete: %d, want 404", code)
	}
	if e := graphExternalEdges(t, g); len(e) != 0 {
		t.Errorf("edges after delete=%+v", e)
	}
	if n := getGraph(t, g, "").node(t, "greeter/v1"); !n.Sunsetable {
		t.Errorf("greeter/v1 after delete: %s", n.SunsetReason)
	}
}

func TestExternalDependents_UnmetMetric(t *testing.T) {
	g := New(WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(g.Close)
	out := putExternal(t, g, "acme", `{"kind":"partner","depends":[{"service":"ghost","lines":["v1"]}]}`)
	if !strings.Contains(out, `"warnings":["ghost: no line is registered"]`) {
		t.Errorf("put response has no warning for an unknown service: %s", out)
	}
	edges := graphExternalEdges(t, g)
	if len(edges) != 1 || edges[0].Met {
		t.Fatalf("edges=%+v, want one unmet", edges)
	}
	wantMetric(t, g, 1, "dependency_unmet", "consumer_kind", "external", "consumer_namespace", "acme", "consumer_version", "", "provider", "ghost")
	if code, _ := adminDo(t, g, http.MethodDelete, "/admin/external-dependents/acme", ""); code != http.StatusOK {
		t.Fatal("delete failed")
	}
	wantNoSeries(t, g, "dependency_unmet", "consumer_namespace", "acme")
}

func TestExternalDependents_BlocksDrain(t *testing.T) {
	_, addrA := startGreeterServer(t)
	_, addrB := startGreeterServer(t)
	g := newExternalGateway(t)
	registerOn(t, g, greeterReq(t, addrA, "greeter-v1", "v1"))
	putExternal(t, g, "acme", acmeV1)

	resp := drainRPC(t, g, "greeter-v1", false)
	if resp.GetState() != cpv1.DrainState_DRAIN_STATE_BLOCKED {
		t.Fatalf("state=%v, want BLOCKED", resp.GetState())
	}
	if r := resp.GetReasons(); len(r) != 1 || !strings.Contains(r[0], "external acme (Acme Corp) accepts greeter [v1]") {
		t.Fatalf("reasons=%v", r)
	}

	// Acme moves to [v1, v2] and v2 is ready: the drain proceeds.
	registerOn(t, g, greeterReq(t, addrB, "greeter-v2", "v2"))
	putExternal(t, g, "acme", strings.Replace(acmeV1, `["v1"]`, `["v1","v2"]`, 1))
	waitDrainState(t, g, "greeter-v1", cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP, 5*time.Second)

	// The v2 instance is now acme's only cover.
	resp = drainRPC(t, g, "greeter-v2", false)
	if resp.GetState() != cpv1.DrainState_DRAIN_STATE_BLOCKED || !strings.Contains(strings.Join(resp.GetReasons(), ";"), "external acme") {
		t.Fatalf("greeter-v2: %v %v", resp.GetState(), resp.GetReasons())
	}
	// Deleting the declaration releases it.
	if code, _ := adminDo(t, g, http.MethodDelete, "/admin/external-dependents/acme", ""); code != http.StatusOK {
		t.Fatal("delete failed")
	}
	waitDrainState(t, g, "greeter-v2", cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP, 5*time.Second)
}

func TestExternalDependents_DoesNotGateStart(t *testing.T) {
	g := newExternalGateway(t)
	putExternal(t, g, "acme", `{"kind":"app","depends":[{"service":"greeter","lines":["v1"]}]}`)
	_, addr := startGreeterServer(t)
	resp, err := g.ControlPlane().Register(context.Background(), greeterReq(t, addr, "greeter-v1", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range resp.GetBindings() {
		if b.GetState() != cpv1.InstanceState_INSTANCE_STATE_READY {
			t.Errorf("binding %s/%s state=%v, want READY", b.GetNamespace(), b.GetVersion(), b.GetState())
		}
	}
}

func TestExternalDependents_Expiry(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	old := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	_, addrA := startGreeterServer(t)
	g := newExternalGateway(t)
	registerOn(t, g, greeterReq(t, addrA, "greeter-v1", "v1"))
	putExternal(t, g, "acme", strings.Replace(acmeV1, `"note"`, `"expiresAt":"2026-11-08T00:00:00Z","note"`, 1))

	if r := drainRPC(t, g, "greeter-v1", false); r.GetState() != cpv1.DrainState_DRAIN_STATE_BLOCKED {
		t.Fatalf("before expiry: %v", r.GetState())
	}
	if e := graphExternalEdges(t, g); len(e) != 1 {
		t.Fatalf("before expiry edges=%+v", e)
	}

	now = now.Add(24 * time.Hour)
	if e := graphExternalEdges(t, g); len(e) != 0 {
		t.Errorf("after expiry edges=%+v", e)
	}
	_, list := adminDo(t, g, http.MethodGet, "/admin/external-dependents", "")
	if !strings.Contains(list, `"expired":true`) || !strings.Contains(list, `"expiresAt":"2026-11-08T00:00:00Z"`) {
		t.Errorf("expired declaration not listed as expired: %s", list)
	}
	waitDrainState(t, g, "greeter-v1", cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP, 5*time.Second)
}

func TestExternalDependents_Validation(t *testing.T) {
	g := newExternalGateway(t)
	code, body := adminDo(t, g, http.MethodPut, "/admin/external-dependents/acme",
		`{"kind":"vendor","depends":[{"service":"greeter","lines":["v01","stable"]},{"service":"greeter","lines":[]}],"expiresAt":"tomorrow"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", code, body)
	}
	for _, loc := range []string{"body.kind", "body.depends[0].lines", "body.depends[1].service", "body.depends[1].lines", "body.expiresAt"} {
		if !strings.Contains(body, `"location":"`+loc+`"`) {
			t.Errorf("no %s detail in %s", loc, body)
		}
	}
	code, body = adminDo(t, g, http.MethodPut, "/admin/external-dependents/Acme%20Corp", `{"kind":"customer","depends":[{"service":"greeter","lines":["v1"]}]}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"location":"path.id"`) {
		t.Errorf("bad id: %d %s", code, body)
	}
	code, body = adminDo(t, g, http.MethodPut, "/admin/external-dependents/acme", `{"kind":"customer","depends":[]}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"location":"body.depends"`) {
		t.Errorf("no depends: %d %s", code, body)
	}
	if len(g.externals.all()) != 0 {
		t.Errorf("invalid declarations stored: %+v", g.externals.all())
	}

	// Writes need the admin token.
	req := httptest.NewRequest(http.MethodPut, "/admin/external-dependents/acme", strings.NewReader(acmeV1))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newAdminRouter(t, g).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("PUT without token: %d", rr.Code)
	}

	// Lines canonicalise; "unstable" is a line.
	out := putExternal(t, g, "acme", `{"kind":"customer","depends":[{"service":"greeter","lines":["v1","v1","unstable"]}]}`)
	if !strings.Contains(out, `"lines":["v1","unstable"]`) {
		t.Errorf("lines not canonical: %s", out)
	}
}

// Validation errors reach GraphQL clients as extensions.fields through
// the self-ingested admin API (upstream_fields.go).
func TestExternalDependents_GraphQLFieldErrors(t *testing.T) {
	g := newExternalGateway(t)
	mux, spec, err := g.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.AdminMiddleware(mux))
	t.Cleanup(srv.Close)
	if err := g.AddOpenAPIBytes(spec, As("admin"), To(srv.URL), ForwardHeaders("Authorization")); err != nil {
		t.Fatal(err)
	}
	q := `{"query":"mutation { admin { putExternalDependent(id:\"acme\", body:{kind:\"customer\", depends:[{service:\"greeter\", lines:[\"v0\"]}]}) { warnings } } }"}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(q))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	g.Handler().ServeHTTP(rr, req)
	var out struct {
		Errors []struct {
			Extensions struct {
				Fields []struct{ Path, Message string } `json:"fields"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, rr.Body.String())
	}
	if len(out.Errors) != 1 || len(out.Errors[0].Extensions.Fields) != 1 || out.Errors[0].Extensions.Fields[0].Path != "body.depends.0.lines" {
		t.Fatalf("errors=%s", rr.Body.String())
	}
}

// Cluster: a declaration made on one node counts on the other, in its
// graph and its drain coverage; a delete reaches it too. The bucket's
// replicas follow the peer count like the other buckets.
func TestExternalDependents_ClusterReplication(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	for _, n := range []*clusterNode{a, b} {
		n.gw.cfg.adminToken = []byte("tok")
	}
	waitForPeers(t, a.gw, b.gw)

	putExternal(t, a.gw, "acme", acmeV1)
	waitUntil(t, 10*time.Second, "acme on node B", func() bool {
		_, ok := b.gw.externals.get("acme")
		return ok
	})
	if e := graphExternalEdges(t, b.gw); len(e) != 1 || e[0].ConsumerNamespace != "acme" {
		t.Fatalf("node B edges=%+v", e)
	}

	// Replicas raised with the peer count.
	waitUntil(t, 15*time.Second, "external dependents bucket at R=2", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s, err := testCluster.a.JS.Stream(ctx, "KV_"+externalDependentsBucketName)
		if err != nil {
			return false
		}
		info, err := s.Info(ctx)
		return err == nil && info.Config.Replicas == 2
	})

	// A gateway that starts later replays the bucket.
	c := newSharedClusterNode(t, testCluster.b)
	waitUntil(t, 10*time.Second, "acme replayed on a late gateway", func() bool {
		_, ok := c.gw.externals.get("acme")
		return ok
	})

	if code, body := adminDo(t, b.gw, http.MethodDelete, "/admin/external-dependents/acme", ""); code != http.StatusOK {
		t.Fatalf("delete on B: %d %s", code, body)
	}
	waitUntil(t, 10*time.Second, "acme gone on node A", func() bool {
		_, ok := a.gw.externals.get("acme")
		return !ok
	})
}

func waitUntil(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
