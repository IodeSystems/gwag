package gateway

import (
	"fmt"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

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
// per (ns, ver); the slot's kind picks which per-kind dispatch
// handle (`proto` / `openapi` / `graphql`) carries the format-specific
// state. The slot is the canonical management-side index — every
// registration goes through `registerSlotLocked`, every iteration
// (schema rebuild, admin lists, FDS export, dispatcher registration,
// inject inventory, reconciler delete-routing) goes through
// `g.slots`.
//
// `ir` is the request-ready IR baked at slot creation: ingest +
// internal-flag stamp + `applySchemaRewrites` (HideType / HidePath /
// NullableType / NullablePath — the schema-half of every Transform) +
// proto subscription auth-arg injection + `PopulateSchemaIDs`. Schema
// rebuild reads this directly; no per-kind walk, no transform pass.
// `Use(...)` is the single invalidator: it appends to `g.transforms`
// and re-bakes every slot's IR (cheap; ingest + transforms over
// already-loaded handles).
type slot struct {
	key  poolKey
	kind slotKind

	// Schema-equality material. Two registrations with identical hash
	// and caps are interchangeable (multi-replica add); anything else
	// is either a swap (unstable) or a reject (vN).
	hash                      [32]byte
	maxConcurrency            int
	maxConcurrencyPerInstance int

	// Request-ready IR. Populated by `bakeSlotIRLocked` on slot
	// creation and re-baked on `Use(...)`. nil only between slot
	// allocation and the per-kind handle being attached (a transient
	// in-mu state inside `joinPoolLocked` / `addOpenAPISourceLocked`
	// / `addGraphQLSourceLocked`).
	ir []*ir.Service

	// Exactly one of these is non-nil based on kind. The per-kind
	// struct holds the format handle (`*pool.file`, `*openAPISource.doc`,
	// `*graphQLSource.introspection`), the original schema bytes for
	// `/schema/*` re-emit, the replicas, and the backpressure sems.
	proto   *pool
	openapi *openAPISource
	graphql *graphQLSource
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
	if err := g.checkVersionTierAllowed(key.version); err != nil {
		return false, err
	}
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

// evictSlotLocked drops the per-kind dispatch storage and IR cache
// backing a slot. Used by unstable swaps when the prior occupant is
// being replaced. The slot index entry is overwritten by the swap
// path with the new occupant. Caller holds g.mu.
func (g *Gateway) evictSlotLocked(s *slot) {
	g.publishServiceChange(adminEventsActionDeregistered, s.key.namespace, s.key.version, "", 0)
	s.proto = nil
	s.openapi = nil
	s.graphql = nil
	s.ir = nil
}

// releaseSlotLocked drops the slot index entry. Called by per-kind
// removal helpers (`removeReplicaByIDLocked`,
// `removeReplicasByOwnerLocked`, and the OpenAPI / GraphQL analogues)
// when a pool/source empties on the natural deregister path. Caller
// holds g.mu.
func (g *Gateway) releaseSlotLocked(key poolKey) {
	delete(g.slots, key)
}

// protoSlot returns the proto pool stored on the slot at `key`, or
// nil if no slot exists at `key` or the slot's kind is not proto.
// Caller holds g.mu.
func (g *Gateway) protoSlot(key poolKey) *pool {
	s := g.slots[key]
	if s == nil || s.kind != slotKindProto {
		return nil
	}
	return s.proto
}

// openAPISlot returns the OpenAPI source stored on the slot at `key`,
// or nil. Caller holds g.mu.
func (g *Gateway) openAPISlot(key poolKey) *openAPISource {
	s := g.slots[key]
	if s == nil || s.kind != slotKindOpenAPI {
		return nil
	}
	return s.openapi
}

// graphQLSlot returns the GraphQL source stored on the slot at `key`,
// or nil. Caller holds g.mu.
func (g *Gateway) graphQLSlot(key poolKey) *graphQLSource {
	s := g.slots[key]
	if s == nil || s.kind != slotKindGraphQL {
		return nil
	}
	return s.graphql
}

// bakeSlotIRLocked fills `s.ir` from the slot's per-kind handle,
// applying the gateway's current schema-half transforms in the same
// order the legacy `*ServicesAsIRLocked` walks did:
//
//  1. Ingest the format handle into raw `[]*ir.Service`.
//  2. Stamp Namespace / Version / Internal on each.
//  3. Apply `HideInternal` (drops services whose namespace is `_*`
//     so they never reach the public schema).
//  4. Apply every Transform.Schema rewrite (HideType / HidePath /
//     NullableType / NullablePath — the IR-mutating injectors).
//  5. Inject HMAC subscription auth args (proto only).
//  6. Populate SchemaIDs so dispatchers can be looked up by
//     op.SchemaID at request time.
//
// Caller holds g.mu. The slot's per-kind handle (`s.proto` /
// `s.openapi` / `s.graphql`) must be set before calling.
func (g *Gateway) bakeSlotIRLocked(s *slot) {
	var raw []*ir.Service
	switch s.kind {
	case slotKindProto:
		if s.proto == nil {
			return
		}
		raw = ir.IngestProto(s.proto.file)
	case slotKindOpenAPI:
		if s.openapi == nil {
			return
		}
		raw = []*ir.Service{ir.IngestOpenAPI(s.openapi.doc)}
	case slotKindGraphQL:
		if s.graphql == nil {
			return
		}
		svc, err := ir.IngestGraphQL(s.graphql.rawIntrospection)
		if err != nil {
			s.ir = nil
			return
		}
		raw = []*ir.Service{svc}
	default:
		return
	}
	for _, svc := range raw {
		svc.Namespace = s.key.namespace
		svc.Version = s.key.version
		svc.Internal = g.isInternal(s.key.namespace)
	}
	raw = ir.HideInternal(raw)
	g.applySchemaRewrites(raw)
	if s.kind == slotKindProto {
		for _, svc := range raw {
			injectProtoSubscriptionAuthArgs(svc)
		}
	}
	for _, svc := range raw {
		ir.PopulateSchemaIDs(svc)
	}
	s.ir = raw
}

// rebakeAllSlotsLocked re-bakes every slot's IR. Use(...) calls this
// after appending to `g.transforms` so the cached IR reflects the
// new injector set without waiting for a register/dereg event.
// Caller holds g.mu.
func (g *Gateway) rebakeAllSlotsLocked() {
	for _, s := range g.slots {
		g.bakeSlotIRLocked(s)
	}
}

// collectSlotIRLocked walks every slot whose key matches `filter` and
// returns the concatenation of each slot's pre-baked IR. The result
// is the same shape the legacy `*ServicesAsIRLocked` functions
// produced — already post-transform, post-internal-filter, with
// SchemaIDs populated — but without the per-kind ingest each rebuild.
// Caller holds g.mu.
func (g *Gateway) collectSlotIRLocked(filter schemaFilter) []*ir.Service {
	out := make([]*ir.Service, 0, len(g.slots))
	for _, s := range g.slots {
		if !filter.matchPool(s.key) {
			continue
		}
		out = append(out, s.ir...)
	}
	return out
}
