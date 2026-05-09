package gateway

import "fmt"

// slotKind tags which registration form occupies a slot. Plan §4's
// tier model is single-slot per (namespace, version), and a single
// occupant means single kind: the same coordinate cannot be claimed
// by both proto and openapi at once. Cross-kind collision is what
// the slot index catches that the per-kind maps couldn't.
type slotKind int

const (
	slotKindProto slotKind = iota + 1
	slotKindOpenAPI
	slotKindGraphQL
)

func (k slotKind) String() string {
	switch k {
	case slotKindProto:
		return "proto"
	case slotKindOpenAPI:
		return "openapi"
	case slotKindGraphQL:
		return "graphql"
	default:
		return "unknown"
	}
}

// slot is the per-(namespace, version) registration record. One slot
// per (ns, ver); the slot's kind picks which per-kind structure
// (g.pools / g.openAPISources / g.graphQLSources) holds the dispatch
// state.
//
// Phase 1 (this commit) wires the slot index alongside the existing
// per-kind maps and centralizes the §4 tier policy at
// `registerSlotLocked`. Phase 2 will pre-distill `*ir.Service` at
// registration into a `slot.ir` field, drop the three parallel maps,
// and make schema rebuild iterate slots — see plan §4.
type slot struct {
	key  poolKey
	kind slotKind

	// Schema-equality material. Two registrations with identical hash
	// and caps are interchangeable (multi-replica add); anything else
	// is either a swap (unstable) or a reject (vN).
	hash                      [32]byte
	maxConcurrency            int
	maxConcurrencyPerInstance int
}

// registerSlotLocked applies the §4 tier policy at registration time.
// The four outcomes:
//
//   - No slot exists: insert + return existed=false. Caller does its
//     kind-specific creation (allocate sems, build pool/source, add
//     replica, schema rebuild).
//   - Slot exists, identical (kind + hash + caps): return
//     existed=true. Caller does its kind-specific add-replica only.
//   - Slot exists, version="unstable", anything different: evict the
//     prior slot's per-kind storage and insert fresh. Returns
//     existed=false so the caller proceeds with creation. Plan §4
//     single-overwrite-slot semantics.
//   - Slot exists, version="vN", anything different: reject with a
//     descriptive error — cross-kind, schema-hash, or caps as the
//     differentiator. vN is locked once registered.
//
// Caller holds g.mu.
func (g *Gateway) registerSlotLocked(kind slotKind, key poolKey, hash [32]byte, maxConcurrency, maxConcurrencyPerInstance int) (existed bool, err error) {
	if g.slots == nil {
		g.slots = map[poolKey]*slot{}
	}
	s, ok := g.slots[key]
	if !ok {
		g.slots[key] = &slot{
			key:                       key,
			kind:                      kind,
			hash:                      hash,
			maxConcurrency:            maxConcurrency,
			maxConcurrencyPerInstance: maxConcurrencyPerInstance,
		}
		return false, nil
	}
	sameKind := s.kind == kind
	sameHash := s.hash == hash
	sameCaps := s.maxConcurrency == maxConcurrency && s.maxConcurrencyPerInstance == maxConcurrencyPerInstance
	if sameKind && sameHash && sameCaps {
		return true, nil
	}
	if key.version != "unstable" {
		switch {
		case !sameKind:
			return false, fmt.Errorf("%s/%s already registered as %s; cannot re-register as %s",
				key.namespace, key.version, s.kind, kind)
		case !sameHash:
			return false, fmt.Errorf("%s/%s already registered with different schema hash; vN is locked once registered (cut v(N+1) for a new schema, or use unstable for the mutable slot)",
				key.namespace, key.version)
		default:
			return false, fmt.Errorf("%s/%s already registered with different concurrency caps (have max=%d/inst=%d, got max=%d/inst=%d)",
				key.namespace, key.version, s.maxConcurrency, s.maxConcurrencyPerInstance, maxConcurrency, maxConcurrencyPerInstance)
		}
	}
	// unstable swap: evict the prior occupant — including its kind-
	// specific storage if the kind changed — and install a fresh slot
	// record. The caller's creation path then builds a fresh per-kind
	// struct under the new shape. Old replicas tracked by their owner
	// registration get cleaned up lazily on Deregister /
	// heartbeat-eviction; conn refcounts (cp.conns standalone,
	// reconciler.conns cluster) decrement on the same path.
	g.evictSlotLocked(s)
	g.slots[key] = &slot{
		key:                       key,
		kind:                      kind,
		hash:                      hash,
		maxConcurrency:            maxConcurrency,
		maxConcurrencyPerInstance: maxConcurrencyPerInstance,
	}
	return false, nil
}

// evictSlotLocked drops the per-kind dispatch storage backing a slot.
// Used by unstable swaps when the prior occupant is being replaced.
// The slot index entry is left to the caller (overwritten with the
// new occupant in the swap path; deleted by `releaseSlotLocked` when
// the last replica leaves on the natural deregister path). Caller
// holds g.mu.
func (g *Gateway) evictSlotLocked(s *slot) {
	g.publishServiceChange(adminEventsActionDeregistered, s.key.namespace, s.key.version, "", 0)
	switch s.kind {
	case slotKindProto:
		delete(g.pools, s.key)
	case slotKindOpenAPI:
		delete(g.openAPISources, s.key)
	case slotKindGraphQL:
		delete(g.graphQLSources, s.key)
	}
}

// releaseSlotLocked drops the slot index entry. Called by per-kind
// removal helpers (`removeReplicaByIDLocked`,
// `removeReplicasByOwnerLocked`, and the OpenAPI / GraphQL analogues)
// when a pool/source empties and is deleted from its per-kind map —
// keeps the slot index synchronized with the per-kind maps. Caller
// holds g.mu.
func (g *Gateway) releaseSlotLocked(key poolKey) {
	delete(g.slots, key)
}
