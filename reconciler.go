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

	cancel context.CancelFunc
	done   chan struct{}
}

type reconcilerConn struct {
	conn *grpc.ClientConn
	refs int
}

func (g *Gateway) startReconciler(ctx context.Context, kv jetstream.KeyValue) (*reconciler, error) {
	rctx, cancel := context.WithCancel(ctx)
	r := &reconciler{
		gw:     g,
		kv:     kv,
		conns:  map[string]*reconcilerConn{},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go r.watchLoop(rctx)
	return r, nil
}

func (r *reconciler) stop() {
	if r == nil {
		return
	}
	r.cancel()
	<-r.done
	r.mu.Lock()
	for _, c := range r.conns {
		_ = c.conn.Close()
	}
	r.conns = nil
	r.mu.Unlock()
}

// watchLoop subscribes to all pool.> keys. KV replays the current
// state (one Put per existing key) before delivering new events, so
// boot-time consumers get the full picture.
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
		for ev := range w.Updates() {
			if ev == nil {
				continue
			}
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
	}
}

// handlePut is idempotent: if the replica is already in the pool
// (matched by id), nothing to do. Otherwise, ensure the pool exists,
// dial the addr (refcount bump), and add.
//
// OpenAPI-tagged values take a separate path: the gateway's openAPI
// source map is keyed by namespace only (single source per ns in v1),
// so we just call addOpenAPISourceLocked which is idempotent under
// hash equality.
func (r *reconciler) handlePut(ctx context.Context, ns, ver, replicaID string, raw []byte) {
	var v registryValue
	if err := json.Unmarshal(raw, &v); err != nil {
		r.gw.cfg.cluster.Server.Warnf("reconciler: bad value at %s: %v", replicaKey(ns, ver, replicaID), err)
		return
	}

	g := r.gw

	if v.IsOpenAPI() {
		var hash [32]byte
		copy(hash[:], v.Hash)
		g.mu.Lock()
		err := g.addOpenAPISourceLocked(ns, v.Addr, v.OpenAPISpec, hash, v.RegID, replicaID)
		g.mu.Unlock()
		if err != nil {
			g.cfg.cluster.Server.Warnf("reconciler: openapi %s: %v", ns, err)
		}
		return
	}

	if v.IsGraphQL() {
		var hash [32]byte
		copy(hash[:], v.Hash)
		g.mu.Lock()
		err := g.addGraphQLSourceLocked(ns, v.GraphQLEndpoint, v.GraphQLIntrospection, hash, v.RegID)
		g.mu.Unlock()
		if err != nil {
			g.cfg.cluster.Server.Warnf("reconciler: graphql %s: %v", ns, err)
		}
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if p, ok := g.pools[poolKey{namespace: ns, version: ver}]; ok {
		if existing := p.findReplicaByID(replicaID); existing != nil {
			return // already known
		}
	}

	fd, err := parseFileDescriptorSet(v.FileDescriptorSet, v.FileName)
	if err != nil {
		g.cfg.cluster.Server.Warnf("reconciler: parse fd for %s: %v", replicaKey(ns, ver, replicaID), err)
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
		namespace: ns,
		version:   ver,
		hash:      hash,
		file:      fd,
		addr:      v.Addr,
		conn:      conn,
		owner:     v.RegID,
		replicaID: replicaID,
	}); err != nil {
		r.releaseConn(v.Addr)
		g.cfg.cluster.Server.Warnf("reconciler: join pool %s/%s: %v", ns, ver, err)
		return
	}
}

func (r *reconciler) handleDelete(ns, ver, replicaID string) {
	g := r.gw
	g.mu.Lock()
	// Try the OpenAPI side first — sources are keyed by namespace.
	// If we find one, drop just this replica; the source dies when
	// its last replica leaves.
	if _, isOpenAPI := g.openAPISources[ns]; isOpenAPI {
		g.removeOpenAPIReplicaByIDLocked(ns, replicaID)
		g.mu.Unlock()
		return
	}
	// GraphQL sources are also single-source-per-ns; one delete
	// drops the whole entry.
	if _, isGraphQL := g.graphQLSources[ns]; isGraphQL {
		g.removeGraphQLSourceLocked(ns)
		g.mu.Unlock()
		return
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

func (r *reconciler) acquireConn(addr string) (grpc.ClientConnInterface, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.conns[addr]; ok {
		c.refs++
		return c.conn, nil
	}
	var creds grpc.DialOption
	if t := r.gw.cfg.tls; t != nil {
		creds = grpc.WithTransportCredentials(credentials.NewTLS(t))
	} else {
		creds = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	conn, err := grpc.NewClient(addr, creds)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	r.conns[addr] = &reconcilerConn{conn: conn, refs: 1}
	return conn, nil
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
		_ = c.conn.Close()
		delete(r.conns, addr)
	}
}
