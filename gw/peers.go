package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	peersBucketName      = "gwag-peers"
	registryBucketName   = "gwag-registry"
	stableBucketName     = "gwag-stable"
	deprecatedBucketName = "gwag-deprecated"

	peerTTL = 30 * time.Second
	// peerTTLPerRefresh: a node refreshes its peer entry every
	// peerTTL/3 (10s by default).
	peerTTLPerRefresh = 3

	registryTTL = 30 * time.Second

	// stableTTL / deprecatedTTL are 0 — both states are operator-set
	// and must survive any deregister/restart cycle. Stable is
	// monotonic (advances at registration, decreases only via
	// RetractStable per plan §4); deprecated is operator-toggled per
	// (ns, ver) per plan §5. The buckets are the only way a fresh
	// node joining mid-life recovers state when the relevant
	// replicas are temporarily absent.
	stableTTL     = 0
	deprecatedTTL = 0

	maxReplicas = 3
)

// peerEntry is the value stored under each peer's NodeID in the peers
// KV bucket. Joined is an instant marker for ForgetPeer to compare
// against the bucket's TTL — anything older than now-TTL is considered
// disconnected (the bucket auto-expires the entry; if it's still
// present, it's been refreshed at least once within TTL).
type peerEntry struct {
	NodeID  string `json:"node_id"`
	Name    string `json:"name,omitempty"`
	JoinedM int64  `json:"joined_unix_ms"`
}

// peerTracker is created on the first Gateway.startClusterTracking
// call. It owns the refresh + watch goroutines and the live peer set.
type peerTracker struct {
	gw         *Gateway
	js         jetstream.JetStream
	peers      jetstream.KeyValue
	reg        jetstream.KeyValue
	stable     jetstream.KeyValue
	deprecated jetstream.KeyValue
	mcpConfig  jetstream.KeyValue
	drain      jetstream.KeyValue
	externals  jetstream.KeyValue
	// notifications holds the hook-failure notification items
	// (notification_store.go).
	notifications jetstream.KeyValue
	// buckets is every managed bucket: replicaBuckets, plus
	// gwag-schemas with WithSchemaRegistry.
	buckets []string

	nodeID string
	self   []byte

	// peerTTL / registryTTL are the bucket TTLs (clusterTTLs).
	peerTTL     time.Duration
	registryTTL time.Duration

	mu   sync.Mutex
	live map[string]struct{} // active peer NodeIDs (incl. self)

	cancel context.CancelFunc
	done   chan struct{}

	stableDone        chan struct{}
	deprecatedDone    chan struct{}
	mcpConfigDone     chan struct{}
	drainDone         chan struct{}
	externalsDone     chan struct{}
	notificationsDone chan struct{}

	// currentR is the managed buckets' replica count as last read
	// from JetStream or applied by this node. replMu serialises the
	// read-compare-write of a raise (reconcileReplicas) and a shrink
	// (ForgetPeer).
	currentR atomic.Int32
	replMu   sync.Mutex
	// reconciling is set while a kickReconcile goroutine runs.
	reconciling atomic.Bool

	rec *reconciler
}

// startClusterTracking is idempotent — only the first invocation starts
// the goroutines. Subsequent calls return the same tracker.
func (g *Gateway) startClusterTracking(ctx context.Context) (*peerTracker, error) {
	if g.cfg.cluster == nil {
		return nil, errors.New("gateway: cluster is not configured")
	}
	g.armRegistrySync()
	g.mu.Lock()
	if g.peers != nil {
		t := g.peers
		g.mu.Unlock()
		return t, nil
	}
	cl := g.cfg.cluster

	// Both buckets are created at R=1 if they don't exist; live R is
	// raised monotonically by reconcileReplicas as peers join.
	// Peers and registry entries expire when their writer stops
	// refreshing them. Both buckets leave a delete marker on expiry
	// so every watcher sees the key go (openExpiringBucket).
	peerTTL, registryTTL := g.clusterTTLs()
	peers, err := openExpiringBucket(ctx, cl.JS, peersBucketName, peerTTL)
	if err != nil {
		g.mu.Unlock()
		return nil, err
	}
	reg, err := openExpiringBucket(ctx, cl.JS, registryBucketName, registryTTL)
	if err != nil {
		g.mu.Unlock()
		return nil, err
	}

	stable, err := cl.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   stableBucketName,
		Replicas: 1,
		TTL:      stableTTL,
	})
	if err != nil && !errors.Is(err, jetstream.ErrBucketExists) {
		g.mu.Unlock()
		return nil, fmt.Errorf("stable bucket: %w", err)
	}
	if stable == nil {
		stable, err = cl.JS.KeyValue(ctx, stableBucketName)
		if err != nil {
			g.mu.Unlock()
			return nil, fmt.Errorf("stable bucket open: %w", err)
		}
	}

	deprecated, err := cl.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   deprecatedBucketName,
		Replicas: 1,
		TTL:      deprecatedTTL,
	})
	if err != nil && !errors.Is(err, jetstream.ErrBucketExists) {
		g.mu.Unlock()
		return nil, fmt.Errorf("deprecated bucket: %w", err)
	}
	if deprecated == nil {
		deprecated, err = cl.JS.KeyValue(ctx, deprecatedBucketName)
		if err != nil {
			g.mu.Unlock()
			return nil, fmt.Errorf("deprecated bucket open: %w", err)
		}
	}

	mcpConfig, err := cl.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   mcpConfigBucketName,
		Replicas: 1,
		TTL:      mcpConfigTTL,
	})
	if err != nil && !errors.Is(err, jetstream.ErrBucketExists) {
		g.mu.Unlock()
		return nil, fmt.Errorf("mcp_config bucket: %w", err)
	}
	if mcpConfig == nil {
		mcpConfig, err = cl.JS.KeyValue(ctx, mcpConfigBucketName)
		if err != nil {
			g.mu.Unlock()
			return nil, fmt.Errorf("mcp_config bucket open: %w", err)
		}
	}

	drain, err := openDrainBucket(ctx, cl.JS)
	if err != nil {
		g.mu.Unlock()
		return nil, err
	}

	externals, err := openExternalDependentsBucket(ctx, cl.JS)
	if err != nil {
		g.mu.Unlock()
		return nil, err
	}

	notifications, err := openNotificationsBucket(ctx, cl.JS)
	if err != nil {
		g.mu.Unlock()
		return nil, err
	}

	buckets := replicaBuckets
	var schemas jetstream.KeyValue
	if g.sr != nil {
		if schemas, err = openSchemasBucket(ctx, cl.JS); err != nil {
			g.mu.Unlock()
			return nil, err
		}
		buckets = append(append([]string(nil), replicaBuckets...), schemasBucketName)
	}

	selfBytes, err := json.Marshal(peerEntry{
		NodeID:  cl.NodeID,
		Name:    cl.Server.Name(),
		JoinedM: time.Now().UnixMilli(),
	})
	if err != nil {
		g.mu.Unlock()
		return nil, fmt.Errorf("marshal self: %w", err)
	}

	tctx, cancel := context.WithCancel(ctx)
	t := &peerTracker{
		gw:                g,
		js:                cl.JS,
		peers:             peers,
		reg:               reg,
		stable:            stable,
		deprecated:        deprecated,
		mcpConfig:         mcpConfig,
		drain:             drain,
		externals:         externals,
		notifications:     notifications,
		buckets:           buckets,
		nodeID:            cl.NodeID,
		self:              selfBytes,
		peerTTL:           peerTTL,
		registryTTL:       registryTTL,
		live:              map[string]struct{}{cl.NodeID: {}},
		cancel:            cancel,
		done:              make(chan struct{}),
		stableDone:        make(chan struct{}),
		deprecatedDone:    make(chan struct{}),
		mcpConfigDone:     make(chan struct{}),
		drainDone:         make(chan struct{}),
		externalsDone:     make(chan struct{}),
		notificationsDone: make(chan struct{}),
	}
	t.currentR.Store(1)
	g.peers = t
	g.mu.Unlock()

	// fail unpublishes t and closes the done channels its loops would
	// have closed. A Close that took t off g.peers while this start was
	// in flight is already waiting in t.stop; without the closes it
	// blocks forever. Safe: fail runs at most once, before any loop starts.
	fail := func(err error) (*peerTracker, error) {
		cancel()
		g.mu.Lock()
		if g.peers == t {
			g.peers = nil
		}
		g.mu.Unlock()
		close(t.done)
		close(t.stableDone)
		close(t.deprecatedDone)
		close(t.mcpConfigDone)
		close(t.drainDone)
		close(t.externalsDone)
		close(t.notificationsDone)
		return nil, err
	}

	// Initial put of self before launching loops, so reconcile sees us.
	// tctx, not ctx: a Close during this Put cancels it via t.stop.
	if _, err := peers.Put(tctx, cl.NodeID, selfBytes); err != nil {
		return fail(fmt.Errorf("put self: %w", err))
	}

	rec, err := g.startReconciler(tctx, reg, registryTTL)
	if err != nil {
		return fail(fmt.Errorf("start reconciler: %w", err))
	}
	t.rec = rec

	go t.refreshLoop(tctx)
	go t.watchLoop(tctx)
	go t.stableWatchLoop(tctx)
	go t.deprecatedWatchLoop(tctx)
	go t.mcpConfigWatchLoop(tctx)
	go t.drainWatchLoop(tctx)
	go t.externalsWatchLoop(tctx)
	go t.notificationsWatchLoop(tctx)
	if schemas != nil {
		g.sr.bindCluster(schemas)
	}
	return t, nil
}

func (t *peerTracker) refreshLoop(ctx context.Context) {
	tk := time.NewTicker(t.peerTTL / peerTTLPerRefresh)
	defer tk.Stop()
	missing := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, _ = t.peers.Put(c, t.nodeID, t.self)
			cancel()
			missing = t.sweepPeers(ctx, missing)
			t.kickReconcile(ctx)
		}
	}
}

// kickReconcile runs reconcileReplicas off the refresh loop, so a
// slow raise never delays the peer refresh. Skipped while one is
// running. It retries a raise that failed and picks up a replica
// change another node made.
func (t *peerTracker) kickReconcile(ctx context.Context) {
	if !t.reconciling.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer t.reconciling.Store(false)
		t.reconcileReplicas(ctx)
	}()
}

// sweepPeers is the backstop for a peer delete the watch can no
// longer deliver, by the rule of reconciler.sweepLoop: a live peer
// whose key has been missing for at least peerTTL leaves the live
// set. missing maps a peer to the first sweep that missed it; the
// result is the map for the next sweep.
func (t *peerTracker) sweepPeers(ctx context.Context, missing map[string]time.Time) map[string]time.Time {
	now := time.Now()
	next := map[string]time.Time{}
	for _, id := range t.LivePeers() {
		if id == t.nodeID {
			continue
		}
		c, cancel := kvCtx(ctx)
		gone, err := kvKeyGone(c, t.gw.cfg.cluster.Conn, peersBucketName, id)
		cancel()
		if err != nil || !gone {
			continue
		}
		first, ok := missing[id]
		if !ok {
			first = now
		}
		if now.Sub(first) < t.peerTTL {
			next[id] = first
			continue
		}
		t.mu.Lock()
		delete(t.live, id)
		t.mu.Unlock()
	}
	return next
}

// watchLoop tracks the live peer set and triggers a replica reconcile
// on every change. KV TTL handles departure: a peer that stops
// refreshing has its key expired, and the delete marker the bucket
// leaves (openExpiringBucket) arrives here as a Purge.
func (t *peerTracker) watchLoop(ctx context.Context) {
	defer close(t.done)
	for {
		w, err := t.peers.WatchAll(ctx, jetstream.IncludeHistory())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(time.Second)
			continue
		}
		for ev := range w.Updates() {
			if ev == nil {
				// initial-state-replayed marker
				t.reconcileReplicas(ctx)
				continue
			}
			t.mu.Lock()
			switch ev.Operation() {
			case jetstream.KeyValuePut:
				t.live[ev.Key()] = struct{}{}
			case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
				delete(t.live, ev.Key())
			}
			t.mu.Unlock()
			t.reconcileReplicas(ctx)
		}
		_ = w.Stop()
		if ctx.Err() != nil {
			return
		}
	}
}

// replicaBuckets is every managed KV bucket, by bucket name.
var replicaBuckets = []string{
	peersBucketName,
	registryBucketName,
	stableBucketName,
	deprecatedBucketName,
	mcpConfigBucketName,
	drainBucketName,
	externalDependentsBucketName,
	notificationsBucketName,
}

// reconcileReplicas raises every managed bucket to the replica count
// the cluster policy (WithClusterReplicas) gives for the live peers.
// Strictly monotonic — never lowers; ForgetPeer is the only path that
// shrinks (a peer is only dropped from the live set after its entry
// expired, so a brief loss never shrinks). Idempotent if already at
// target. currentR is first set from the buckets' actual replica
// count, so a raise another node applied (or one whose answer this
// node never got) is not sent again.
func (t *peerTracker) reconcileReplicas(ctx context.Context) {
	t.replMu.Lock()
	defer t.replMu.Unlock()
	t.mu.Lock()
	desired := t.gw.cfg.clusterReplicas.For(len(t.live))
	t.mu.Unlock()
	cur := t.syncReplicasLocked(ctx)
	if desired <= cur {
		return
	}
	if err := t.setReplicas(ctx, desired); err != nil {
		// Best-effort. The next peer change or refresh tick retries.
		return
	}
	t.currentR.Store(int32(desired))
}

// syncReplicasLocked sets currentR to the lowest replica count of the
// managed buckets as JetStream reports it, and returns it. On a read
// error currentR is left as it is. Caller holds replMu.
func (t *peerTracker) syncReplicasLocked(ctx context.Context) int {
	if r, err := t.appliedReplicas(ctx); err == nil {
		t.currentR.Store(int32(r))
	}
	return int(t.currentR.Load())
}

// appliedReplicas is the lowest configured replica count of the
// managed buckets.
func (t *peerTracker) appliedReplicas(ctx context.Context) (int, error) {
	low := 0
	for _, b := range t.buckets {
		cfg, err := readStreamConfig(ctx, t.js, "KV_"+b)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", b, err)
		}
		if cfg == nil {
			return 0, fmt.Errorf("%s: %w", b, errStreamMissing)
		}
		if low == 0 || cfg.Replicas < low {
			low = cfg.Replicas
		}
	}
	return low, nil
}

// setReplicas sets the replica count of every cluster bucket and
// event stream (applyStream: no write when already there, one writer
// per change, a lost answer verified by reading). If one fails we
// still try the others; caller decides whether to retry. Only the
// replica count changes; the rest of each bucket's config stays.
func (t *peerTracker) setReplicas(ctx context.Context, r int) error {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var firstErr error
	for _, b := range t.buckets {
		_, err := applyStream(c, t.js, "KV_"+b, t.gw.isJetStreamLeader, func(cur *jetstream.StreamConfig) (jetstream.StreamConfig, bool) {
			if cur == nil {
				return jetstream.StreamConfig{}, false
			}
			want := *cur
			want.Replicas = r
			return want, true
		})
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", b, err)
		}
	}
	// Named-channel event streams follow the same policy.
	if err := t.gw.setEventStreamReplicas(c, t.js, r); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// clusterTTLs returns the peers and registry bucket TTLs.
func (g *Gateway) clusterTTLs() (peer, registry time.Duration) {
	if d := g.cfg.clusterKVTTL; d > 0 {
		return d, d
	}
	return peerTTL, registryTTL
}

// openExpiringBucket opens (creating it at R=1 if needed) a KV bucket
// whose entries expire ttl after their last Put, with subject delete
// markers on. JetStream removes an expired message without a delete
// event; the marker it writes instead is what tells every watcher
// (reconciler, peer tracker) that the key is gone. Without it a node
// that was watching keeps the key forever, while a node that replays
// the bucket after the expiry never sees it.
//
// A bucket created by an older gateway has no markers. Its stream
// config is updated in place with markers on and nothing else
// changed (replicas, limits); the server allows turning markers on,
// never off.
func openExpiringBucket(ctx context.Context, js jetstream.JetStream, name string, ttl time.Duration) (jetstream.KeyValue, error) {
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   name,
		Replicas: 1,
		TTL:      ttl,
		// A marker expires with the bucket TTL (MaxAge) whatever
		// its own TTL; ask for the same.
		LimitMarkerTTL: ttl,
	})
	if err == nil {
		return kv, nil
	}
	if !errors.Is(err, jetstream.ErrBucketExists) {
		return nil, fmt.Errorf("%s bucket: %w", name, err)
	}
	stream, err := js.Stream(ctx, "KV_"+name)
	if err != nil {
		return nil, fmt.Errorf("%s bucket open: %w", name, err)
	}
	if cfg := stream.CachedInfo().Config; cfg.SubjectDeleteMarkerTTL <= 0 {
		cfg.AllowMsgTTL = true
		cfg.SubjectDeleteMarkerTTL = cfg.MaxAge
		if cfg.SubjectDeleteMarkerTTL < time.Second {
			cfg.SubjectDeleteMarkerTTL = ttl
		}
		if _, err := js.UpdateStream(ctx, cfg); err != nil {
			return nil, fmt.Errorf("%s bucket: turn on delete markers: %w", name, err)
		}
	}
	kv, err = js.KeyValue(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("%s bucket open: %w", name, err)
	}
	return kv, nil
}

// registryKV returns the registry bucket if cluster tracking is up, or
// nil if the gateway is running standalone or tracking hasn't booted.
func (g *Gateway) registryKV() jetstream.KeyValue {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.peers == nil {
		return nil
	}
	return g.peers.reg
}

// nodeID returns the NATS server's ID, or empty string when standalone.
func (g *Gateway) nodeID() string {
	if g.cfg.cluster == nil {
		return ""
	}
	return g.cfg.cluster.NodeID
}

// LivePeers returns a sorted snapshot of currently live peer NodeIDs.
func (t *peerTracker) LivePeers() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.live))
	for k := range t.live {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stop cancels the refresh + watch loops and waits for the watch to
// return. Safe to call multiple times.
func (t *peerTracker) stop() {
	if t == nil {
		return
	}
	t.cancel()
	<-t.done
	if t.stableDone != nil {
		<-t.stableDone
	}
	if t.deprecatedDone != nil {
		<-t.deprecatedDone
	}
	if t.mcpConfigDone != nil {
		<-t.mcpConfigDone
	}
	if t.drainDone != nil {
		<-t.drainDone
	}
	if t.externalsDone != nil {
		<-t.externalsDone
	}
	if t.notificationsDone != nil {
		<-t.notificationsDone
	}
	t.rec.stop()
}
