package gateway

import (
	"context"
	"os"
	"sort"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/gwag/gw/ir"
)

// TestExtractChannelBindings_FromProtoOption pins that
// `(gwag.ps.v1.binding)` on a message round-trips through
// protocompile and surfaces on the extracted ChannelBindings list as
// (pattern, message FQN). Top-level and nested messages both count;
// messages without the option are ignored.
func TestExtractChannelBindings_FromProtoOption(t *testing.T) {
	optionsBytes, err := os.ReadFile("proto/ps/v1/options.proto")
	if err != nil {
		t.Fatalf("read options.proto: %v", err)
	}
	src := []byte(`syntax = "proto3";

package example.events.v1;

import "gw/proto/ps/v1/options.proto";

message OrderUpdate {
  option (gwag.ps.v1.binding) = { pattern: "events.orders.*.update" };
  string order_id = 1;
}

message ShipmentCreated {
  option (gwag.ps.v1.binding) = { pattern: "events.shipments.>" };
  string shipment_id = 1;

  // Nested-with-binding to confirm walkMessagesForBindings recurses.
  message LegEvent {
    option (gwag.ps.v1.binding) = { pattern: "events.shipments.legs.*.update" };
    string leg_id = 1;
  }
}

message NoBinding {
  string thing = 1;
}
`)
	fd, err := compileProtoBytes("entry.proto", src, map[string][]byte{
		"gw/proto/ps/v1/options.proto": optionsBytes,
	})
	if err != nil {
		t.Fatalf("compileProtoBytes: %v", err)
	}

	got := extractChannelBindings(fd)
	sort.Slice(got, func(i, j int) bool { return got[i].Pattern < got[j].Pattern })
	want := []ir.ChannelBinding{
		{Pattern: "events.orders.*.update", MessageFQN: "example.events.v1.OrderUpdate"},
		{Pattern: "events.shipments.>", MessageFQN: "example.events.v1.ShipmentCreated"},
		{Pattern: "events.shipments.legs.*.update", MessageFQN: "example.events.v1.ShipmentCreated.LegEvent"},
	}
	sort.Slice(want, func(i, j int) bool { return want[i].Pattern < want[j].Pattern })
	if len(got) != len(want) {
		t.Fatalf("got %d bindings, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("binding[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestExtractChannelBindings_NoOptionsFileImport pins the no-op case:
// a proto file that never imports options.proto yields no bindings,
// even when other messages in the same compile have bindings. Empty
// vs nil is irrelevant — callers iterate without checking.
func TestExtractChannelBindings_NoOptionsFileImport(t *testing.T) {
	src := []byte(`syntax = "proto3";
package example.plain.v1;

message Thing { string id = 1; }
`)
	fd, err := compileProtoBytes("entry.proto", src, nil)
	if err != nil {
		t.Fatalf("compileProtoBytes: %v", err)
	}
	if got := extractChannelBindings(fd); len(got) != 0 {
		t.Errorf("expected no bindings, got %#v", got)
	}
}

// TestBakeSlotIR_StampsChannelBindings pins the end-to-end:
// a proto file registered via the internal-proto path lands with its
// ChannelBindings populated on every Service in s.ir. This is the
// path schema-rebuild aggregation (todo 8) will walk.
func TestBakeSlotIR_StampsChannelBindings(t *testing.T) {
	optionsBytes, err := os.ReadFile("proto/ps/v1/options.proto")
	if err != nil {
		t.Fatalf("read options.proto: %v", err)
	}
	src := []byte(`syntax = "proto3";
package example.bake.v1;

import "gw/proto/ps/v1/options.proto";

message Tick {
  option (gwag.ps.v1.binding) = { pattern: "ticks.>" };
  string id = 1;
}

service Heartbeat {
  rpc Beat(Tick) returns (Tick);
}
`)
	fd, err := compileProtoBytes("entry.proto", src, map[string][]byte{
		"gw/proto/ps/v1/options.proto": optionsBytes,
	})
	if err != nil {
		t.Fatalf("compileProtoBytes: %v", err)
	}

	g := &Gateway{}
	handlers := map[string]InternalProtoHandler{
		"Beat": func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
			return req, nil
		},
	}
	if err := g.addInternalProtoSlotLocked("example_bake", "v1", fd, src, handlers, nil); err != nil {
		t.Fatalf("addInternalProtoSlotLocked: %v", err)
	}
	s := g.slots[poolKey{namespace: "example_bake", version: "v1"}]
	if s == nil {
		t.Fatal("slot not created")
	}
	if len(s.ir) == 0 {
		t.Fatal("slot.ir empty after bake")
	}
	for i, svc := range s.ir {
		if len(svc.ChannelBindings) != 1 {
			t.Fatalf("svc[%d].ChannelBindings = %#v, want 1 entry", i, svc.ChannelBindings)
		}
		got := svc.ChannelBindings[0]
		want := ir.ChannelBinding{Pattern: "ticks.>", MessageFQN: "example.bake.v1.Tick"}
		if got != want {
			t.Errorf("svc[%d].ChannelBindings[0] = %+v, want %+v", i, got, want)
		}
	}
}
