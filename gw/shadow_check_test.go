package gateway

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

const corpOrderProto = `syntax = "proto3";
package corp.orders.v1;
message Order {
  string order_id = 1;
  int64 cents = 2;
  string note = 3;
}
`

// shadowProto is gwag's copy; cents is the field under test.
func shadowProto(centsType string) string {
	return `syntax = "proto3";
package shop.v1;
import "gw/proto/ps/v1/options.proto";
message CorpOrder {
  option (gwag.ps.v1.shadow) = {
    registry: "corp"
    subject: "corp-orders-value"
    message: "corp.orders.v1.Order"
  };
  string order_id = 1;
  ` + centsType + ` cents = 2;
}
`
}

func shadowImports(t *testing.T) map[string][]byte {
	t.Helper()
	opts, err := os.ReadFile("proto/ps/v1/options.proto")
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{"gw/proto/ps/v1/options.proto": opts}
}

func shadowGateway(t *testing.T, withRegistry bool, opts ...Option) *Gateway {
	t.Helper()
	opts = append([]Option{WithoutBackpressure()}, opts...)
	if withRegistry {
		opts = append(opts, WithSchemaRegistry(SchemaRegistryOptions{}))
	}
	g := New(opts...)
	t.Cleanup(g.Close)
	return g
}

// importCorp puts corpOrderProto at subject in context, as `gwag ctl
// schema import --into` would.
func importCorp(t *testing.T, g *Gateway, context_, subject string) {
	t.Helper()
	store := g.sr.store.Load()
	if store == nil {
		t.Fatal("no store")
	}
	_, err := store.Context(context_).Register(context.Background(), subject, sr.Schema{SchemaType: "PROTOBUF", Schema: corpOrderProto})
	if err != nil {
		t.Fatal(err)
	}
}

func shadowReq(t *testing.T, ns, src string) *cpv1.RegisterRequest {
	return &cpv1.RegisterRequest{
		Addr:       "127.0.0.1:1",
		InstanceId: ns + "@1",
		Services: []*cpv1.ServiceBinding{{
			Namespace:    ns,
			Version:      "unstable",
			ProtoSource:  []byte(src),
			ProtoImports: shadowImports(t),
		}},
	}
}

func checkOne(t *testing.T, g *Gateway, ns, src string) []string {
	t.Helper()
	resp, err := g.ControlPlane().Register(context.Background(), shadowReq(t, ns, src))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return resp.GetWarnings()
}

func TestShadowCheck_Match(t *testing.T) {
	g := shadowGateway(t, true)
	importCorp(t, g, ".corp", "corp-orders-value")
	if w := checkOne(t, g, "shop", shadowProto("int64")); len(w) != 0 {
		t.Fatalf("warnings on a match: %v", w)
	}
}

func TestShadowCheck_Mismatch(t *testing.T) {
	g := shadowGateway(t, true)
	importCorp(t, g, ".corp", "corp-orders-value")
	w := checkOne(t, g, "shop", shadowProto("string"))
	if len(w) != 1 {
		t.Fatalf("want 1 warning, got %v", w)
	}
	for _, want := range []string{"shop/unstable", "shop.v1.CorpOrder", "corp-orders-value", ".corp", "cents", "int64"} {
		if !strings.Contains(w[0], want) {
			t.Errorf("warning lacks %q: %s", want, w[0])
		}
	}
	// ListServices carries it too, and the registration stands.
	resp, err := g.ControlPlane().ListServices(context.Background(), &cpv1.ListServicesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range resp.GetServices() {
		if s.GetNamespace() == "shop" {
			got = s.GetWarnings()
		}
	}
	if len(got) != 1 || got[0] != w[0] {
		t.Errorf("ListServices warnings %v, want %v", got, w)
	}
	// A fixed shadow clears them.
	if w := checkOne(t, g, "shop", shadowProto("int64")); len(w) != 0 {
		t.Fatalf("warnings after the fix: %v", w)
	}
	resp, _ = g.ControlPlane().ListServices(context.Background(), &cpv1.ListServicesRequest{})
	for _, s := range resp.GetServices() {
		if s.GetNamespace() == "shop" && len(s.GetWarnings()) != 0 {
			t.Errorf("stale warnings: %v", s.GetWarnings())
		}
	}
}

func TestShadowCheck_NotFound(t *testing.T) {
	g := shadowGateway(t, true)
	// Present only in the default context: not a corp schema.
	importCorp(t, g, ".", "corp-orders-value")
	if w := checkOne(t, g, "shop", shadowProto("string")); len(w) != 0 {
		t.Fatalf("warnings with no subject: %v", w)
	}
}

// The registry name picks the context first; any other non-default
// context holding the subject is the fallback.
func TestShadowCheck_ContextLookup(t *testing.T) {
	g := shadowGateway(t, true)
	importCorp(t, g, ".elsewhere", "corp-orders-value")
	if w := checkOne(t, g, "shop", shadowProto("string")); len(w) != 1 || !strings.Contains(w[0], ".elsewhere") {
		t.Fatalf("fallback context: %v", w)
	}
}

func TestShadowCheck_RegistryOff(t *testing.T) {
	g := shadowGateway(t, false)
	if w := checkOne(t, g, "shop", shadowProto("string")); len(w) != 0 {
		t.Fatalf("warnings with the registry off: %v", w)
	}
}

func TestShadowCheck_Metric(t *testing.T) {
	g := shadowGateway(t, true)
	importCorp(t, g, ".corp", "corp-orders-value")
	checkOne(t, g, "shop", shadowProto("int64"))
	checkOne(t, g, "shop2", shadowProto("string"))
	rec := httptest.NewRecorder()
	g.MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{`shadow_check_total{outcome="match"} 1`, `shadow_check_total{outcome="mismatch"} 1`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}
