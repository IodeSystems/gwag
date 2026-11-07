package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Per-instance drain. A deploy system (or the registration client on
// SIGTERM) asks the gateway to drain one registration. The gateway:
//
//  1. checks coverage: for every line L the instance serves and every
//     dependent D whose depends accept L, D must keep at least one
//     accepted line with a ready instance other than this one. If
//     not, the drain is BLOCKED: routing continues, reasons name the
//     dependents. Declared external dependents (external_dependents.go)
//     count as dependents here, review-due ones included.
//  2. otherwise stops picking the instance's replicas (every line,
//     every kind) and waits for in-flight requests to it to reach 0.
//  3. reports SAFE_TO_STOP.
//
// The state is recomputed on every tick, so a BLOCKED drain proceeds
// by itself when another covering instance registers, and a drain
// whose cover disappears goes back to BLOCKED and routing resumes.
//
// Two drains that need each other's cover are ordered by request
// time: the earlier request may count the later-requested instance as
// cover, not the reverse, so one of them proceeds instead of both
// blocking.
//
// Cluster: the request is a key in the gwag-drain KV bucket, so every
// node stops routing. Each node writes an ack (routing stopped, its
// in-flight count). SAFE_TO_STOP needs a matching ack with routing
// stopped and inflight 0 from every live peer.

const (
	drainBucketName = "gwag-drain"

	// drainTick is how often each node re-evaluates requested drains.
	drainTick = 200 * time.Millisecond

	// drainSettle is how long routing must have been stopped before
	// an in-flight count of 0 is trusted. A dispatch that picked the
	// replica just before the flag was set increments inflight after
	// the pick; the settle window covers that gap.
	drainSettle = 100 * time.Millisecond

	// drainAckRefresh re-puts an unchanged ack so a slow watcher
	// recovers it.
	drainAckRefresh = 5 * time.Second
)

// Drain state names, as reported by the admin API.
const (
	drainStateServing    = "serving"
	drainStateDraining   = "draining"
	drainStateBlocked    = "blocked"
	drainStateSafeToStop = "safe_to_stop"
)

var errDrainUnknownInstance = errors.New("unknown instance")

// readyInstancesFunc returns the registration IDs of instances that
// can serve (namespace, line) right now. Coverage counts only these.
type readyInstancesFunc func(ns, line string) []string

// instanceRec is what this node knows about one registration: its
// lines and the manifest each line carries.
type instanceRec struct {
	regID      string
	instanceID string
	addr       string
	lines      map[poolKey]serviceManifest
}

// drainRequest is the KV value under "req.<regID>".
type drainRequest struct {
	RegID             string `json:"reg_id"`
	RequestedUnixNano int64  `json:"requested_unix_nano"`
}

// drainAck is the KV value under "ack.<regID>.<nodeID>": one node's
// view of one drain request.
type drainAck struct {
	NodeID            string `json:"node_id"`
	RequestedUnixNano int64  `json:"requested_unix_nano"`
	Stopped           bool   `json:"stopped"`
	Blocked           bool   `json:"blocked,omitempty"`
	Inflight          int32  `json:"inflight"`
}

// localDrain is this node's evaluation of one requested drain.
type localDrain struct {
	stoppedSince time.Time // zero while routing
	reasons      []string
	ack          drainAck
	ackPutAt     time.Time
}

// instanceDrains holds the instance index and drain state. The zero
// value is ready to use. mu is a leaf lock: never take g.mu while
// holding it.
type instanceDrains struct {
	mu         sync.Mutex
	regs       map[string]*instanceRec
	replicaReg map[string]string // "ns/ver/replicaID" → regID (cluster deletes)
	requests   map[string]drainRequest
	acks       map[string]map[string]drainAck // regID → nodeID → ack
	local      map[string]*localDrain
	flagged    map[string]bool // regIDs whose replicas carry draining=true
	gone       []string        // regIDs forgotten while a request existed

	// ready is the coverage source; nil → g.defaultReadyInstances
	// (the start gate's readyOwnersLocked).
	ready readyInstancesFunc

	tickMu   sync.Mutex
	loopOnce sync.Once
	kv       jetstream.KeyValue // cluster only
}

func (d *instanceDrains) initLocked() {
	if d.regs == nil {
		d.regs = map[string]*instanceRec{}
		d.replicaReg = map[string]string{}
		d.requests = map[string]drainRequest{}
		d.acks = map[string]map[string]drainAck{}
		d.local = map[string]*localDrain{}
		d.flagged = map[string]bool{}
	}
}

// noteInstanceLine records that regID serves (ns, ver). replicaID is
// the registry KV replica id in cluster mode, "" standalone.
func (g *Gateway) noteInstanceLine(regID, instanceID, addr, ns, ver, replicaID string, m serviceManifest) {
	if regID == "" {
		return
	}
	d := &g.drains
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initLocked()
	rec := d.regs[regID]
	if rec == nil {
		rec = &instanceRec{regID: regID, lines: map[poolKey]serviceManifest{}}
		d.regs[regID] = rec
	}
	rec.instanceID = instanceID
	rec.addr = addr
	rec.lines[poolKey{namespace: ns, version: ver}] = m
	if replicaID != "" {
		d.replicaReg[ns+"/"+ver+"/"+replicaID] = regID
	}
}

// forgetInstanceReplica drops one cluster replica from the index; the
// registration goes when its last line goes.
func (g *Gateway) forgetInstanceReplica(ns, ver, replicaID string) {
	d := &g.drains
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initLocked()
	k := ns + "/" + ver + "/" + replicaID
	regID, ok := d.replicaReg[k]
	if !ok {
		return
	}
	delete(d.replicaReg, k)
	rec := d.regs[regID]
	if rec == nil {
		return
	}
	delete(rec.lines, poolKey{namespace: ns, version: ver})
	if len(rec.lines) == 0 {
		d.forgetLocked(regID)
	}
}

// forgetInstance drops a registration from the index (Deregister,
// TTL eviction).
func (g *Gateway) forgetInstance(regID string) {
	d := &g.drains
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initLocked()
	if _, ok := d.regs[regID]; !ok {
		return
	}
	for k, id := range d.replicaReg {
		if id == regID {
			delete(d.replicaReg, k)
		}
	}
	d.forgetLocked(regID)
}

func (d *instanceDrains) forgetLocked(regID string) {
	delete(d.regs, regID)
	if _, ok := d.requests[regID]; ok {
		delete(d.requests, regID)
		d.gone = append(d.gone, regID)
	}
	delete(d.local, regID)
	delete(d.flagged, regID)
}

// defaultReadyInstances is the start gate's notion of ready
// (readyOwnersLocked): non-draining replicas in the (ns, line) slot,
// so WAITING bindings never count as cover. A ready replica with no
// registration (boot-time, in-process) is reported as "", which no
// drain can target, so it always covers. Takes g.mu; called with no
// lock held.
func (g *Gateway) defaultReadyInstances(ns, line string) []string {
	g.mu.Lock()
	owners, ownerless := g.readyOwnersLocked(ns, line)
	g.mu.Unlock()
	for i := 0; i < ownerless; i++ {
		owners = append(owners, "")
	}
	return owners
}

// resolveInstance maps an operator-typed name (registration_id, or an
// instance_id carried by exactly one registration) to a regID.
func (d *instanceDrains) resolveInstance(name string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initLocked()
	if name == "" {
		return "", fmt.Errorf("instance is empty")
	}
	if _, ok := d.regs[name]; ok {
		return name, nil
	}
	var matches []string
	for id, rec := range d.regs {
		if rec.instanceID == name {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w %q", errDrainUnknownInstance, name)
	case 1:
		return matches[0], nil
	}
	sort.Strings(matches)
	return "", fmt.Errorf("instance_id %q matches %d registrations (%s); pass a registration_id", name, len(matches), strings.Join(matches, ", "))
}

// drainBefore reports whether request a was made before request b.
// Ties break on regID so every node orders them the same way.
func drainBefore(a, b drainRequest) bool {
	if a.RequestedUnixNano != b.RequestedUnixNano {
		return a.RequestedUnixNano < b.RequestedUnixNano
	}
	return a.RegID < b.RegID
}

// coverageReasons returns one reason per dependent that would be left
// without a covering line if target stopped. Empty = safe to drain.
// Takes no lock while calling the ready func.
func (g *Gateway) coverageReasons(target string) []string {
	d := &g.drains
	type depLine struct {
		who string // "ns/ver", or an external dependent's label
		dep manifestDependency
	}
	d.mu.Lock()
	rec := d.regs[target]
	if rec == nil {
		d.mu.Unlock()
		return nil
	}
	myReq := d.requests[target]
	var served []poolKey
	for k := range rec.lines {
		served = append(served, k)
	}
	var deps []depLine
	for id, other := range d.regs {
		if id == target {
			continue
		}
		for k, m := range other.lines {
			for _, dep := range m.Depends {
				deps = append(deps, depLine{who: k.namespace + "/" + k.version, dep: dep})
			}
		}
	}
	requests := make(map[string]drainRequest, len(d.requests))
	for id, r := range d.requests {
		requests[id] = r
	}
	ready := d.ready
	d.mu.Unlock()
	for _, x := range g.externals.all() {
		for _, dep := range x.Depends {
			deps = append(deps, depLine{who: x.label(), dep: dep})
		}
	}
	if ready == nil {
		ready = g.defaultReadyInstances
	}

	available := func(id string) bool {
		if id == target {
			return false
		}
		r, draining := requests[id]
		return !draining || drainBefore(myReq, r)
	}
	seen := map[string]bool{}
	var reasons []string
	for _, k := range served {
		for _, dl := range deps {
			if dl.dep.Service != k.namespace || !containsString(dl.dep.Lines, k.version) {
				continue
			}
			covered := false
			for _, line := range dl.dep.Lines {
				for _, id := range ready(k.namespace, line) {
					if available(id) {
						covered = true
						break
					}
				}
				if covered {
					break
				}
			}
			if covered {
				continue
			}
			r := fmt.Sprintf("%s accepts %s [%s]: no other ready instance", dl.who, k.namespace, strings.Join(dl.dep.Lines, ", "))
			if !seen[r] {
				seen[r] = true
				reasons = append(reasons, r)
			}
		}
	}
	sort.Strings(reasons)
	return reasons
}

// setInstanceRoutingLocked sets the draining flag on every replica
// regID owns and returns the requests in flight (or queued on a
// per-instance cap) to them on this node. Caller holds g.mu.
func (g *Gateway) setInstanceRoutingLocked(regID string, draining bool) int32 {
	var busy int32
	for _, s := range g.slots {
		switch {
		case s.proto != nil:
			if rs := s.proto.replicas.Load(); rs != nil {
				for _, r := range *rs {
					if r.owner == regID {
						r.health.draining.Store(draining)
						busy += r.inflight.Load() + r.queueing.Load()
					}
				}
			}
		case s.openapi != nil:
			if rs := s.openapi.replicas.Load(); rs != nil {
				for _, r := range *rs {
					if r.owner == regID {
						r.health.draining.Store(draining)
						busy += r.inflight.Load() + r.queueing.Load()
					}
				}
			}
		case s.graphql != nil:
			if rs := s.graphql.replicas.Load(); rs != nil {
				for _, r := range *rs {
					if r.owner == regID {
						r.health.draining.Store(draining)
						busy += r.inflight.Load()
					}
				}
			}
		}
	}
	return busy
}

// drainTickOnce re-evaluates every requested drain on this node:
// coverage, replica flags, in-flight count, and (cluster) the ack.
func (g *Gateway) drainTickOnce(ctx context.Context) {
	d := &g.drains
	d.tickMu.Lock()
	defer d.tickMu.Unlock()

	d.mu.Lock()
	d.initLocked()
	type item struct {
		regID string
		req   drainRequest
		known bool
	}
	var items []item
	for id, r := range d.requests {
		_, known := d.regs[id]
		items = append(items, item{regID: id, req: r, known: known})
	}
	var unflag []string
	for id := range d.flagged {
		if _, ok := d.requests[id]; !ok {
			unflag = append(unflag, id)
		}
	}
	gone := d.gone
	d.gone = nil
	kv := d.kv
	d.mu.Unlock()

	// Cancelled drains: routing resumes.
	if len(unflag) > 0 {
		g.mu.Lock()
		for _, id := range unflag {
			g.setInstanceRoutingLocked(id, false)
		}
		g.mu.Unlock()
		// Back in the ready set: waiters may now pass the gate.
		g.kickGate()
		d.mu.Lock()
		for _, id := range unflag {
			if _, ok := d.requests[id]; !ok {
				delete(d.flagged, id)
				delete(d.local, id)
			}
		}
		d.mu.Unlock()
	}

	now := time.Now()
	node := g.nodeID()
	type put struct {
		key string
		val []byte
	}
	var puts []put
	for _, it := range items {
		var reasons []string
		if it.known {
			reasons = g.coverageReasons(it.regID)
		}
		blocked := len(reasons) > 0
		var busy int32
		if it.known {
			g.mu.Lock()
			busy = g.setInstanceRoutingLocked(it.regID, !blocked)
			g.mu.Unlock()
		}

		d.mu.Lock()
		if _, still := d.requests[it.regID]; !still {
			d.mu.Unlock()
			continue // cancelled meanwhile; next tick unflags
		}
		ld := d.local[it.regID]
		if ld == nil || ld.ack.RequestedUnixNano != it.req.RequestedUnixNano {
			ld = &localDrain{}
			d.local[it.regID] = ld
		}
		if it.known {
			d.flagged[it.regID] = true
		}
		switch {
		case blocked:
			if !ld.stoppedSince.IsZero() {
				g.kickGate() // routing resumed: back in the ready set
			}
			ld.stoppedSince = time.Time{}
		case ld.stoppedSince.IsZero():
			ld.stoppedSince = now
		}
		ld.reasons = reasons
		ack := drainAck{
			NodeID:            node,
			RequestedUnixNano: it.req.RequestedUnixNano,
			Stopped:           !blocked && now.Sub(ld.stoppedSince) >= drainSettle,
			Blocked:           blocked,
			Inflight:          busy,
		}
		if kv != nil && (ack != ld.ack || now.Sub(ld.ackPutAt) >= drainAckRefresh) {
			if b, err := json.Marshal(ack); err == nil {
				puts = append(puts, put{key: drainAckKey(it.regID, node), val: b})
				ld.ackPutAt = now
			}
		}
		ld.ack = ack
		d.mu.Unlock()
	}

	if kv == nil {
		return
	}
	for _, p := range puts {
		kctx, cancel := kvCtx(ctx)
		_, _ = kv.Put(kctx, p.key, p.val)
		cancel()
	}
	for _, id := range gone {
		kctx, cancel := kvCtx(ctx)
		_ = kv.Delete(kctx, drainReqKey(id))
		_ = kv.Delete(kctx, drainAckKey(id, node))
		cancel()
	}
}

// startDrainLoop runs drainTickOnce every drainTick until Close.
func (g *Gateway) startDrainLoop() {
	g.drains.loopOnce.Do(func() {
		go func() {
			t := time.NewTicker(drainTick)
			defer t.Stop()
			for {
				select {
				case <-g.life.Done():
					return
				case <-t.C:
					g.drainTickOnce(g.life)
				}
			}
		}()
	})
}

func drainReqKey(regID string) string         { return "req." + regID }
func drainAckKey(regID, nodeID string) string { return "ack." + regID + "." + nodeID }

// requestInstanceDrain records (or with cancel, withdraws) a drain
// for regID, then evaluates it once so this node's routing changes
// before the call returns.
func (g *Gateway) requestInstanceDrain(ctx context.Context, regID string, cancel bool) error {
	d := &g.drains
	d.mu.Lock()
	d.initLocked()
	_, exists := d.requests[regID]
	kv := d.kv
	req := drainRequest{RegID: regID, RequestedUnixNano: time.Now().UnixNano()}
	switch {
	case cancel:
		delete(d.requests, regID)
	case !exists:
		d.requests[regID] = req
	}
	d.mu.Unlock()

	if kv != nil {
		kctx, kcancel := kvCtx(ctx)
		defer kcancel()
		if cancel {
			if err := kv.Delete(kctx, drainReqKey(regID)); err != nil {
				return fmt.Errorf("drain: kv delete: %w", err)
			}
		} else if !exists {
			b, _ := json.Marshal(req)
			if _, err := kv.Create(kctx, drainReqKey(regID), b); err != nil && !errors.Is(err, jetstream.ErrKeyExists) {
				return fmt.Errorf("drain: kv create: %w", err)
			}
			// A peer's earlier request may already be in the
			// bucket; adopt its time so every node agrees.
			if e, err := kv.Get(kctx, drainReqKey(regID)); err == nil {
				var cur drainRequest
				if json.Unmarshal(e.Value(), &cur) == nil {
					d.mu.Lock()
					if _, ok := d.requests[regID]; ok {
						d.requests[regID] = cur
					}
					d.mu.Unlock()
				}
			}
		}
	}
	g.startDrainLoop()
	g.drainTickOnce(ctx)
	return nil
}

// instanceDrainStatus is one instance's drain state as the admin API
// and the DrainInstance RPC report it.
type instanceDrainStatus struct {
	RegID      string
	InstanceID string
	Addr       string
	State      string
	Reasons    []string
	Inflight   int32
	Lines      []string
}

// instanceDrainStatusLocked builds the status from the last tick.
// Caller holds d.mu. live is the cluster's live peer set (nil
// standalone).
func (g *Gateway) instanceDrainStatusLocked(regID string, live []string) instanceDrainStatus {
	d := &g.drains
	st := instanceDrainStatus{RegID: regID, State: drainStateServing}
	if rec := d.regs[regID]; rec != nil {
		st.InstanceID = rec.instanceID
		st.Addr = rec.addr
		for k := range rec.lines {
			st.Lines = append(st.Lines, k.namespace+"/"+k.version)
		}
		sort.Strings(st.Lines)
	}
	req, requested := d.requests[regID]
	if !requested {
		return st
	}
	ld := d.local[regID]
	if ld == nil || ld.ack.RequestedUnixNano != req.RequestedUnixNano {
		st.State = drainStateDraining // not evaluated yet
		return st
	}
	if ld.ack.Blocked {
		st.State = drainStateBlocked
		st.Reasons = append([]string(nil), ld.reasons...)
		st.Inflight = ld.ack.Inflight
		return st
	}
	safe := ld.ack.Stopped && ld.ack.Inflight == 0
	st.Inflight = ld.ack.Inflight
	self := g.nodeID()
	for _, peer := range live {
		if peer == self {
			continue
		}
		a, ok := d.acks[regID][peer]
		if !ok || a.RequestedUnixNano != req.RequestedUnixNano {
			safe = false
			continue
		}
		st.Inflight += a.Inflight
		if a.Blocked {
			// A peer still sees a dependent uncovered (KV lag).
			st.State = drainStateBlocked
			st.Reasons = []string{"node " + peer + " still sees a dependent uncovered"}
			return st
		}
		if !a.Stopped || a.Inflight != 0 {
			safe = false
		}
	}
	if safe {
		st.State = drainStateSafeToStop
	} else {
		st.State = drainStateDraining
	}
	return st
}

func (g *Gateway) livePeers() []string {
	g.mu.Lock()
	t := g.peers
	g.mu.Unlock()
	if t == nil {
		return nil
	}
	return t.LivePeers()
}

// instanceDrainStatus evaluates and returns one instance's status.
func (g *Gateway) instanceDrainStatus(ctx context.Context, regID string) instanceDrainStatus {
	g.drainTickOnce(ctx)
	live := g.livePeers()
	d := &g.drains
	d.mu.Lock()
	defer d.mu.Unlock()
	return g.instanceDrainStatusLocked(regID, live)
}

// listInstanceDrains returns every known instance, sorted by
// registration ID. With line set ("ns/ver"), only instances serving
// it that have a drain requested.
func (g *Gateway) listInstanceDrains(line string) []instanceDrainStatus {
	live := g.livePeers()
	d := &g.drains
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initLocked()
	var out []instanceDrainStatus
	for id := range d.regs {
		if line != "" {
			if _, ok := d.requests[id]; !ok {
				continue
			}
		}
		st := g.instanceDrainStatusLocked(id, live)
		if line != "" && !containsString(st.Lines, line) {
			continue
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RegID < out[j].RegID })
	return out
}

// DrainInstance implements the control-plane RPC.
func (cp *controlPlane) DrainInstance(ctx context.Context, req *cpv1.DrainInstanceRequest) (*cpv1.DrainInstanceResponse, error) {
	st, err := cp.gw.drainInstance(ctx, req.GetInstance(), req.GetCancel())
	if err != nil {
		if errors.Is(err, errDrainUnknownInstance) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &cpv1.DrainInstanceResponse{
		RegistrationId: st.RegID,
		InstanceId:     st.InstanceID,
		State:          drainStateProto(st.State),
		Reasons:        st.Reasons,
		Inflight:       uint32(st.Inflight),
		Lines:          st.Lines,
	}, nil
}

// drainInstance resolves name, records the request (or cancel), and
// returns the fresh status.
func (g *Gateway) drainInstance(ctx context.Context, name string, cancel bool) (instanceDrainStatus, error) {
	regID, err := g.drains.resolveInstance(name)
	if err != nil {
		g.lifecycle.recordDrainRequest(cancel, false, err)
		return instanceDrainStatus{}, err
	}
	g.drains.mu.Lock()
	_, existed := g.drains.requests[regID]
	g.drains.mu.Unlock()
	err = g.requestInstanceDrain(ctx, regID, cancel)
	g.lifecycle.recordDrainRequest(cancel, existed, err)
	if err != nil {
		return instanceDrainStatus{}, err
	}
	return g.instanceDrainStatus(ctx, regID), nil
}

func drainStateProto(s string) cpv1.DrainState {
	switch s {
	case drainStateServing:
		return cpv1.DrainState_DRAIN_STATE_SERVING
	case drainStateDraining:
		return cpv1.DrainState_DRAIN_STATE_DRAINING
	case drainStateBlocked:
		return cpv1.DrainState_DRAIN_STATE_BLOCKED
	case drainStateSafeToStop:
		return cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP
	}
	return cpv1.DrainState_DRAIN_STATE_UNSPECIFIED
}

// openDrainBucket creates (or opens) the drain KV bucket. No TTL:
// requests live until cancelled or the instance goes.
func openDrainBucket(ctx context.Context, js jetstream.JetStream) (jetstream.KeyValue, error) {
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: drainBucketName, Replicas: 1})
	if err != nil && !errors.Is(err, jetstream.ErrBucketExists) {
		return nil, fmt.Errorf("drain bucket: %w", err)
	}
	if kv == nil {
		if kv, err = js.KeyValue(ctx, drainBucketName); err != nil {
			return nil, fmt.Errorf("drain bucket open: %w", err)
		}
	}
	return kv, nil
}

// drainWatchLoop mirrors the drain bucket into g.drains and runs the
// drain tick on this node.
func (t *peerTracker) drainWatchLoop(ctx context.Context) {
	defer close(t.drainDone)
	g := t.gw
	d := &g.drains
	d.mu.Lock()
	d.initLocked()
	d.kv = t.drain
	d.mu.Unlock()
	w, err := t.drain.WatchAll(ctx)
	if err != nil {
		g.cfg.cluster.Server.Warnf("gateway: drain watch start: %v", err)
		return
	}
	defer w.Stop()
	g.startDrainLoop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Updates():
			if !ok {
				return
			}
			if ev == nil {
				continue
			}
			g.observeDrainKV(ctx, ev)
		}
	}
}

func (g *Gateway) observeDrainKV(ctx context.Context, ev jetstream.KeyValueEntry) {
	d := &g.drains
	parts := strings.Split(ev.Key(), ".")
	del := ev.Operation() == jetstream.KeyValueDelete || ev.Operation() == jetstream.KeyValuePurge
	switch {
	case len(parts) == 2 && parts[0] == "req":
		regID := parts[1]
		d.mu.Lock()
		if del {
			delete(d.requests, regID)
			kv := d.kv
			d.mu.Unlock()
			if kv != nil {
				kctx, cancel := kvCtx(ctx)
				_ = kv.Delete(kctx, drainAckKey(regID, g.nodeID()))
				cancel()
			}
			return
		}
		var r drainRequest
		if err := json.Unmarshal(ev.Value(), &r); err == nil {
			d.requests[regID] = r
		}
		d.mu.Unlock()
	case len(parts) == 3 && parts[0] == "ack":
		regID, node := parts[1], parts[2]
		d.mu.Lock()
		defer d.mu.Unlock()
		if del {
			delete(d.acks[regID], node)
			if len(d.acks[regID]) == 0 {
				delete(d.acks, regID)
			}
			return
		}
		var a drainAck
		if err := json.Unmarshal(ev.Value(), &a); err != nil {
			return
		}
		if d.acks[regID] == nil {
			d.acks[regID] = map[string]drainAck{}
		}
		d.acks[regID][node] = a
	}
}
