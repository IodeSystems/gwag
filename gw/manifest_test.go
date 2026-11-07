package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Manifest fields (ServiceBinding.depends / owner) and the gateway
// environment name: stored on the slot, reported by ListServices and
// /admin/services, replicated across the cluster.

func greeterBinding(t *testing.T, ver string) *cpv1.ServiceBinding {
	t.Helper()
	return &cpv1.ServiceBinding{
		Namespace:   "greeter",
		Version:     ver,
		ProtoSource: testProtoBytes(t, "greeter.proto"),
	}
}

func libraryBinding(t *testing.T, ver string) *cpv1.ServiceBinding {
	t.Helper()
	return &cpv1.ServiceBinding{
		Namespace:   "library",
		Version:     ver,
		ProtoSource: testProtoBytes(t, "library.proto"),
	}
}

func listServiceInfo(t *testing.T, g *Gateway, ns, ver string) (*cpv1.ListServicesResponse, *cpv1.ServiceInfo) {
	t.Helper()
	resp, err := g.ControlPlane().ListServices(context.Background(), &cpv1.ListServicesRequest{})
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	for _, s := range resp.GetServices() {
		if s.GetNamespace() == ns && s.GetVersion() == ver {
			return resp, s
		}
	}
	return resp, nil
}

func TestManifest_StandaloneRegisterStoresDependsAndOwner(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure(), WithEnvironment("dev"))
	t.Cleanup(g.Close)

	b := greeterBinding(t, "v1")
	b.Depends = []*cpv1.Dependency{
		{Service: "library", Lines: []string{"v1", "v2"}},
		{Service: "users", Lines: []string{"unstable"}},
	}
	b.Owner = &cpv1.Owner{Team: "catalog", Contact: "#catalog"}
	if _, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr:     addr,
		Services: []*cpv1.ServiceBinding{b},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	resp, info := listServiceInfo(t, g, "greeter", "v1")
	if info == nil {
		t.Fatalf("greeter/v1 missing: %v", resp)
	}
	if got := resp.GetEnvironment(); got != "dev" {
		t.Errorf("environment=%q, want dev", got)
	}
	var deps []string
	for _, d := range info.GetDepends() {
		deps = append(deps, d.GetService()+":"+strings.Join(d.GetLines(), ","))
	}
	if want := []string{"library:v1,v2", "users:unstable"}; !reflect.DeepEqual(deps, want) {
		t.Errorf("depends=%v, want %v", deps, want)
	}
	if o := info.GetOwner(); o.GetTeam() != "catalog" || o.GetContact() != "#catalog" {
		t.Errorf("owner=%v, want catalog/#catalog", o)
	}
}

// An old client sends no manifest: Register works as before and the
// slot reports no depends / owner.
func TestManifest_NoManifestStillRegisters(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	if _, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr:     addr,
		Services: []*cpv1.ServiceBinding{greeterBinding(t, "v1")},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	resp, info := listServiceInfo(t, g, "greeter", "v1")
	if info == nil {
		t.Fatalf("greeter/v1 missing")
	}
	if len(info.GetDepends()) != 0 || info.GetOwner() != nil {
		t.Errorf("depends=%v owner=%v, want none", info.GetDepends(), info.GetOwner())
	}
	if resp.GetEnvironment() != "" {
		t.Errorf("environment=%q, want empty", resp.GetEnvironment())
	}
}

func TestManifest_RejectsBadDepends(t *testing.T) {
	_, addr := startGreeterServer(t)
	cases := []struct {
		name string
		deps []*cpv1.Dependency
		want string
	}{
		{"empty service", []*cpv1.Dependency{{Lines: []string{"v1"}}}, "namespace is empty"},
		{"no lines", []*cpv1.Dependency{{Service: "library"}}, "at least one line"},
		{"bad line", []*cpv1.Dependency{{Service: "library", Lines: []string{"V2"}}}, "\"V2\""},
		{"stable line", []*cpv1.Dependency{{Service: "library", Lines: []string{"stable"}}}, "stable"},
		{"duplicate service", []*cpv1.Dependency{
			{Service: "library", Lines: []string{"v1"}},
			{Service: "library", Lines: []string{"v2"}},
		}, "listed twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := New(WithoutMetrics(), WithoutBackpressure())
			t.Cleanup(g.Close)
			b := greeterBinding(t, "v1")
			b.Depends = tc.deps
			_, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
				Addr:     addr,
				Services: []*cpv1.ServiceBinding{b},
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want containing %q", err, tc.want)
			}
			if _, info := listServiceInfo(t, g, "greeter", "v1"); info != nil {
				t.Errorf("rejected Register left a slot behind")
			}
		})
	}
}

// Latest registration on a slot wins: a second replica with a new
// owner replaces the reported manifest.
func TestManifest_LatestRegistrationWins(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	for _, team := range []string{"a", "b"} {
		b := greeterBinding(t, "v1")
		b.Owner = &cpv1.Owner{Team: team}
		if _, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
			Addr:     addr,
			Services: []*cpv1.ServiceBinding{b},
		}); err != nil {
			t.Fatalf("Register %s: %v", team, err)
		}
	}
	_, info := listServiceInfo(t, g, "greeter", "v1")
	if got := info.GetOwner().GetTeam(); got != "b" {
		t.Errorf("owner.team=%q, want b", got)
	}
	if got := info.GetReplicaCount(); got != 2 {
		t.Errorf("replicaCount=%d, want 2", got)
	}
}

func TestManifest_AdminServicesRoute(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")), WithEnvironment("staging"))
	t.Cleanup(g.Close)
	withManifest := greeterBinding(t, "v2")
	withManifest.Depends = []*cpv1.Dependency{{Service: "library", Lines: []string{"v1", "v2"}}}
	withManifest.Owner = &cpv1.Owner{Team: "catalog", Contact: "catalog@example.com"}
	if _, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr:     addr,
		Services: []*cpv1.ServiceBinding{greeterBinding(t, "v1"), withManifest},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	h := newAdminRouter(t, g)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/services", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Environment string `json:"environment"`
		Services    []struct {
			Namespace string           `json:"namespace"`
			Version   string           `json:"version"`
			Depends   []map[string]any `json:"depends"`
			Owner     *struct {
				Team    string `json:"team"`
				Contact string `json:"contact"`
			} `json:"owner"`
		} `json:"services"`
	}
	raw := rr.Body.String()
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Environment != "staging" {
		t.Errorf("environment=%q, want staging", out.Environment)
	}
	greeters := out.Services[:0]
	for _, s := range out.Services {
		if s.Namespace == "greeter" {
			greeters = append(greeters, s)
		}
	}
	if len(greeters) != 2 {
		t.Fatalf("greeter services=%d, want 2: %s", len(greeters), raw)
	}
	v1, v2 := greeters[0], greeters[1]
	if v1.Version != "v1" || v2.Version != "v2" {
		t.Fatalf("order: %s", raw)
	}
	if v1.Depends == nil || len(v1.Depends) != 0 || v1.Owner != nil {
		t.Errorf("v1 depends=%v owner=%v, want [] and absent", v1.Depends, v1.Owner)
	}
	if !strings.Contains(raw, `"depends":[]`) {
		t.Errorf("v1 depends must serialise as [] (never null): %s", raw)
	}
	if len(v2.Depends) != 1 || v2.Depends[0]["service"] != "library" ||
		!reflect.DeepEqual(v2.Depends[0]["lines"], []any{"v1", "v2"}) {
		t.Errorf("v2 depends=%v", v2.Depends)
	}
	if v2.Owner == nil || v2.Owner.Team != "catalog" || v2.Owner.Contact != "catalog@example.com" {
		t.Errorf("v2 owner=%v", v2.Owner)
	}
}

// The admin OpenAPI is self-ingested into GraphQL (huma → OpenAPI →
// GraphQL); the UI reads the new fields from there.
func TestManifest_AdminGraphQLSelfIngest(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")), WithEnvironment("prod"))
	t.Cleanup(g.Close)
	b := greeterBinding(t, "v1")
	b.Depends = []*cpv1.Dependency{{Service: "library", Lines: []string{"v2"}}}
	b.Owner = &cpv1.Owner{Team: "catalog", Contact: "#catalog"}
	if _, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr:     addr,
		Services: []*cpv1.ServiceBinding{b},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	mux, spec, err := g.AdminHumaRouter()
	if err != nil {
		t.Fatalf("AdminHumaRouter: %v", err)
	}
	adminSrv := httptest.NewServer(mux)
	t.Cleanup(adminSrv.Close)
	if err := g.AddOpenAPIBytes(spec, As("admin"), To(adminSrv.URL)); err != nil {
		t.Fatalf("self-ingest: %v", err)
	}
	gqlSrv := httptest.NewServer(g.Handler())
	t.Cleanup(gqlSrv.Close)

	q := `{"query":"{ admin { listServices { environment services { namespace depends { service lines } owner { team contact } } } } }"}`
	resp, err := http.Post(gqlSrv.URL+"/graphql", "application/json", strings.NewReader(q))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Data struct {
			Admin struct {
				ListServices struct {
					Environment string `json:"environment"`
					Services    []struct {
						Namespace string `json:"namespace"`
						Depends   []struct {
							Service string   `json:"service"`
							Lines   []string `json:"lines"`
						} `json:"depends"`
						Owner struct {
							Team    string `json:"team"`
							Contact string `json:"contact"`
						} `json:"owner"`
					} `json:"services"`
				} `json:"listServices"`
			} `json:"admin"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if len(out.Errors) > 0 {
		t.Fatalf("graphql errors: %s", body)
	}
	ls := out.Data.Admin.ListServices
	if ls.Environment != "prod" {
		t.Errorf("environment=%q: %s", ls.Environment, body)
	}
	var found bool
	for _, s := range ls.Services {
		if s.Namespace != "greeter" {
			continue
		}
		found = true
		if len(s.Depends) != 1 || s.Depends[0].Service != "library" || !reflect.DeepEqual(s.Depends[0].Lines, []string{"v2"}) {
			t.Errorf("depends=%v", s.Depends)
		}
		if s.Owner.Team != "catalog" || s.Owner.Contact != "#catalog" {
			t.Errorf("owner=%v", s.Owner)
		}
	}
	if !found {
		t.Fatalf("greeter missing: %s", body)
	}
}

// Plan assumption: a binary that serves two lines of one service
// registers once with two ServiceBindings for the same namespace (v1
// and v2), and both lines become routable to the same addr.
func TestManifest_SingleRegisterTwoLinesStandalone(t *testing.T) {
	greeter, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	resp, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr:     addr,
		Services: []*cpv1.ServiceBinding{greeterBinding(t, "v1"), greeterBinding(t, "v2")},
	})
	if err != nil {
		t.Fatalf("Register v1+v2: %v", err)
	}
	for _, ver := range []string{"v1", "v2"} {
		_, info := listServiceInfo(t, g, "greeter", ver)
		if info == nil || info.GetReplicaCount() != 1 {
			t.Fatalf("greeter/%s info=%v, want 1 replica", ver, info)
		}
	}
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	assertGreeterLines(t, srv.URL)
	if got := greeter.helloCalls.Load(); got != 2 {
		t.Fatalf("helloCalls=%d, want 2", got)
	}

	// Deregister drops both lines.
	if _, err := g.ControlPlane().Deregister(context.Background(), &cpv1.DeregisterRequest{RegistrationId: resp.GetRegistrationId()}); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	for _, ver := range []string{"v1", "v2"} {
		if _, info := listServiceInfo(t, g, "greeter", ver); info != nil {
			t.Errorf("greeter/%s still listed after Deregister", ver)
		}
	}
}

// assertGreeterLines queries the latest line (flat, v2) and the older
// line (v1 sub-object) and checks both reach the backend.
func assertGreeterLines(t *testing.T, base string) {
	t.Helper()
	q := `{"query":"{ greeter { hello(name:\"two\") { greeting } v1 { hello(name:\"one\") { greeting } } } }"}`
	resp, err := http.Post(base+"/graphql", "application/json", strings.NewReader(q))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Data struct {
			Greeter struct {
				Hello struct {
					Greeting string `json:"greeting"`
				} `json:"hello"`
				V1 struct {
					Hello struct {
						Greeting string `json:"greeting"`
					} `json:"hello"`
				} `json:"v1"`
			} `json:"greeter"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if len(out.Errors) > 0 {
		t.Fatalf("graphql errors: %s", body)
	}
	if out.Data.Greeter.Hello.Greeting != "hello two" || out.Data.Greeter.V1.Hello.Greeting != "hello one" {
		t.Fatalf("greetings: %s", body)
	}
}

// Cluster: manifest rides the registry KV, so the peer that did not
// receive the Register reports it too; v1+v2 in one Register reaches
// both lines on the peer. library/v1 rides the same Register so v2's
// dependency is present and the start gate opens.
func TestManifest_ClusterReplication(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	waitForPeers(t, a.gw, b.gw)
	greeter, addr := startGreeterServer(t)

	v2 := greeterBinding(t, "v2")
	v2.Depends = []*cpv1.Dependency{{Service: "library", Lines: []string{"v1", "v2"}}}
	v2.Owner = &cpv1.Owner{Team: "catalog", Contact: "#catalog"}
	v2.Public = true
	req := &cpv1.RegisterRequest{
		Addr:     addr,
		Services: []*cpv1.ServiceBinding{greeterBinding(t, "v1"), v2, libraryBinding(t, "v1")},
	}
	regCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var lastErr error
	for regCtx.Err() == nil {
		if _, err := a.gw.ControlPlane().Register(regCtx, req); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("Register: %v", lastErr)
	}
	waitForPool(t, b.gw, "greeter", "v1", 30*time.Second)
	waitForPool(t, b.gw, "greeter", "v2", 30*time.Second)

	for _, n := range []*clusterNode{a, b} {
		_, info := listServiceInfo(t, n.gw, "greeter", "v2")
		if info == nil {
			t.Fatalf("greeter/v2 missing on node")
		}
		if d := info.GetDepends(); len(d) != 1 || d[0].GetService() != "library" || !reflect.DeepEqual(d[0].GetLines(), []string{"v1", "v2"}) {
			t.Errorf("depends=%v", d)
		}
		if info.GetOwner().GetTeam() != "catalog" {
			t.Errorf("owner=%v", info.GetOwner())
		}
		if !info.GetPublic() {
			t.Error("greeter/v2 public=false, want true (replicated with the manifest)")
		}
		_, v1info := listServiceInfo(t, n.gw, "greeter", "v1")
		if v1info == nil || len(v1info.GetDepends()) != 0 || v1info.GetOwner() != nil || v1info.GetPublic() {
			t.Errorf("v1 info=%v, want present without manifest", v1info)
		}
	}
	assertGreeterLines(t, b.httpSrv.URL)
	if got := greeter.helloCalls.Load(); got != 2 {
		t.Fatalf("helloCalls=%d, want 2", got)
	}
}
