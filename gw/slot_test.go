package gateway

import (
	"strings"
	"testing"
)

// registerSlotLocked is the §4 tier policy site. Its job is to decide,
// for any incoming registration, whether the gateway is looking at a
// fresh slot, an idempotent re-add, an unstable swap, or a vN
// rejection — without the per-kind code each having to redo the
// same comparison. These tests pin the contract.

func newSlotGateway(t *testing.T) *Gateway {
	t.Helper()
	g := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(g.Close)
	return g
}

func key(ns, ver string) poolKey {
	return poolKey{namespace: ns, version: ver}
}

func TestRegisterSlot_FreshInsert(t *testing.T) {
	g := newSlotGateway(t)
	hash := [32]byte{1}
	g.mu.Lock()
	defer g.mu.Unlock()
	existed, err := g.registerSlotLocked(slotKindProto, key("greeter", "v1"), hash, 64, 16)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if existed {
		t.Fatalf("existed=true for fresh slot")
	}
	s := g.slots[key("greeter", "v1")]
	if s == nil {
		t.Fatalf("slot not stored")
	}
	if s.kind != slotKindProto || s.hash != hash || s.maxConcurrency != 64 || s.maxConcurrencyPerInstance != 16 {
		t.Errorf("slot fields wrong: %+v", s)
	}
}

func TestRegisterSlot_IdempotentSameKindHashCaps(t *testing.T) {
	g := newSlotGateway(t)
	hash := [32]byte{2}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.registerSlotLocked(slotKindOpenAPI, key("billing", "v3"), hash, 100, 10); err != nil {
		t.Fatalf("first register: %v", err)
	}
	existed, err := g.registerSlotLocked(slotKindOpenAPI, key("billing", "v3"), hash, 100, 10)
	if err != nil {
		t.Fatalf("second register: %v", err)
	}
	if !existed {
		t.Fatalf("existed=false for matching re-register; same kind+hash+caps should be add-replica path")
	}
}

func TestRegisterSlot_VN_DiffHashRejected(t *testing.T) {
	g := newSlotGateway(t)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.registerSlotLocked(slotKindProto, key("greeter", "v2"), [32]byte{3}, 0, 0); err != nil {
		t.Fatalf("first register: %v", err)
	}
	_, err := g.registerSlotLocked(slotKindProto, key("greeter", "v2"), [32]byte{4}, 0, 0)
	if err == nil {
		t.Fatalf("expected reject for vN diff-hash; got nil")
	}
	if !strings.Contains(err.Error(), "different schema hash") {
		t.Errorf("error %q missing 'different schema hash'", err.Error())
	}
	if !strings.Contains(err.Error(), "vN is locked") {
		t.Errorf("error %q missing 'vN is locked' guidance", err.Error())
	}
}

func TestRegisterSlot_VN_DiffCapsRejected(t *testing.T) {
	g := newSlotGateway(t)
	hash := [32]byte{5}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.registerSlotLocked(slotKindProto, key("greeter", "v1"), hash, 64, 16); err != nil {
		t.Fatalf("first register: %v", err)
	}
	_, err := g.registerSlotLocked(slotKindProto, key("greeter", "v1"), hash, 128, 16)
	if err == nil {
		t.Fatalf("expected reject for vN diff-caps; got nil")
	}
	if !strings.Contains(err.Error(), "different concurrency caps") {
		t.Errorf("error %q missing 'different concurrency caps'", err.Error())
	}
}

func TestRegisterSlot_VN_CrossKindRejected(t *testing.T) {
	g := newSlotGateway(t)
	hash := [32]byte{6}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.registerSlotLocked(slotKindProto, key("greeter", "v1"), hash, 0, 0); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Attempting to re-occupy the slot as openapi at vN must reject —
	// even with identical hash bytes (they wouldn't be the same
	// schema, but the slot helper trusts the kind tag as the
	// distinguishing axis).
	_, err := g.registerSlotLocked(slotKindOpenAPI, key("greeter", "v1"), hash, 0, 0)
	if err == nil {
		t.Fatalf("expected reject for vN cross-kind; got nil")
	}
	if !strings.Contains(err.Error(), "already registered as proto") {
		t.Errorf("error %q missing kind-mismatch info", err.Error())
	}
}

func TestRegisterSlot_Unstable_SwapOnDiffHash(t *testing.T) {
	g := newSlotGateway(t)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.registerSlotLocked(slotKindProto, key("greeter", "unstable"), [32]byte{7}, 0, 0); err != nil {
		t.Fatalf("first register: %v", err)
	}
	existed, err := g.registerSlotLocked(slotKindProto, key("greeter", "unstable"), [32]byte{8}, 0, 0)
	if err != nil {
		t.Fatalf("unstable swap: %v", err)
	}
	if existed {
		t.Fatalf("existed=true for unstable swap; caller would skip the create-fresh path and reuse a stale per-kind struct")
	}
	got := g.slots[key("greeter", "unstable")]
	if got == nil || got.hash != ([32]byte{8}) {
		t.Errorf("slot did not adopt new hash: %+v", got)
	}
}

func TestRegisterSlot_Unstable_SwapAcrossKinds(t *testing.T) {
	g := newSlotGateway(t)
	g.mu.Lock()
	defer g.mu.Unlock()
	// Pretend the prior occupant left per-kind state on the slot so
	// we can verify evictSlotLocked clears it on the swap.
	if _, err := g.registerSlotLocked(slotKindOpenAPI, key("greeter", "unstable"), [32]byte{9}, 0, 0); err != nil {
		t.Fatalf("first register: %v", err)
	}
	g.slots[key("greeter", "unstable")].openapi = &openAPISource{}
	existed, err := g.registerSlotLocked(slotKindProto, key("greeter", "unstable"), [32]byte{10}, 0, 0)
	if err != nil {
		t.Fatalf("cross-kind unstable swap: %v", err)
	}
	if existed {
		t.Fatalf("existed=true for cross-kind unstable swap")
	}
	got := g.slots[key("greeter", "unstable")]
	if got == nil || got.kind != slotKindProto {
		t.Errorf("slot did not adopt new kind: %+v", got)
	}
	if got.openapi != nil {
		t.Errorf("evictSlotLocked left slot.openapi populated after kind change")
	}
}

// --allow-tier policy (plan §4): registerSlotLocked is the single
// site every register flows through, so the tier gate lives there.
// The default (WithAllowTier never called) accepts every tier; an
// explicit allow-list rejects everything outside it with a clear
// "tier %q is not in --allow-tier policy" message.
func TestRegisterSlot_AllowTier_DefaultAcceptsAll(t *testing.T) {
	g := newSlotGateway(t)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.registerSlotLocked(slotKindProto, key("a", "unstable"), [32]byte{20}, 0, 0); err != nil {
		t.Errorf("default policy rejected unstable: %v", err)
	}
	if _, err := g.registerSlotLocked(slotKindProto, key("b", "v1"), [32]byte{21}, 0, 0); err != nil {
		t.Errorf("default policy rejected vN: %v", err)
	}
}

func TestRegisterSlot_AllowTier_VNOnly_RejectsUnstable(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")), WithAllowTier("vN"))
	t.Cleanup(g.Close)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.registerSlotLocked(slotKindProto, key("a", "v1"), [32]byte{22}, 0, 0); err != nil {
		t.Errorf("vN allowed but rejected: %v", err)
	}
	_, err := g.registerSlotLocked(slotKindProto, key("a", "unstable"), [32]byte{23}, 0, 0)
	if err == nil {
		t.Fatalf("expected reject for unstable when --allow-tier=vN; got nil")
	}
	if !strings.Contains(err.Error(), "not in --allow-tier policy") {
		t.Errorf("error %q missing policy phrasing", err.Error())
	}
	if !strings.Contains(err.Error(), "\"unstable\"") {
		t.Errorf("error %q should name the rejected tier", err.Error())
	}
}

func TestRegisterSlot_AllowTier_UnstableOnly_RejectsVN(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")), WithAllowTier("unstable"))
	t.Cleanup(g.Close)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.registerSlotLocked(slotKindProto, key("a", "unstable"), [32]byte{24}, 0, 0); err != nil {
		t.Errorf("unstable allowed but rejected: %v", err)
	}
	_, err := g.registerSlotLocked(slotKindProto, key("a", "v1"), [32]byte{25}, 0, 0)
	if err == nil {
		t.Fatalf("expected reject for vN when --allow-tier=unstable; got nil")
	}
	if !strings.Contains(err.Error(), "\"vN\"") {
		t.Errorf("error %q should name the rejected tier", err.Error())
	}
}

// "stable" in the allow set never gates registration — it's a
// computed alias, not a registerable version. Passing only "stable"
// rejects every actual register.
func TestRegisterSlot_AllowTier_StableOnly_RejectsRegistrations(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")), WithAllowTier("stable"))
	t.Cleanup(g.Close)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.registerSlotLocked(slotKindProto, key("a", "v1"), [32]byte{26}, 0, 0); err == nil {
		t.Errorf("expected reject for vN when --allow-tier=stable")
	}
	if _, err := g.registerSlotLocked(slotKindProto, key("a", "unstable"), [32]byte{27}, 0, 0); err == nil {
		t.Errorf("expected reject for unstable when --allow-tier=stable")
	}
}

func TestReleaseSlotLocked_DropsIndex(t *testing.T) {
	g := newSlotGateway(t)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.registerSlotLocked(slotKindProto, key("greeter", "v1"), [32]byte{11}, 0, 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	g.releaseSlotLocked(key("greeter", "v1"))
	if _, ok := g.slots[key("greeter", "v1")]; ok {
		t.Errorf("slot still present after release")
	}
}
