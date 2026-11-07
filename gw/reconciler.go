package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// reconciler watches the registry KV bucket and keeps the local
// pools/replicas state in sync with cluster-wide truth. Every gateway
// runs one. Conn pool is reconciler-owned: every gateway dials every
// service it sees in KV.
type reconciler struct {
	gw *Gateway
	kv jetstream.KeyValue

	mu    sync.Mutex
	conns map[string]*reconcilerConn

	cancel    context.CancelFunc
	done      chan struct{}
	sweepDone chan struct{}
}

type reconcilerConn struct {
	// pool wraps N grpc.ClientConn instances to the same upstream
	// address; round-robin spreads stream creation + transport-mutex
	// pressure beyond what a single conn can absorb at high RPS
	// (HTTP/2 caps streams per conn at 100 by default). Sized at
	// dial time by cfg.grpcConnPoolSize via WithGRPCConnPoolSize.
	pool *connPool
	refs int
}

func (g *Gateway) startReconciler(ctx context.Context, kv jetstream.KeyValue, ttl time.Duration) (*reconciler, error) {
	rctx, cancel := context.WithCancel(ctx)
	r := &reconciler{
		gw:        g,
		kv:        kv,
		conns:     map[string]*reconcilerConn{},
		cancel:    cancel,
		done:      make(chan struct{}),
		sweepDone: make(chan struct{}),
	}
	go r.watchLoop(rctx)
	go r.sweepLoop(rctx, ttl)
	return r, nil
}

func (r *reconciler) stop() {
	if r == nil {
		return
	}
	r.cancel()
	<-r.done
	<-r.sweepDone
	r.mu.Lock()
	for _, c := range r.conns {
		_ = c.pool.Close()
	}
	r.conns = nil
	r.mu.Unlock()
}

// watchLoop subscribes to all pool.> keys. KV replays the current
// state (one Put per existing key) before delivering new events, so
// boot-time consumers get the full picture. The nil entry that ends
// the replay marks the node ready for /health, once replayComplete
// agrees the replay held every key.
func (r *reconciler) watchLoop(ctx context.Context) {
	defer close(r.done)
	for {
		w, err := r.kv.WatchAll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.gw.cfg.cluster.Server.Warnf("reconciler: watch: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		replayed := 0 // entries before the nil marker
		short := false
		for ev := range w.Updates() {
			if ev == nil {
				// End of the initial values. handlePut applies each
				// value and rebuilds the schema before it returns, so
				// every replayed key is routable now. Later watches
				// (after a reconnect) also send nil; once synced the
				// node stays synced.
				if r.gw.registrySync.Load() == registrySyncDone {
					continue
				}
				if !r.replayComplete(ctx, replayed) {
					short = true
					break
				}
				r.gw.markRegistrySynced()
				continue
			}
			replayed++
			ns, ver, rid, ok := parseReplicaKey(ev.Key())
			if !ok {
				continue
			}
			switch ev.Operation() {
			case jetstream.KeyValuePut:
				r.handlePut(ctx, ns, ver, rid, ev.Value())
			case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
				r.handleDelete(ns, ver, rid)
			}
		}
		_ = w.Stop()
		if ctx.Err() != nil {
			return
		}
		if short {
			// Watch again: the next replay is checked the same way.
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
}

// replayComplete checks a watch's initial replay against the bucket's
// stream state, which the stream leader reports. The nil marker alone
// is not enough: a watch created while a node rejoins the cluster can
// report 0 initial values for a stream that holds keys (seen in the
// e2e S4 restart loop), and the node would then turn ready without
// its services. The replay delivers the last entry of every key,
// deletes included, so it must hold at least NumSubjects entries.
// A key added after the watch started makes the check fail once more
// and costs one more replay.
func (r *reconciler) replayComplete(ctx context.Context, replayed int) bool {
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := r.kv.Status(sctx)
	if err != nil {
		r.gw.cfg.cluster.Server.Warnf("reconciler: registry status after replay: %v", err)
		return false
	}
	bs, ok := st.(*jetstream.KeyValueBucketStatus)
	if !ok {
		return true // cannot check; trust the marker
	}
	want := bs.StreamInfo().State.NumSubjects
	if uint64(replayed) >= want {
		return true
	}
	r.gw.cfg.cluster.Server.Warnf("reconciler: registry replay delivered %d of %d keys; watching again", replayed, want)
	return false
}

// handlePut is idempotent: if the replica is already in the pool
// (matched by id), nothing to do. Otherwise, ensure the pool exists,
// dial the addr (refcount bump), and add.
//
// OpenAPI / GraphQL bindings take a separate path: their source maps
// are keyed by (namespace, version) and addXxxSourceLocked is
// idempotent under hash equality.
func (r *reconciler) handlePut(ctx context.Context, ns, ver, replicaID string, raw []byte) {
	var v registryValue
	if err := json.Unmarshal(raw, &v); err != nil {
		r.gw.cfg.cluster.Server.Warnf("reconciler: bad value at %s: %v", replicaKey(ns, ver, replicaID), err)
		return
	}

	g := r.gw

	// Start gate: every value is recorded (admin listing, cycle
	// check, cross-node readiness); a waiting one is not joined. Any
	// change can open the gate for a waiter, so kick the gate loop.
	defer g.kickGate()
	var hash0 [32]byte
	copy(hash0[:], v.Hash)
	g.mu.Lock()
	waiting := g.setGateInstanceLocked(replicaKey(ns, ver, replicaID), &gateInstance{
		key:        poolKey{namespace: ns, version: ver},
		hash:       hash0,
		regID:      v.RegID,
		replicaID:  replicaID,
		addr:       v.Addr,
		instanceID: v.InstanceID,
		manifest:   v.serviceManifest,
		waiting:    v.Waiting,
	})
	g.mu.Unlock()
	if waiting {
		return
	}

	if v.IsOpenAPI() {
		var hash [32]byte
		copy(hash[:], v.Hash)
		g.mu.Lock()
		err := g.addOpenAPISourceLocked(ns, ver, v.Addr, v.OpenAPISpec, hash, v.RegID, replicaID, int(v.MaxConcurrency), int(v.MaxConcurrencyPerInstance), msDuration(v.TimeoutMs))
		if err == nil {
			g.setSlotManifestLocked(poolKey{namespace: ns, version: ver}, v.serviceManifest)
			g.noteInstanceLine(v.RegID, v.InstanceID, v.Addr, ns, ver, replicaID, v.serviceManifest)
		}
		g.mu.Unlock()
		if err != nil {
			g.cfg.cluster.Server.Warnf("reconciler: openapi %s/%s: %v", ns, ver, err)
		}
		return
	}

	if v.IsGraphQL() {
		var hash [32]byte
		copy(hash[:], v.Hash)
		g.mu.Lock()
		err := g.addGraphQLSourceLocked(ns, ver, v.GraphQLEndpoint, v.GraphQLIntrospection, hash, v.RegID, replicaID, msDuration(v.TimeoutMs))
		if err == nil {
			g.setSlotManifestLocked(poolKey{namespace: ns, version: ver}, v.serviceManifest)
			g.noteInstanceLine(v.RegID, v.InstanceID, v.GraphQLEndpoint, ns, ver, replicaID, v.serviceManifest)
		}
		g.mu.Unlock()
		if err != nil {
			g.cfg.cluster.Server.Warnf("reconciler: graphql %s/%s: %v", ns, ver, err)
		}
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if p := g.protoSlot(poolKey{namespace: ns, version: ver}); p != nil {
		if existing := p.findReplicaByID(replicaID); existing != nil {
			g.setSlotManifestLocked(poolKey{namespace: ns, version: ver}, v.serviceManifest)
			g.noteInstanceLine(v.RegID, v.InstanceID, v.Addr, ns, ver, replicaID, v.serviceManifest)
			return // already known
		}
	}

	entry := ns + ".proto"
	fd, err := compileProtoBytes(entry, v.ProtoSource, v.ProtoImports)
	if err != nil {
		g.cfg.cluster.Server.Warnf("reconciler: compile proto for %s: %v", replicaKey(ns, ver, replicaID), err)
		return
	}

	conn, err := r.acquireConn(v.Addr)
	if err != nil {
		g.cfg.cluster.Server.Warnf("reconciler: dial %s: %v", v.Addr, err)
		return
	}

	var hash [32]byte
	copy(hash[:], v.Hash)

	if err := g.joinPoolLocked(poolEntry{
		namespace:                 ns,
		version:                   ver,
		hash:                      hash,
		file:                      fd,
		addr:                      v.Addr,
		conn:                      conn,
		owner:                     v.RegID,
		replicaID:                 replicaID,
		maxConcurrency:            int(v.MaxConcurrency),
		maxConcurrencyPerInstance: int(v.MaxConcurrencyPerInstance),
		timeout:                   msDuration(v.TimeoutMs),
	}); err != nil {
		r.releaseConn(v.Addr)
		g.cfg.cluster.Server.Warnf("reconciler: join pool %s/%s: %v", ns, ver, err)
		return
	}
	g.setSlotManifestLocked(poolKey{namespace: ns, version: ver}, v.serviceManifest)
	g.noteInstanceLine(v.RegID, v.InstanceID, v.Addr, ns, ver, replicaID, v.serviceManifest)
}

func (r *reconciler) handleDelete(ns, ver, replicaID string) {
	g := r.gw
	defer g.kickGate()
	g.forgetInstanceReplica(ns, ver, replicaID)
	g.mu.Lock()
	if g.removeGateInstanceLocked(replicaKey(ns, ver, replicaID)) {
		// Was waiting: never joined, nothing to remove.
		g.mu.Unlock()
		return
	}
	key := poolKey{namespace: ns, version: ver}
	// Slot kind decides which removal path applies — the slot is the
	// source of truth, so a single switch replaces the previous "look
	// in three maps in order" pattern.
	if s := g.slots[key]; s != nil {
		switch s.kind {
		case slotKindOpenAPI:
			g.removeOpenAPIReplicaByIDLocked(ns, ver, replicaID)
			g.mu.Unlock()
			return
		case slotKindGraphQL:
			g.removeGraphQLReplicaByIDLocked(ns, ver, replicaID)
			g.mu.Unlock()
			return
		}
	}
	rep, err := g.removeReplicaByIDLocked(ns, ver, replicaID)
	g.mu.Unlock()
	if err != nil {
		g.cfg.cluster.Server.Warnf("reconciler: remove %s/%s/%s: %v", ns, ver, replicaID, err)
	}
	if rep != nil {
		r.releaseConn(rep.addr)
	}
}

// sweepLoop is the backstop for a delete the watch can no longer
// deliver. An expired entry leaves a delete marker
// (openExpiringBucket), but the marker itself expires one TTL later.
// A watch stalled past that (its consumer lived on a node that died,
// or this node sat in a partition's minority) resumes after the
// marker is gone and would keep the entry for good.
//
// Every ttl/2 the sweep looks up each registry key this node holds.
// A key missing for at least ttl is dropped as if its delete had
// arrived. Not sooner: until the marker expires a stalled watch can
// still deliver it, in order after any Put written before it (a
// client's failover registration), and dropping first would open a
// gap on this node. The lookup goes to the stream leader (kvKeyGone);
// no leader, no drop.
func (r *reconciler) sweepLoop(ctx context.Context, ttl time.Duration) {
	defer close(r.sweepDone)
	tk := time.NewTicker(ttl / 2)
	defer tk.Stop()
	missing := map[string]time.Time{} // key → first lookup that missed
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		now := time.Now()
		next := map[string]time.Time{}
		for _, key := range r.gw.registryInstanceKeys() {
			gctx, cancel := kvCtx(ctx)
			gone, err := kvKeyGone(gctx, r.gw.cfg.cluster.Conn, registryBucketName, key)
			cancel()
			if err != nil || !gone {
				continue // present, or the leader did not answer: keep it
			}
			first, ok := missing[key]
			if !ok {
				first = now
			}
			if now.Sub(first) < ttl {
				next[key] = first
				continue
			}
			ns, ver, rid, ok := parseReplicaKey(key)
			if !ok {
				continue
			}
			r.gw.cfg.cluster.Server.Warnf("reconciler: %s is gone from the registry without a delete event; dropping it", key)
			r.handleDelete(ns, ver, rid)
		}
		missing = next
	}
}

// registryInstanceKeys lists the registry keys of the instances this
// node applied from the registry KV.
func (g *Gateway) registryInstanceKeys() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for id, inst := range g.gate.instances {
		if inst.replicaID != "" {
			out = append(out, id)
		}
	}
	return out
}

func (r *reconciler) acquireConn(addr string) (grpc.ClientConnInterface, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.conns[addr]; ok {
		c.refs++
		return c.pool, nil
	}
	var creds grpc.DialOption
	if t := r.gw.cfg.tls; t != nil {
		creds = grpc.WithTransportCredentials(credentials.NewTLS(t))
	} else {
		creds = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	size := r.gw.cfg.grpcConnPoolSize
	if size <= 0 {
		size = defaultGRPCConnPoolSize
	}
	pool, err := dialConnPool(addr, size, creds)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	r.conns[addr] = &reconcilerConn{pool: pool, refs: 1}
	return pool, nil
}

func (r *reconciler) releaseConn(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conns[addr]
	if !ok {
		return
	}
	c.refs--
	if c.refs <= 0 {
		_ = c.pool.Close()
		delete(r.conns, addr)
	}
}
