package gateway

// stableVN tracks the highest-ever-seen "vN" cut per namespace.
// Plan §4 stable-alias support: at schema render time, if
// stableVN[ns] > 0 and a service with matching version is currently
// in the build, the renderer emits a `stable` sub-field on the
// namespace container aliasing that service's content.
//
// Monotonic — only advances. advanceStableLocked never decreases
// the recorded value, so deregistration of the current stable's vN
// does not roll the alias back. The accompanying RetractStable admin
// RPC (separate todo, plan §4) is the only path that decrements.
//
// Standalone gateways keep this map purely in-process. Cluster-wide
// nodes converge through the same reconciler-driven path that
// populates slots — every per-replica registration calls
// joinPoolLocked on every node, which advances stableVN.
//
// Cluster persistence (so a fresh node joining mid-life recovers
// the historical max even when the target vN is currently absent
// from the live registry) is the next step under this todo —
// dedicated KV bucket without TTL — and is tracked in plan §4 as
// a follow-up commit.

// advanceStableLocked records ns's vN as a candidate for the
// per-namespace "stable" alias. Only advances; never decreases.
// Called from every kind's registration path after the slot has
// been allocated. Caller holds g.mu. vN <= 0 (i.e. version=="unstable")
// is a no-op.
func (g *Gateway) advanceStableLocked(ns string, vN int) {
	if vN <= 0 {
		return
	}
	if g.stableVN == nil {
		g.stableVN = map[string]int{}
	}
	if cur := g.stableVN[ns]; vN > cur {
		g.stableVN[ns] = vN
	}
}

// stableSnapshotLocked returns a snapshot of the stable_vN map for
// passing into RenderGraphQLRuntimeFields. Returns nil when no
// namespace has registered a `vN` cut so far (so the renderer can
// short-circuit). Caller holds g.mu.
func (g *Gateway) stableSnapshotLocked() map[string]int {
	if len(g.stableVN) == 0 {
		return nil
	}
	out := make(map[string]int, len(g.stableVN))
	for k, v := range g.stableVN {
		out[k] = v
	}
	return out
}
