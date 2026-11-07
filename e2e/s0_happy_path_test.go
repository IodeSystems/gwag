//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

const dispatchCount = "dispatch_duration_seconds_count"

// TestS0HappyPath boots one gateway with a proto, an OpenAPI and a
// downstream-GraphQL service, calls them through every ingress the
// gateway exposes for their kind, and checks the admin surface and
// that the dispatch and request metrics moved by exactly the calls
// made.
func TestS0HappyPath(t *testing.T) {
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{Environment: "e2e"})
	orders := env.Fixture(gw, harness.FixtureOpts{Namespace: "orders", Versions: []string{"v1", "v2"}, Owner: "commerce"})
	billing := env.Fixture(gw, harness.FixtureOpts{Kind: harness.KindOpenAPI, Namespace: "billing", Owner: "finance"})
	catalog := env.Fixture(gw, harness.FixtureOpts{Kind: harness.KindGraphQL, Namespace: "catalog", Owner: "books"})

	ctx := harness.Ctx(t, 2*time.Minute)
	before := gw.MustScrape(t)

	type call struct {
		desc    string
		f       *harness.Fixture
		version string // the line that must serve it
		caller  string
		ingress string // request_duration_seconds ingress label
		do      func() (harness.Echo, error)
	}
	gql := func(f *harness.Fixture, line, caller string) func() (harness.Echo, error) {
		return func() (harness.Echo, error) { return f.Echo(ctx, line, "hi", harness.Caller(caller)) }
	}
	calls := []call{
		{"graphql orders latest", orders, "v2", "s0/graphql", "graphql", gql(orders, "", "s0/graphql")},
		{"graphql orders v1", orders, "v1", "s0/graphql", "graphql", gql(orders, "v1", "s0/graphql")},
		{"graphql billing (openapi)", billing, "v1", "s0/graphql", "graphql", gql(billing, "", "s0/graphql")},
		{"graphql catalog (graphql)", catalog, "v1", "s0/graphql", "graphql", gql(catalog, "", "s0/graphql")},
		{"rest orders v1", orders, "v1", "s0/rest", "http", func() (harness.Echo, error) {
			return orders.EchoREST(ctx, "v1", "hi", harness.Caller("s0/rest"))
		}},
		{"rest billing v1", billing, "v1", "s0/rest", "http", func() (harness.Echo, error) {
			return billing.EchoREST(ctx, "v1", "hi", harness.Caller("s0/rest"))
		}},
		{"grpc orders v2", orders, "v2", "s0/grpc", "grpc", func() (harness.Echo, error) {
			return orders.EchoGRPC(ctx, "v2", "hi", "s0/grpc")
		}},
		// Connect reads the caller from gRPC metadata, which Connect
		// carries as the Caller-Id header; it ignores X-Caller-Id.
		{"connect orders v1", orders, "v1", "s0/connect", "connect", func() (harness.Echo, error) {
			return orders.EchoConnect(ctx, "v1", "hi", harness.Header("Caller-Id", "s0/connect"))
		}},
	}

	type key struct{ ns, ver, method, code, caller string }
	wantDispatch := map[key]float64{}
	wantIngress := map[string]float64{}
	for _, c := range calls {
		e, err := c.do()
		if err != nil {
			t.Fatalf("%s: %v", c.desc, err)
		}
		if e.Msg != "hi" || e.Namespace != c.f.Opts.Namespace || e.Version != c.version || e.Instance != c.f.Opts.Instance {
			t.Errorf("%s: got %+v, want msg hi from %s %s/%s", c.desc, e, c.f.Opts.Instance, c.f.Opts.Namespace, c.version)
		}
		wantDispatch[key{c.f.Opts.Namespace, c.version, c.f.MetricMethod(c.version), "ok", c.caller}]++
		wantIngress[c.ingress]++
	}

	// One failing call: the upstream answers gRPC Internal.
	orders.SetFaults(harness.Faults{ErrorRate: 1})
	if _, err := orders.Echo(ctx, "v1", "boom", harness.Caller("s0/fail")); err == nil || !strings.Contains(err.Error(), "injected error") {
		t.Fatalf("failing call: want the injected error, got %v", err)
	}
	orders.SetFaults(harness.Faults{})
	wantDispatch[key{"orders", "v1", orders.MetricMethod("v1"), "Internal", "s0/fail"}]++
	wantIngress["graphql"]++

	var wants []harness.Want
	var total float64
	for k, n := range wantDispatch {
		wants = append(wants, harness.Want{Labels: harness.Labels{
			"namespace": k.ns, "version": k.ver, "method": k.method, "code": k.code, "caller": k.caller,
		}, Delta: n})
		total += n
	}
	// Nothing else dispatched: the per-series wants add up to the total.
	wants = append(wants, harness.Want{Labels: nil, Delta: total})
	gw.WaitDeltas(t, before, dispatchCount, wants, 10*time.Second)

	var ingress []harness.Want
	for ing, n := range wantIngress {
		ingress = append(ingress, harness.Want{Labels: harness.Labels{"ingress": ing}, Delta: n})
	}
	gw.WaitDeltas(t, before, "request_duration_seconds_count", ingress, 10*time.Second)

	t.Run("admin services", func(t *testing.T) {
		ss, err := gw.Admin().Services(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ss.Environment != "e2e" {
			t.Errorf("environment %q, want e2e", ss.Environment)
		}
		for _, want := range []struct {
			f       *harness.Fixture
			version string
			team    string
		}{{orders, "v1", "commerce"}, {orders, "v2", "commerce"}, {billing, "v1", "finance"}, {catalog, "v1", "books"}} {
			ns := want.f.Opts.Namespace
			svc := ss.Service(ns, want.version)
			if svc == nil {
				t.Errorf("%s/%s not listed", ns, want.version)
				continue
			}
			if svc.ReplicaCount != 1 {
				t.Errorf("%s/%s replicaCount %d, want 1", ns, want.version, svc.ReplicaCount)
			}
			if svc.Owner == nil || svc.Owner.Team != want.team {
				t.Errorf("%s/%s owner %+v, want team %s", ns, want.version, svc.Owner, want.team)
			}
			if in := svc.Instance(want.f.Opts.Instance); in == nil || in.State != "ready" {
				t.Errorf("%s/%s instance %s: %+v, want ready", ns, want.version, want.f.Opts.Instance, in)
			}
		}

		// The same list through the self-ingested admin GraphQL surface.
		r := gw.MustGraphQL(t, `{ admin { listServices { environment services { namespace version replicaCount } } } }`, nil)
		var out struct {
			Admin struct {
				ListServices struct {
					Environment string
					Services    []struct {
						Namespace    string
						Version      string
						ReplicaCount int
					}
				}
			}
		}
		if err := r.Decode(&out); err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, s := range out.Admin.ListServices.Services {
			got[s.Namespace+"/"+s.Version] = s.ReplicaCount
		}
		for _, id := range []string{"orders/v1", "orders/v2", "billing/v1", "catalog/v1"} {
			if got[id] != 1 {
				t.Errorf("admin.listServices: %s replicaCount %d, want 1 (got %v)", id, got[id], got)
			}
		}
	})

	t.Run("admin graph", func(t *testing.T) {
		g, err := gw.Admin().Graph(ctx, "1m")
		if err != nil {
			t.Fatal(err)
		}
		if g.Environment != "e2e" || g.CallerSource != "header" {
			t.Errorf("graph environment %q callerSource %q, want e2e / header", g.Environment, g.CallerSource)
		}
		for id, kind := range map[string]string{"orders/v1": "proto", "orders/v2": "proto", "billing/v1": "openapi", "catalog/v1": "graphql"} {
			n := g.Node(id)
			if n == nil {
				t.Errorf("graph: no node %s", id)
				continue
			}
			if n.Kind != kind || n.ReadyCount != 1 || n.ReplicaCount != 1 {
				t.Errorf("graph node %s: kind %q ready %d replicas %d, want %s 1 1", id, n.Kind, n.ReadyCount, n.ReplicaCount, kind)
			}
		}
		for _, w := range []struct {
			caller, ns, ver string
			count, ok       uint64
		}{
			{"s0/graphql", "orders", "v2", 1, 1},
			{"s0/graphql", "orders", "v1", 1, 1},
			{"s0/rest", "billing", "v1", 1, 1},
			{"s0/fail", "orders", "v1", 1, 0},
		} {
			e := g.Edge(w.caller, w.ns, w.ver)
			if e == nil {
				t.Errorf("graph: no active edge %s -> %s/%s", w.caller, w.ns, w.ver)
				continue
			}
			if e.Count != w.count || e.OkCount != w.ok {
				t.Errorf("edge %s -> %s/%s: count %d ok %d, want %d %d", w.caller, w.ns, w.ver, e.Count, e.OkCount, w.count, w.ok)
			}
		}
	})

	t.Run("mcp", func(t *testing.T) {
		if err := gw.Admin().MCPInclude(ctx, "orders.**"); err != nil {
			t.Fatal(err)
		}
		c := gw.MCP(t)
		res, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
			Name: "schema_search", Arguments: map[string]any{"pathGlob": "orders.**"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if s := toolText(res); res.IsError || !strings.Contains(s, "echo") {
			t.Fatalf("schema_search orders.**: want an echo op, got %s", s)
		}
		pre := gw.MustScrape(t)
		res, err = c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
			Name: "query", Arguments: map[string]any{"query": `{ orders { v1 { echo(msg: "via-mcp") { msg version } } } }`},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if s := toolText(res); res.IsError || !strings.Contains(s, "via-mcp") {
			t.Fatalf("query tool: want the echo, got %s", s)
		}
		gw.WaitDeltas(t, pre, dispatchCount, []harness.Want{
			{Labels: harness.Labels{"namespace": "orders", "version": "v1", "code": "ok"}, Delta: 1},
		}, 10*time.Second)
	})

	// A response holds exactly the fields the query selected, for
	// every upstream kind. admin.listServices is the self-ingested
	// OpenAPI surface (huma adds $schema to its responses). Runs last:
	// its calls would move the counts the subtests above check.
	t.Run("selection", func(t *testing.T) {
		for _, c := range []struct{ desc, query, path, want string }{
			{"proto", `{ orders { echo(msg: "x") { msg } } }`, "orders.echo", "msg"},
			{"openapi", `mutation { billing { echo(body: {msg: "x"}) { msg } } }`, "billing.echo", "msg"},
			{"graphql", `{ catalog { echo(msg: "x") { msg } } }`, "catalog.echo", "msg"},
			{"openapi alias and __typename", `mutation { billing { echo(body: {msg: "x"}) { m: msg __typename } } }`, "billing.echo", "__typename,m"},
			{"graphql alias and __typename", `{ catalog { echo(msg: "x") { m: msg __typename } } }`, "catalog.echo", "__typename,m"},
			{"admin object", `{ admin { listServices { environment } } }`, "admin.listServices", "environment"},
			{"admin list of objects", `{ admin { listServices { services { namespace } } } }`, "admin.listServices.services", "namespace"},
		} {
			r := gw.MustGraphQL(t, c.query, nil)
			v := r.Path(c.path)
			objs, isList := v.([]any)
			if !isList {
				objs = []any{v}
			}
			if len(objs) == 0 {
				t.Errorf("%s: no value at %s: %s", c.desc, c.path, r.Data)
			}
			for _, o := range objs {
				obj, _ := o.(map[string]any)
				var keys []string
				for k := range obj {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				if strings.Join(keys, ",") != c.want {
					t.Errorf("%s: selected %s, got fields %v: %s", c.desc, c.want, keys, r.Data)
					break
				}
			}
		}
		// Field collection on the proto append path, compared as raw
		// bytes: a decoded map hides a duplicate key. orders resolves
		// to v2, whose messages are in package fixture.orders.v2.
		for _, c := range []struct {
			desc, query string
			vars        map[string]any
		}{
			{"proto named fragment", `query { orders { echo(msg: "x") { ...F } } } fragment F on Fixture_Orders_V2_EchoResponse { msg }`, nil},
			{"proto @skip", `{ orders { echo(msg: "x") { msg instance @skip(if: true) } } }`, nil},
			{"proto @skip variable", `query($s: Boolean!) { orders { echo(msg: "x") { msg instance @skip(if: $s) } } }`, map[string]any{"s": true}},
			{"proto duplicate field", `{ orders { echo(msg: "x") { msg msg } } }`, nil},
		} {
			r := gw.MustGraphQL(t, c.query, c.vars)
			var got bytes.Buffer
			if err := json.Compact(&got, r.Data); err != nil {
				t.Fatalf("%s: %v: %s", c.desc, err, r.Data)
			}
			if want := `{"orders":{"echo":{"msg":"x"}}}`; got.String() != want {
				t.Errorf("%s: got %s, want %s", c.desc, got.String(), want)
			}
		}
	})
}

func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if b.Len() == 0 {
		return fmt.Sprintf("%+v", res.StructuredContent)
	}
	return b.String()
}
