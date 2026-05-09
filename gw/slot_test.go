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
	// Pretend the prior occupant left some per-kind state behind so we
	// can verify evictSlotLocked clears it on the swap.
	if g.openAPISources == nil {
		g.openAPISources = map[poolKey]*openAPISource{}
	}
	g.openAPISources[key("greeter", "unstable")] = &openAPISource{}
	if _, err := g.registerSlotLocked(slotKindOpenAPI, key("greeter", "unstable"), [32]byte{9}, 0, 0); err != nil {
		t.Fatalf("first register: %v", err)
	}
	existed, err := g.registerSlotLocked(slotKindProto, key("greeter", "unstable"), [32]byte{10}, 0, 0)
	if err != nil {
		t.Fatalf("cross-kind unstable swap: %v", err)
	}
	if existed {
		t.Fatalf("existed=true for cross-kind unstable swap")
	}
	if _, stillThere := g.openAPISources[key("greeter", "unstable")]; stillThere {
		t.Errorf("evictSlotLocked left openAPISources[%v] populated after kind change", key("greeter", "unstable"))
	}
	got := g.slots[key("greeter", "unstable")]
	if got == nil || got.kind != slotKindProto {
		t.Errorf("slot did not adopt new kind: %+v", got)
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
