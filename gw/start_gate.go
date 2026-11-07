package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Start gate.
//
// A control-plane binding that declares depends is not routable until
// every dependency is present: for each dependency, at least one
// accepted line has a ready instance. Until then the binding is
// WAITING and is NOT added to the dispatch pool (no slot, no schema
// fields, no stable advance). When its deps become present it joins
// the pool like any other registration and is READY from then on.
// The flag is sticky: if a dependency later loses every ready
// instance, the binding keeps serving and is reported DEGRADED.
//
// Ready instance: a replica in the (namespace, line) slot. Only
// bindings that passed the gate join a slot, so "in the pool" and
// "ready" are the same set. readyInstancesLocked is the single place
// that counts them.
//
// Cluster mode: each registryValue carries Waiting. The node that
// received the Register (the owner, which holds the registration and
// re-Puts its keys on heartbeat) decides; reconcilers on every node
// skip waiting values and join the rest. A dependency registered on
// node B joins node A's pool through A's reconciler, which kicks A's
// gate loop, which promotes A's waiters by re-Putting their values
// with Waiting=false. Every node then joins them.

// gateTick is the period of the gate loop's safety re-evaluation, for
// changes that do not kick it (boot-time AddProto after start).
const gateTick = 2 * time.Second

// gateInstance is one registered instance of one binding: one
// ServiceBinding of one Register call. Standalone: written by
// Register / Deregister. Cluster: mirrored from every registry KV
// value by the reconciler, so each node knows the waiting instances
// of every other node too (admin listing, cycle check).
type gateInstance struct {
	key        poolKey
	hash       [32]byte
	regID      string
	replicaID  string // "" standalone
	addr       string
	instanceID string
	manifest   serviceManifest
	waiting    bool
}

// gateState is the gateway's start-gate bookkeeping. instances is
// guarded by g.mu; the kick channel is lazily created and safe to use
// from any goroutine.
type gateState struct {
	instances map[string]*gateInstance

	kickOnce sync.Once
	kickCh   chan struct{}
}

func (g *Gateway) gateKickCh() chan struct{} {
	g.gate.kickOnce.Do(func() { g.gate.kickCh = make(chan struct{}, 1) })
	return g.gate.kickCh
}

// kickGate asks the gate loop to re-evaluate waiting bindings.
// Non-blocking; safe with or without g.mu held.
func (g *Gateway) kickGate() {
	select {
	case g.gateKickCh() <- struct{}{}:
	default:
	}
}

func standaloneGateID(regID string, key poolKey) string {
	return "reg/" + regID + "/" + key.namespace + "/" + key.version
}

// setGateInstanceLocked upserts inst under id. A known instance that
// already passed the gate stays passed (sticky), so a stale re-Put
// with Waiting=true cannot take it out of service. Returns the
// effective waiting flag. A new waiting instance publishes a service
// change (it joins no pool, so nothing else does), so admin
// subscribers re-fetch and see it. Caller holds g.mu.
func (g *Gateway) setGateInstanceLocked(id string, inst *gateInstance) bool {
	if g.gate.instances == nil {
		g.gate.instances = map[string]*gateInstance{}
	}
	prev := g.gate.instances[id]
	if prev != nil && !prev.waiting {
		inst.waiting = false
	}
	g.gate.instances[id] = inst
	if prev == nil && inst.waiting {
		g.publishServiceChange(adminEventsActionRegistered, inst.key.namespace, inst.key.version, inst.addr, uint32(g.readyInstancesLocked(inst.key.namespace, inst.key.version)))
	}
	return inst.waiting
}

// removeGateInstanceLocked drops id and reports whether it was still
// waiting (so never joined a pool). Caller holds g.mu.
func (g *Gateway) removeGateInstanceLocked(id string) (wasWaiting bool) {
	inst := g.gate.instances[id]
	if inst == nil {
		return false
	}
	delete(g.gate.instances, id)
	if inst.waiting {
		g.publishServiceChange(adminEventsActionDeregistered, inst.key.namespace, inst.key.version, inst.addr, uint32(g.readyInstancesLocked(inst.key.namespace, inst.key.version)))
	}
	return inst.waiting
}

// removeGateInstancesByRegLocked drops every instance of regID.
// Caller holds g.mu.
func (g *Gateway) removeGateInstancesByRegLocked(regID string) {
	for id, inst := range g.gate.instances {
		if inst.regID == regID {
			delete(g.gate.instances, id)
		}
	}
}

// readyInstancesLocked returns how many ready instances serve
// (ns, line): the replicas in its slot that are not draining
// (instance_drain.go). In-process slots (internal proto, MCP) count
// as one. This is the "present" test of the start gate, and drain
// coverage counts the same set (readyOwnersLocked), so the two never
// disagree. Caller holds g.mu.
func (g *Gateway) readyInstancesLocked(ns, line string) int {
	owners, ownerless := g.readyOwnersLocked(ns, line)
	return len(owners) + ownerless
}

// readyOwnersLocked lists the registration IDs of the non-draining
// replicas in the (ns, line) slot, plus a count of ready replicas
// without a registration (boot-time AddProto / AddOpenAPI /
// AddGraphQL, in-process slots). WAITING bindings never join a slot,
// so they are not listed. Caller holds g.mu.
func (g *Gateway) readyOwnersLocked(ns, line string) (owners []string, ownerless int) {
	s := g.slots[poolKey{namespace: ns, version: line}]
	if s == nil {
		return nil, 0
	}
	add := func(owner string, h *replicaHealth) {
		if h.draining.Load() {
			return
		}
		if owner == "" {
			ownerless++
			return
		}
		owners = append(owners, owner)
	}
	switch s.kind {
	case slotKindProto:
		if s.proto != nil {
			if rs := s.proto.replicas.Load(); rs != nil {
				for _, r := range *rs {
					add(r.owner, &r.health)
				}
			}
		}
	case slotKindOpenAPI:
		if s.openapi != nil {
			if rs := s.openapi.replicas.Load(); rs != nil {
				for _, r := range *rs {
					add(r.owner, &r.health)
				}
			}
		}
	case slotKindGraphQL:
		if s.graphql != nil {
			if rs := s.graphql.replicas.Load(); rs != nil {
				for _, r := range *rs {
					add(r.owner, &r.health)
				}
			}
		}
	case slotKindInternalProto:
		if s.internalProto != nil {
			ownerless = 1
		}
	case slotKindMCP:
		if s.mcp != nil {
			ownerless = 1
		}
	}
	return owners, ownerless
}

// depsMetLocked reports whether every dependency has at least one
// accepted line with a ready instance. No deps → true.
func (g *Gateway) depsMetLocked(deps []manifestDependency) bool {
	for _, d := range deps {
		if len(g.presentLinesLocked(d)) == 0 {
			return false
		}
	}
	return true
}

func (g *Gateway) presentLinesLocked(d manifestDependency) []string {
	var out []string
	for _, l := range d.Lines {
		if g.readyInstancesLocked(d.Service, l) > 0 {
			out = append(out, l)
		}
	}
	return out
}

func (g *Gateway) dependencyStatusLocked(deps []manifestDependency) []*cpv1.DependencyStatus {
	if len(deps) == 0 {
		return nil
	}
	out := make([]*cpv1.DependencyStatus, 0, len(deps))
	for _, d := range deps {
		out = append(out, &cpv1.DependencyStatus{
			Service:      d.Service,
			Lines:        append([]string(nil), d.Lines...),
			PresentLines: g.presentLinesLocked(d),
		})
	}
	return out
}

// gateStateLocked maps (waiting flag, current dep presence) to the
// reported state.
func (g *Gateway) gateStateLocked(waiting bool, deps []manifestDependency) cpv1.InstanceState {
	switch {
	case waiting:
		return cpv1.InstanceState_INSTANCE_STATE_WAITING
	case g.depsMetLocked(deps):
		return cpv1.InstanceState_INSTANCE_STATE_READY
	default:
		return cpv1.InstanceState_INSTANCE_STATE_DEGRADED
	}
}

// missingDeps renders the deps with no present line, for logs and
// errors: "library(v1|v2), users(unstable)".
func missingDeps(st []*cpv1.DependencyStatus) string {
	var parts []string
	for _, d := range st {
		if len(d.GetPresentLines()) == 0 {
			parts = append(parts, d.GetService()+"("+strings.Join(d.GetLines(), "|")+")")
		}
	}
	return strings.Join(parts, ", ")
}

// gateInstancesLocked returns the known instances grouped by
// (namespace, version), each list sorted for stable output.
func (g *Gateway) gateInstancesLocked() map[poolKey][]*gateInstance {
	out := map[poolKey][]*gateInstance{}
	for _, inst := range g.gate.instances {
		out[inst.key] = append(out[inst.key], inst)
	}
	for _, l := range out {
		sort.Slice(l, func(i, j int) bool {
			if l[i].regID != l[j].regID {
				return l[i].regID < l[j].regID
			}
			return l[i].replicaID < l[j].replicaID
		})
	}
	return out
}

func (g *Gateway) instanceStatusLocked(inst *gateInstance) *cpv1.InstanceStatus {
	return &cpv1.InstanceStatus{
		ReplicaId:      inst.replicaID,
		RegistrationId: inst.regID,
		InstanceId:     inst.instanceID,
		Addr:           inst.addr,
		State:          g.gateStateLocked(inst.waiting, inst.manifest.Depends),
		Depends:        g.dependencyStatusLocked(inst.manifest.Depends),
	}
}

func (cp *controlPlane) instanceStatusesLocked(l []*gateInstance) []*cpv1.InstanceStatus {
	if len(l) == 0 {
		return nil
	}
	out := make([]*cpv1.InstanceStatus, 0, len(l))
	for _, inst := range l {
		out = append(out, cp.gw.instanceStatusLocked(inst))
	}
	return out
}

// gateEdges is one binding's outgoing dependency edges for the cycle
// check.
type gateEdges struct {
	from poolKey
	deps []manifestDependency
}

// checkDependencyCycleLocked refuses add when its edges, together
// with every known instance's and slot's manifest, close a cycle in
// the (service, line) graph. Every accepted line is an edge: a
// consumer accepting library v1|v2 may end up on either.
//
// FailedPrecondition, not InvalidArgument: the same request is valid
// against a registry where the other side of the cycle is absent, so
// the refusal is about system state, not the argument alone.
// Caller holds g.mu.
func (g *Gateway) checkDependencyCycleLocked(add []gateEdges) error {
	adj := map[poolKey]map[poolKey]bool{}
	addEdges := func(from poolKey, deps []manifestDependency) {
		for _, d := range deps {
			for _, l := range d.Lines {
				if adj[from] == nil {
					adj[from] = map[poolKey]bool{}
				}
				adj[from][poolKey{namespace: d.Service, version: l}] = true
			}
		}
	}
	for k, s := range g.slots {
		addEdges(k, s.manifest.Depends)
	}
	for _, inst := range g.gate.instances {
		addEdges(inst.key, inst.manifest.Depends)
	}
	for _, e := range add {
		addEdges(e.from, e.deps)
	}
	for _, e := range add {
		if path := findCycle(adj, e.from); path != nil {
			names := make([]string, len(path))
			for i, k := range path {
				names[i] = k.namespace + "/" + k.version
			}
			return status.Errorf(codes.FailedPrecondition,
				"controlplane: depends close a cycle: %s", strings.Join(names, " -> "))
		}
	}
	return nil
}

// findCycle returns a path start -> ... -> start, or nil. Neighbours
// are visited in sorted order so the reported cycle is deterministic.
func findCycle(adj map[poolKey]map[poolKey]bool, start poolKey) []poolKey {
	seen := map[poolKey]bool{}
	var path []poolKey
	var dfs func(k poolKey) bool
	dfs = func(k poolKey) bool {
		path = append(path, k)
		for _, n := range sortedKeys(adj[k]) {
			if n == start {
				path = append(path, n)
				return true
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			if dfs(n) {
				return true
			}
		}
		path = path[:len(path)-1]
		return false
	}
	if dfs(start) {
		return path
	}
	return nil
}

func sortedKeys(m map[poolKey]bool) []poolKey {
	out := make([]poolKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].namespace != out[j].namespace {
			return out[i].namespace < out[j].namespace
		}
		return out[i].version < out[j].version
	})
	return out
}

// checkWaitingSlotLocked refuses a waiting binding that could never
// join: the (namespace, vN) slot is already held by a different kind
// or schema. A ready binding gets the same refusal from
// registerSlotLocked; without this check a waiting one would be
// accepted and fail later, when its deps arrive. unstable swaps, so
// it never conflicts. Caller holds g.mu.
func (g *Gateway) checkWaitingSlotLocked(kind slotKind, key poolKey, hash [32]byte) error {
	s := g.slots[key]
	if s == nil || key.version == "unstable" {
		return nil
	}
	if s.kind != kind {
		return fmt.Errorf("controlplane: %s/%s already registered as %s; cannot re-register as %s", key.namespace, key.version, s.kind, kind)
	}
	if s.hash != hash {
		return fmt.Errorf("controlplane: %s/%s already registered with different schema hash; vN is locked once registered", key.namespace, key.version)
	}
	return nil
}

// ---------------------------------------------------------------------
// Control-plane side: the owner of a registration decides when its
// waiting bindings pass the gate.
// ---------------------------------------------------------------------

// regBinding is the gate state of one binding of a registration.
type regBinding struct {
	prep    preparedBinding
	waiting bool

	// Cluster mode: index into registration.kvKeys and the value to
	// re-marshal on promotion.
	kvIdx int
	val   registryValue

	// joinErr is the last deferred-join failure (standalone), logged
	// once per distinct error.
	joinErr string
}

func (p *preparedBinding) key() poolKey {
	return poolKey{namespace: p.namespace, version: p.version}
}

func (p *preparedBinding) kind() slotKind {
	switch {
	case p.isGraphQL:
		return slotKindGraphQL
	case p.isOpenAPI:
		return slotKindOpenAPI
	default:
		return slotKindProto
	}
}

// admitLocked runs the gate checks of a Register: refuses a cycle,
// and reports per binding whether it must wait. Caller holds g.mu.
func (g *Gateway) admitLocked(prep []preparedBinding) ([]bool, error) {
	edges := make([]gateEdges, 0, len(prep))
	for _, p := range prep {
		if len(p.manifest.Depends) > 0 {
			edges = append(edges, gateEdges{from: p.key(), deps: p.manifest.Depends})
		}
	}
	if err := g.checkDependencyCycleLocked(edges); err != nil {
		return nil, err
	}
	waiting := make([]bool, len(prep))
	for i, p := range prep {
		waiting[i] = !g.depsMetLocked(p.manifest.Depends)
	}
	return waiting, nil
}

// bindingStatusesLocked reports the gate state of reg's bindings in
// request order. Caller holds cp.mu and g.mu.
func (cp *controlPlane) bindingStatusesLocked(reg *registration) []*cpv1.BindingStatus {
	g := cp.gw
	out := make([]*cpv1.BindingStatus, 0, len(reg.bindings))
	for _, b := range reg.bindings {
		out = append(out, &cpv1.BindingStatus{
			Namespace: b.prep.namespace,
			Version:   b.prep.version,
			State:     g.gateStateLocked(b.waiting, b.prep.manifest.Depends),
			Depends:   g.dependencyStatusLocked(b.prep.manifest.Depends),
		})
	}
	return out
}

func (cp *controlPlane) logf(format string, args ...any) {
	if cl := cp.gw.cfg.cluster; cl != nil && cl.Server != nil {
		cl.Server.Warnf(format, args...)
		return
	}
	fmt.Printf(format+"\n", args...)
}

// gateLoop re-evaluates waiting bindings on every kick (registration
// change) and every gateTick. Stops with the gateway.
func (cp *controlPlane) gateLoop() {
	t := time.NewTicker(gateTick)
	defer t.Stop()
	kick := cp.gw.gateKickCh()
	for {
		select {
		case <-cp.gw.life.Done():
			return
		case <-t.C:
		case <-kick:
		}
		cp.evaluateGate()
	}
}

func (cp *controlPlane) evaluateGate() {
	cp.gw.lifecycle.observeGate()
	defer cp.gw.lifecycle.observeGate()
	if cp.gw.registryKV() != nil {
		cp.promoteCluster()
		return
	}
	cp.mu.Lock()
	cp.gw.mu.Lock()
	cp.promoteStandaloneLocked()
	cp.gw.mu.Unlock()
	cp.mu.Unlock()
}

// promoteStandaloneLocked joins every waiting binding whose deps are
// present, repeating until nothing changes (a promotion can satisfy
// another waiter). A join that fails (the slot was taken by another
// schema meanwhile) leaves the binding waiting and is logged once.
// Caller holds cp.mu and g.mu.
func (cp *controlPlane) promoteStandaloneLocked() {
	g := cp.gw
	for changed := true; changed; {
		changed = false
		for _, reg := range cp.regs {
			if reg.kvKeys != nil {
				continue // registered through the cluster path
			}
			for _, b := range reg.bindings {
				if !b.waiting || !g.depsMetLocked(b.prep.manifest.Depends) {
					continue
				}
				if err := cp.joinPreparedLocked(reg.id, reg.addr, reg.conn, b.prep); err != nil {
					if msg := err.Error(); msg != b.joinErr {
						b.joinErr = msg
						cp.logf("controlplane: start gate: %s/%s deps present but join failed: %v", b.prep.namespace, b.prep.version, err)
					}
					continue
				}
				b.waiting = false
				b.joinErr = ""
				if inst := g.gate.instances[standaloneGateID(reg.id, b.prep.key())]; inst != nil {
					inst.waiting = false
				}
				changed = true
			}
		}
	}
}

// promoteCluster re-Puts, with Waiting=false, the registry value of
// every local waiting binding whose deps are present. The reconcilers
// (this node's included) then join it. One pass; the joins it causes
// kick the loop again, which covers chains.
func (cp *controlPlane) promoteCluster() {
	type cand struct {
		reg *registration
		b   *regBinding
	}
	var cands []cand
	cp.mu.Lock()
	cp.gw.mu.Lock()
	for _, reg := range cp.regs {
		for _, b := range reg.bindings {
			if b.waiting && reg.kvKeys != nil && cp.gw.depsMetLocked(b.prep.manifest.Depends) {
				cands = append(cands, cand{reg, b})
			}
		}
	}
	cp.gw.mu.Unlock()
	cp.mu.Unlock()
	kv := cp.gw.registryKV()
	if kv == nil {
		return
	}
	for _, c := range cands {
		// putMu orders this Put against heartbeat re-Puts of the same
		// keys, so a heartbeat cannot land the old Waiting=true value
		// after the promotion.
		c.reg.putMu.Lock()
		cp.mu.Lock()
		if cp.regs[c.reg.id] != c.reg || !c.b.waiting {
			cp.mu.Unlock()
			c.reg.putMu.Unlock()
			continue
		}
		c.b.waiting = false
		c.b.val.Waiting = false
		raw, err := json.Marshal(c.b.val)
		if err != nil {
			c.b.waiting, c.b.val.Waiting = true, true
			cp.mu.Unlock()
			c.reg.putMu.Unlock()
			continue
		}
		ref := &c.reg.kvKeys[c.b.kvIdx]
		ref.value = raw
		k := replicaKey(ref.namespace, ref.version, ref.replicaID)
		cp.mu.Unlock()
		kctx, cancel := context.WithTimeout(cp.gw.life, 2*kvCallTimeout)
		if err := kvPutRetry(kctx, kv, k, raw); err != nil {
			// kvKeys already holds the promoted value; the next
			// heartbeat re-Puts it.
			cp.logf("controlplane: start gate: promote %s: %v", k, err)
		}
		cancel()
		c.reg.putMu.Unlock()
	}
}

// logWaiting logs each waiting binding of a fresh registration with
// the deps it waits for.
func (cp *controlPlane) logWaiting(regID string, st []*cpv1.BindingStatus) {
	for _, b := range st {
		if b.GetState() == cpv1.InstanceState_INSTANCE_STATE_WAITING {
			cp.logf("controlplane: start gate: %s/%s (registration %s) waiting for %s",
				b.GetNamespace(), b.GetVersion(), regID, missingDeps(b.GetDepends()))
		}
	}
}
