package gateway

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
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

// installInternalProtoWithBinding registers a single-message,
// single-method internal-proto slot at (ns, ver) whose only message
// declares `(gwag.ps.v1.binding) = { pattern }`. Helper used by the
// cross-slot uniqueness tests below — keeps each test self-contained
// without re-spelling the proto source.
func installInternalProtoWithBinding(t *testing.T, g *Gateway, ns, ver, pattern string) {
	t.Helper()
	optionsBytes, err := os.ReadFile("proto/ps/v1/options.proto")
	if err != nil {
		t.Fatalf("read options.proto: %v", err)
	}
	pkg := strings.ReplaceAll(ns, "_", ".") + "." + ver
	entry := fmt.Sprintf("entry_%s_%s.proto", ns, ver)
	src := []byte(fmt.Sprintf(`syntax = "proto3";
package %s;

import "gw/proto/ps/v1/options.proto";

message Event {
  option (gwag.ps.v1.binding) = { pattern: %q };
  string id = 1;
}

service S {
  rpc Echo(Event) returns (Event);
}
`, pkg, pattern))
	fd, err := compileProtoBytes(entry, src, map[string][]byte{
		"gw/proto/ps/v1/options.proto": optionsBytes,
	})
	if err != nil {
		t.Fatalf("compile %s/%s: %v", ns, ver, err)
	}
	handlers := map[string]InternalProtoHandler{
		"Echo": func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
			return req, nil
		},
	}
	if err := g.addInternalProtoSlotLocked(ns, ver, fd, src, handlers, nil); err != nil {
		t.Fatalf("addInternalProtoSlotLocked(%s/%s): %v", ns, ver, err)
	}
}

// TestRegisterSlot_CrossSlotBindingConflict_Rejected pins the pre-1.0
// rule that two different `(namespace, version)` slots cannot both
// declare the same channel binding pattern. The conflict is hard-
// rejected at slot registration; the prior occupant stays intact.
func TestRegisterSlot_CrossSlotBindingConflict_Rejected(t *testing.T) {
	g := newSlotGateway(t)
	installInternalProtoWithBinding(t, g, "orders", "v1", "events.orders.*.update")

	optionsBytes, err := os.ReadFile("proto/ps/v1/options.proto")
	if err != nil {
		t.Fatalf("read options.proto: %v", err)
	}
	// Different namespace/version, same pattern → must reject.
	conflictSrc := []byte(`syntax = "proto3";
package shipments.v1;

import "gw/proto/ps/v1/options.proto";

message Update {
  option (gwag.ps.v1.binding) = { pattern: "events.orders.*.update" };
  string id = 1;
}

service S { rpc Echo(Update) returns (Update); }
`)
	fd, err := compileProtoBytes("conflict.proto", conflictSrc, map[string][]byte{
		"gw/proto/ps/v1/options.proto": optionsBytes,
	})
	if err != nil {
		t.Fatalf("compile conflict: %v", err)
	}
	handlers := map[string]InternalProtoHandler{
		"Echo": func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
			return req, nil
		},
	}
	err = g.addInternalProtoSlotLocked("shipments", "v1", fd, conflictSrc, handlers, nil)
	if err == nil {
		t.Fatal("expected cross-slot binding conflict reject; got nil")
	}
	if !strings.Contains(err.Error(), "events.orders.*.update") {
		t.Errorf("error %q missing conflicting pattern", err.Error())
	}
	if !strings.Contains(err.Error(), "already claimed by orders/v1") {
		t.Errorf("error %q missing prior-claimant attribution", err.Error())
	}
	if _, ok := g.slots[poolKey{namespace: "shipments", version: "v1"}]; ok {
		t.Error("rejected slot was inserted anyway")
	}
	// Prior slot should still own its binding.
	s := g.slots[poolKey{namespace: "orders", version: "v1"}]
	if s == nil || len(s.channelBindings) != 1 || s.channelBindings[0].Pattern != "events.orders.*.update" {
		t.Errorf("prior slot lost bindings after rejection: %+v", s)
	}
}

// TestRegisterSlot_CrossSlotBinding_NoConflictWhenPatternsDiffer pins
// that two slots with distinct patterns coexist — the check fires
// only on overlap.
func TestRegisterSlot_CrossSlotBinding_NoConflictWhenPatternsDiffer(t *testing.T) {
	g := newSlotGateway(t)
	installInternalProtoWithBinding(t, g, "orders", "v1", "events.orders.>")
	installInternalProtoWithBinding(t, g, "shipments", "v1", "events.shipments.>")
	if g.slots[poolKey{namespace: "shipments", version: "v1"}] == nil {
		t.Fatal("second slot not installed despite distinct pattern")
	}
}

// TestRegisterSlot_CrossSlotBindingReleased_ClearsClaim pins that
// when a slot's last replica drops (`releaseSlotLocked`), its bindings
// release with it, so a fresh slot can claim the freed pattern.
func TestRegisterSlot_CrossSlotBindingReleased_ClearsClaim(t *testing.T) {
	g := newSlotGateway(t)
	installInternalProtoWithBinding(t, g, "orders", "v1", "events.orders.>")
	g.mu.Lock()
	g.releaseSlotLocked(poolKey{namespace: "orders", version: "v1"})
	g.mu.Unlock()
	// With the slot released, a different namespace can adopt the pattern.
	installInternalProtoWithBinding(t, g, "shipments", "v1", "events.orders.>")
	s := g.slots[poolKey{namespace: "shipments", version: "v1"}]
	if s == nil || len(s.channelBindings) != 1 {
		t.Fatalf("shipments/v1 did not install after orders/v1 released: %+v", s)
	}
}

// TestRegisterSlot_CrossSlotBinding_UnstableSelfSwap pins that an
// unstable slot re-registering with the SAME pattern is not a self-
// conflict — the registration is a no-op (same hash) or a swap whose
// own bindings are excluded from the cross-slot check.
func TestRegisterSlot_CrossSlotBinding_UnstableSelfSwap(t *testing.T) {
	g := newSlotGateway(t)
	installInternalProtoWithBinding(t, g, "orders", "unstable", "events.orders.>")
	// Re-register the same slot with the same proto → hash match, idempotent.
	installInternalProtoWithBinding(t, g, "orders", "unstable", "events.orders.>")
	s := g.slots[poolKey{namespace: "orders", version: "unstable"}]
	if s == nil || len(s.channelBindings) != 1 || s.channelBindings[0].Pattern != "events.orders.>" {
		t.Fatalf("self-swap clobbered slot: %+v", s)
	}
}
