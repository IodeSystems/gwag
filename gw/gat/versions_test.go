package gat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/sse"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// shop is a service with two versions in one process: v2 has the
// handler, v1 adapts its requests and responses onto v2's. Both name
// their result type Order.
type shop struct {
	v1Calls, v2Calls atomic.Int32
}

type orderIn struct {
	ID string `path:"id"`
}

type orderEventsIn struct {
	ID string `query:"id"`
}

// v2Order is the v2 handler and the v2 wire type.
func (s *shop) registerV2(api huma.API, g *gat.Gateway) {
	type Order struct {
		ID     string `json:"id"`
		Total  int32  `json:"total"`
		Status string `json:"status"`
	}
	type out struct{ Body Order }
	gat.Register(api, g, huma.Operation{OperationID: "getOrder", Method: http.MethodGet, Path: "/v2/orders/{id}"},
		func(_ context.Context, in *orderIn) (*out, error) {
			s.v2Calls.Add(1)
			return &out{Body: Order{ID: in.ID, Total: 1250, Status: "paid"}}, nil
		})
	gat.RegisterSSE(api, g, huma.Operation{OperationID: "orderEvents", Method: http.MethodGet, Path: "/v2/order-events"},
		map[string]any{"order": Order{}}, func(context.Context, *orderEventsIn, sse.Sender) {})
}

// registerV1 maps v1 onto v2: v1's Order has amount as a string.
func (s *shop) registerV1(api huma.API, g *gat.Gateway) {
	type Order struct {
		ID     string `json:"id"`
		Amount string `json:"amount"`
	}
	type out struct{ Body Order }
	gat.Register(api, g, huma.Operation{OperationID: "getOrder", Method: http.MethodGet, Path: "/v1/orders/{id}"},
		func(_ context.Context, in *orderIn) (*out, error) {
			s.v1Calls.Add(1)
			return &out{Body: Order{ID: in.ID, Amount: "12.50"}}, nil
		})
	gat.RegisterSSE(api, g, huma.Operation{OperationID: "orderEvents", Method: http.MethodGet, Path: "/v1/order-events"},
		map[string]any{"order": Order{}}, func(context.Context, *orderEventsIn, sse.Sender) {})
}

func versionAPI(mux *http.ServeMux, version string) huma.API {
	cfg := huma.DefaultConfig("Shop", version+".0.0")
	cfg.OpenAPIPath = "/" + version + "/openapi"
	cfg.DocsPath = ""
	cfg.SchemasPath = "/" + version + "/schemas"
	return humago.New(mux, cfg)
}

type shopGat struct {
	*shop
	g        *gat.Gateway
	api1     huma.API
	srv      *httptest.Server
	graphql  string
	schemaOf func(query string) []byte
}

func newVersionedShop(t *testing.T, configure func(*gat.Gateway), v2opts ...gat.VersionOption) *shopGat {
	t.Helper()
	mux := http.NewServeMux()
	api1, api2 := versionAPI(mux, "v1"), versionAPI(mux, "v2")
	g := mustNewGat(t)
	if configure != nil {
		configure(g)
	}
	s := &shop{}
	s.registerV1(api1, g)
	s.registerV2(api2, g)
	if err := gat.RegisterHumaVersion(api1, g, "v1"); err != nil {
		t.Fatalf("RegisterHumaVersion v1: %v", err)
	}
	if err := gat.RegisterHumaVersion(api2, g, "v2", v2opts...); err != nil {
		t.Fatalf("RegisterHumaVersion v2: %v", err)
	}
	if err := gat.RegisterHuma(api2, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	if err := gat.RegisterGRPC(mux, g, "/api/grpc"); err != nil {
		t.Fatalf("RegisterGRPC: %v", err)
	}
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.EnableHTTP2 = true
	srv.Start()
	t.Cleanup(srv.Close)
	return &shopGat{shop: s, g: g, api1: api1, srv: srv, graphql: srv.URL + "/api/graphql",
		schemaOf: func(q string) []byte { return mustGET(t, srv.URL+"/api/schema/"+q) }}
}

func TestVersions_GraphQL(t *testing.T) {
	sg := newVersionedShop(t, nil)
	got := graphqlRaw(t, sg.graphql,
		`{ Shop { v1 { getOrder(id: "a") { id amount } } v2 { getOrder(id: "b") { id total status } } getOrder(id: "c") { id total } } }`, nil)
	const want = `{"data":{"Shop":{"v1":{"getOrder":{"id":"a","amount":"12.50"}},"v2":{"getOrder":{"id":"b","total":1250,"status":"paid"}},"getOrder":{"id":"c","total":1250}}}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if v1, v2 := sg.v1Calls.Load(), sg.v2Calls.Load(); v1 != 1 || v2 != 2 {
		t.Errorf("handler calls: v1=%d v2=%d, want 1 and 2", v1, v2)
	}

	sdl := string(sg.schemaOf("graphql"))
	for _, want := range []string{
		`v1: ShopV1QueryNamespace! @deprecated(reason: "v2 is current")`,
		"  v2: ShopV2QueryNamespace!\n",
		"type Shop_Order {",    // v2, the latest
		"type Shop_v1_Order {", // v1
		"amount: String!",
		"getOrder(id: String!): Shop_v1_Order",
		"Shop_orderEvents(id: String): Shop_Order!",
		`Shop_v1_orderEvents(id: String): Shop_v1_Order! @deprecated(reason: "v2 is current")`,
	} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL lacks %q:\n%s", want, sdl)
		}
	}

	// REST is huma's, one API per version.
	if b := string(mustGET(t, sg.srv.URL+"/v1/orders/x")); !strings.Contains(b, `"amount":"12.50"`) {
		t.Errorf("REST v1: %s", b)
	}
	if b := string(mustGET(t, sg.srv.URL+"/v2/orders/x")); !strings.Contains(b, `"total":1250`) {
		t.Errorf("REST v2: %s", b)
	}
}

func TestVersions_GRPC(t *testing.T) {
	sg := newVersionedShop(t, nil)
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(sg.schemaOf("proto"), fds); err != nil {
		t.Fatal(err)
	}
	deprecated := map[string]bool{}
	for _, f := range fds.File {
		deprecated[f.GetPackage()] = f.GetOptions().GetDeprecated()
		for _, s := range f.Service {
			for _, m := range s.Method {
				if m.GetOptions().GetDeprecated() != f.GetOptions().GetDeprecated() {
					t.Errorf("%s %s: method deprecated=%v, file %v", f.GetPackage(), m.GetName(), m.GetOptions().GetDeprecated(), f.GetOptions().GetDeprecated())
				}
			}
		}
	}
	if len(deprecated) != 2 || !deprecated["Shop.v1"] || deprecated["Shop.v2"] {
		t.Errorf("packages and deprecation: %v, want Shop.v1 deprecated, Shop.v2 not", deprecated)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	call := func(pkg, field string) string {
		d, err := files.FindDescriptorByName(protoreflect.FullName(pkg + ".Service.getOrder"))
		if err != nil {
			t.Fatalf("%s: %v", pkg, err)
		}
		md := d.(protoreflect.MethodDescriptor)
		client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](sg.srv.Client(),
			sg.srv.URL+"/api/grpc/"+pkg+".Service/getOrder",
			connect.WithSchema(md),
			connect.WithResponseInitializer(func(_ connect.Spec, msg any) error {
				*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(md.Output())
				return nil
			}))
		req := dynamicpb.NewMessage(md.Input())
		req.Set(md.Input().Fields().ByName("id"), protoreflect.ValueOfString("g"))
		resp, err := client.CallUnary(context.Background(), connect.NewRequest(req))
		if err != nil {
			t.Fatalf("%s: %v", pkg, err)
		}
		f := resp.Msg.Descriptor().Fields().ByName(protoreflect.Name(field))
		if f == nil {
			t.Fatalf("%s: response has no %s", pkg, field)
		}
		return resp.Msg.Get(f).String()
	}
	if got := call("Shop.v1", "amount"); got != "12.50" {
		t.Errorf("v1 amount = %q", got)
	}
	if got := call("Shop.v2", "total"); got != "1250" {
		t.Errorf("v2 total = %q", got)
	}
	if v1, v2 := sg.v1Calls.Load(), sg.v2Calls.Load(); v1 != 1 || v2 != 1 {
		t.Errorf("handler calls: v1=%d v2=%d", v1, v2)
	}

	// ?version= narrows the set.
	one := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(sg.schemaOf("proto?version=v1"), one); err != nil {
		t.Fatal(err)
	}
	if len(one.File) != 1 || one.File[0].GetPackage() != "Shop.v1" || !one.File[0].GetOptions().GetDeprecated() {
		t.Errorf("?version=v1: %d files, first %s", len(one.File), one.File[0].GetPackage())
	}
}

// operationsDeprecated reports each operation's deprecated flag by
// "METHOD path".
func operationsDeprecated(t *testing.T, raw []byte) map[string]bool {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]struct {
			Deprecated bool `json:"deprecated"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	out := map[string]bool{}
	for p, item := range doc.Paths {
		for m, op := range item {
			out[strings.ToUpper(m)+" "+p] = op.Deprecated
		}
	}
	return out
}

func TestVersions_OpenAPI(t *testing.T) {
	sg := newVersionedShop(t, nil)
	v1 := operationsDeprecated(t, sg.schemaOf("openapi?version=v1"))
	if len(v1) != 2 || !v1["GET /v1/orders/{id}"] || !v1["GET /v1/order-events"] {
		t.Errorf("v1 ops: %v, want both deprecated", v1)
	}
	v2 := operationsDeprecated(t, sg.schemaOf("openapi?version=v2"))
	if len(v2) != 2 || v2["GET /v2/orders/{id}"] || v2["GET /v2/order-events"] {
		t.Errorf("v2 ops: %v, want neither deprecated", v2)
	}
	if a, b := sg.schemaOf("openapi?service=Shop:v2"), sg.schemaOf("openapi?version=v2"); string(a) != string(b) {
		t.Errorf("?service=Shop:v2 differs from ?version=v2")
	}
	// ?openapi= still applies to a selected version.
	if b := string(sg.schemaOf("openapi?version=v1&openapi=3.0")); !strings.Contains(b, `"openapi":"3.0.`) || !strings.Contains(b, `"deprecated":true`) {
		t.Errorf("v1 as 3.0: %.200s", b)
	}

	// Unselected: namespace, then version.
	var bundle map[string]map[string]json.RawMessage
	if err := json.Unmarshal(sg.schemaOf("openapi"), &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle) != 1 || len(bundle["Shop"]) != 2 || bundle["Shop"]["v1"] == nil || bundle["Shop"]["v2"] == nil {
		t.Errorf("bundle keys: %v", bundle)
	}

	resp, err := http.Get(sg.srv.URL + "/api/schema/openapi?version=v9")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("?version=v9: %d, want 404", resp.StatusCode)
	}

	// OpenAPISpec is the spec huma built, without deprecation marks.
	spec, err := sg.g.OpenAPISpec("v1")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(sg.api1.OpenAPI())
	if string(spec) != string(want) {
		t.Errorf("OpenAPISpec(v1) is not api1's document")
	}
	if strings.Contains(string(spec), `"deprecated"`) {
		t.Errorf("OpenAPISpec(v1) carries deprecated")
	}
	if _, err := sg.g.OpenAPISpec("v3"); err == nil {
		t.Errorf("OpenAPISpec(v3): want error")
	}
}

func TestVersions_VersionedOnly(t *testing.T) {
	sg := newVersionedShop(t, func(g *gat.Gateway) { g.VersionedOnly(true) })
	got := graphqlRaw(t, sg.graphql, `{ Shop { v1 { getOrder(id: "a") { amount } } v2 { getOrder(id: "b") { total } } } }`, nil)
	if want := `{"data":{"Shop":{"v1":{"getOrder":{"amount":"12.50"}},"v2":{"getOrder":{"total":1250}}}}}`; got != want {
		t.Errorf("got %s", got)
	}
	if got := graphqlRaw(t, sg.graphql, `{ Shop { getOrder(id: "c") { total } } }`, nil); !strings.Contains(got, `"errors"`) {
		t.Errorf("flat copy still served: %s", got)
	}
	sdl := string(sg.schemaOf("graphql"))
	if !strings.Contains(sdl, "Shop_v2_orderEvents(") || strings.Contains(sdl, "Shop_orderEvents(") {
		t.Errorf("VersionedOnly subscriptions:\n%s", sdl)
	}
}

func TestVersions_ManualDeprecation(t *testing.T) {
	sg := newVersionedShop(t, nil, gat.VersionDeprecated("ends 2027-06; no v3 planned"))
	sdl := string(sg.schemaOf("graphql"))
	for _, want := range []string{
		`v2: ShopV2QueryNamespace! @deprecated(reason: "ends 2027-06; no v3 planned")`,
		`getOrder(id: String!): Shop_Order @deprecated(reason: "ends 2027-06; no v3 planned")`,
	} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL lacks %q:\n%s", want, sdl)
		}
	}
	if v2 := operationsDeprecated(t, sg.schemaOf("openapi?version=v2")); !v2["GET /v2/orders/{id}"] {
		t.Errorf("v2 ops not deprecated: %v", v2)
	}
}

func TestVersions_Errors(t *testing.T) {
	noop := func(context.Context, *orderIn) (*getProjectOutput, error) { return &getProjectOutput{}, nil }
	reg := func(api huma.API, g *gat.Gateway, path string) {
		gat.Register(api, g, huma.Operation{OperationID: "getOrder", Method: http.MethodGet, Path: path}, noop)
	}
	setup := func() (*gat.Gateway, huma.API, huma.API) {
		mux := http.NewServeMux()
		api1, api2 := versionAPI(mux, "v1"), versionAPI(mux, "v2")
		g := mustNewGat(t)
		reg(api1, g, "/v1/orders/{id}")
		reg(api2, g, "/v2/orders/{id}")
		return g, api1, api2
	}

	g, api1, api2 := setup()
	if err := gat.RegisterHumaVersion(api1, g, "1"); err == nil {
		t.Errorf("version 1: want error")
	}
	if err := gat.RegisterHumaVersion(api1, g, "v1"); err != nil {
		t.Fatal(err)
	}
	if err := gat.RegisterHumaVersion(api2, g, "v1"); err == nil {
		t.Errorf("v1 twice: want error")
	}
	if err := gat.RegisterHumaVersion(api1, g, "v2"); err == nil {
		t.Errorf("same API twice: want error")
	}
	if err := gat.RegisterHumaVersion(api2, g, "v2", gat.VersionNamespace("Other")); err == nil {
		t.Errorf("namespace mismatch: want error")
	}

	// An op on an API no version covers fails the finalize.
	g, api1, _ = setup()
	if err := gat.RegisterHumaVersion(api1, g, "v1"); err != nil {
		t.Fatal(err)
	}
	if err := gat.RegisterHuma(api1, g, "/api"); err == nil || !strings.Contains(err.Error(), "no RegisterHumaVersion call covers") {
		t.Errorf("uncovered op: %v", err)
	}

	// RegisterHTTP finalizes too.
	g, api1, api2 = setup()
	for i, api := range []huma.API{api1, api2} {
		if err := gat.RegisterHumaVersion(api, g, []string{"v1", "v2"}[i]); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	if err := gat.RegisterHTTP(mux, g, "/api"); err != nil {
		t.Fatalf("RegisterHTTP: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if got := graphqlRaw(t, srv.URL+"/api/graphql", `{ Shop { v1 { getOrder(id: "a") { id } } } }`, nil); !strings.Contains(got, `"data"`) || strings.Contains(got, `"errors"`) {
		t.Errorf("RegisterHTTP: %s", got)
	}
}
