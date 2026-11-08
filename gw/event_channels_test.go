package gateway

import (
	"context"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// compileEventProto compiles src (which may import options.proto) as
// one file.
func compileEventProto(t *testing.T, name string, src string) protoreflect.FileDescriptor {
	t.Helper()
	optionsBytes, err := os.ReadFile("proto/ps/v1/options.proto")
	if err != nil {
		t.Fatalf("read options.proto: %v", err)
	}
	fd, err := compileProtoBytes(name, []byte(src), map[string][]byte{
		"gw/proto/ps/v1/options.proto": optionsBytes,
	})
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return fd
}

const ordersEventsProto = `syntax = "proto3";
package orders.v1;

import "gw/proto/ps/v1/options.proto";

message OrderCreated {
  option (gwag.ps.v1.binding) = { name: "OrderEvents", pattern: "events.orders.{orderId}" };
  string order_id = 1;
  int64 total = 2;
}

message OrderShipped {
  option (gwag.ps.v1.binding) = { name: "OrderEvents", pattern: "events.orders.{orderId}" };
  string order_id = 1;
  string carrier = 2;
}

message Audit {
  option (gwag.ps.v1.binding) = { name: "Audit", pattern: "events.audit" };
  string who = 1;
}

service Orders {
  rpc Get(OrderCreated) returns (OrderCreated);
}
`

func TestExtractChannelBindings_NamedAndUnion(t *testing.T) {
	fd := compileEventProto(t, "orders.proto", ordersEventsProto)
	got, err := extractChannelBindings(fd)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d bindings: %+v", len(got), got)
	}
	for _, b := range got[:2] {
		if b.Name != "OrderEvents" || b.Channel != "orders.v1.OrderEvents" || b.Pattern != "events.orders.{orderId}" {
			t.Errorf("binding %+v", b)
		}
	}
	if got[2].Channel != "orders.v1.Audit" {
		t.Errorf("audit channel = %q", got[2].Channel)
	}
}

func TestExtractChannelBindings_Rejects(t *testing.T) {
	cases := map[string]string{
		"same name, different pattern": `
message A { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.x" }; }
message B { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.y" }; }`,
		"same pattern, different names": `
message A { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.x" }; }
message B { option (gwag.ps.v1.binding) = { name: "F", pattern: "a.x" }; }`,
		"named and unnamed on one pattern": `
message A { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.*" }; }
message B { option (gwag.ps.v1.binding) = { pattern: "a.{id}" }; }`,
		"malformed brace": `
message A { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.{id" }; }`,
		"brace inside segment": `
message A { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.x{id}" }; }`,
		"duplicate segment name": `
message A { option (gwag.ps.v1.binding) = { name: "E", pattern: "a.{id}.{id}" }; }`,
		"name without pattern": `
message A { option (gwag.ps.v1.binding) = { name: "E" }; }`,
		"name not an identifier": `
message A { option (gwag.ps.v1.binding) = { name: "E-1", pattern: "a.x" }; }`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			src := "syntax = \"proto3\";\npackage p.v1;\nimport \"gw/proto/ps/v1/options.proto\";\n" + body + "\n"
			fd := compileEventProto(t, "p.proto", src)
			if _, err := extractChannelBindings(fd); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestNormalizeChannelPattern(t *testing.T) {
	norm, names, err := normalizeChannelPattern("events.{tenant}.orders.{orderId}.>")
	if err != nil {
		t.Fatal(err)
	}
	if norm != "events.*.orders.*.>" || strings.Join(names, ",") != "tenant,orderId" {
		t.Errorf("got %q %v", norm, names)
	}
	for _, bad := range []string{"", "a..b", "a.>.b", "a.{}", "a.{1x}", "a.b*"} {
		if _, _, err := normalizeChannelPattern(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	if !subjectMatchesPattern("events.orders.{orderId}", "events.orders.42") {
		t.Error("named segment should match one token")
	}
	if subjectMatchesPattern("events.orders.{orderId}", "events.orders.42.x") {
		t.Error("named segment matched two tokens")
	}
}

// installEventSlot installs src as an internal-proto slot at (ns, ver)
// with a no-op handler for every unary method of its first service.
func installEventSlot(t *testing.T, g *Gateway, ns, ver, file, src string) error {
	t.Helper()
	fd := compileEventProto(t, file, src)
	handlers := map[string]internalProtoHandler{}
	svc := fd.Services().Get(0)
	for i := 0; i < svc.Methods().Len(); i++ {
		handlers[string(svc.Methods().Get(i).Name())] = func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
			return req, nil
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.addInternalProtoSlotLocked(ns, ver, fd, []byte(src), handlers, nil)
}

func TestEventChannels_UnionIndexAndCrossSlot(t *testing.T) {
	g := newSlotGateway(t)
	if err := installEventSlot(t, g, "orders", "v1", "orders.proto", ordersEventsProto); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.rebuildChannelBindingIndexLocked()
	g.mu.Unlock()
	idx := g.channelBindingIndex.Load()
	c := idx.lookupChannel("events.orders.42")
	if c == nil || c.key != "orders.v1.OrderEvents" || !c.union() || len(c.descs) != 2 {
		t.Fatalf("lookupChannel = %+v", c)
	}
	if c.streamName() != "EV_orders_v1_OrderEvents" {
		t.Errorf("stream name %q", c.streamName())
	}

	// Another slot claiming the same pattern (spelled with a different
	// segment name) is rejected.
	other := `syntax = "proto3";
package billing.v1;
import "gw/proto/ps/v1/options.proto";
message Charged {
  option (gwag.ps.v1.binding) = { name: "OrderEvents", pattern: "events.orders.{id}" };
  string id = 1;
}
service Billing { rpc Get(Charged) returns (Charged); }
`
	err := installEventSlot(t, g, "billing", "v1", "billing.proto", other)
	if err == nil || !strings.Contains(err.Error(), "cross-slot pattern conflict") {
		t.Fatalf("want cross-slot conflict, got %v", err)
	}
}
