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
//
// Version, LevelMin and LevelMax are absent in entries written by a
// gateway older than feature levels; such a peer counts as [0, 0].
type peerEntry struct {
	NodeID   string `json:"node_id"`
	Name     string `json:"name,omitempty"`
	JoinedM  int64  `json:"joined_unix_ms"`
	Version  string `json:"version,omitempty"`
	LevelMin int    `json:"level_min,omitempty"`
	LevelMax int    `json:"level_max,omitempty"`
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
	appKeys    jetstream.KeyValue
	// cluster holds the active feature level (cluster_level.go).
	cluster jetstream.KeyValue
	// notifications holds the hook-failure notification items
	// (notification_store.go).
	notifications jetstream.KeyValue
	// buckets is every managed bucket: replicaBuckets and
	// gwag-schemas (skipped while it does not exist).
	buckets []string

	nodeID string
	self   []byte

	// peerTTL / registryTTL are the bucket TTLs (clusterTTLs).
	peerTTL     time.Duration
	registryTTL time.Duration

	mu   sync.Mutex
	live map[string]struct{} // active peer NodeIDs (incl. self)
	// names maps every peer NodeID this node has seen to its server
	// name, so ForgetPeer can remove an expired peer from the
	// JetStream meta group.
	names map[string]string

	// levelMu guards appliedLevel: the highest feature level whose
	// Apply this node has run.
	levelMu      sync.Mutex
	appliedLevel int

	cancel context.CancelFunc
	done   chan struct{}

	stableDone        chan struct{}
	deprecatedDone    chan struct{}
	mcpConfigDone     chan struct{}
	drainDone         chan struct{}
	externalsDone     chan struct{}
	notificationsDone chan struct{}
	levelDone         chan struct{}

	// currentR is the managed buckets' replica count as last read
	// from JetStream or applied by this node. replMu serialises the
	// read-compare-write of a raise (reconcileReplicas) and a shrink
	// (ForgetPeer).
	currentR atomic.Int32
	replMu   sync.Mutex
	// reconciling is set while a kickReconcile goroutine runs;
	// reconcilePending is a kick it has not run for yet.
	reconciling      atomic.Bool
	reconcilePending atomic.Bool

	rec *reconciler
}

// startClusterTracking is idempotent — only the first invocation starts
// the goroutines. Subsequent calls return the same tracker.
func (g *Gateway) startClusterTracking(ctx context.Context) (*peerTracker, error) {
	if g.cfg.cluster == nil {
		return nil, errors.New("gateway: cluster is not configured")
	}
	g.armRegistrySync()
	// trackMu, not g.mu, is held while the buckets open: those are
	// JetStream round trips that can take seconds (meta leader
	// election, a stream catching up after a restart). g.mu held
	// across them blocks every registration, and with it the HTTP
	// listener of a booting gateway.
	g.trackMu.Lock()
	defer g.trackMu.Unlock()
	g.mu.Lock()
	t := g.peers
	g.mu.Unlock()
	if t != nil {
		return t, nil
	}
	cl := g.cfg.cluster

	// Buckets this node creates get the replica count the cluster
	// runs at (bootReplicaTarget), not R1: a raise later can place a
	// replica on a node being evacuated (cluster_evacuate.go).
	r := bootReplicaTarget(ctx, cl.JS)

	// The feature level comes first: it decides the format of what
	// the rest writes. A level this binary cannot run at stops here,
	// before anything is written.
	clusterKV, err := openClusterBucket(ctx, cl.JS, r)
	if err != nil {
		return nil, err
	}
	level, err := loadClusterLevel(ctx, cl.Conn, cl.JS, clusterKV, cl.Server.Name())
	if err != nil {
		return nil, err
	}
	g.clusterLevel.Store(int32(level))
	if err := checkClusterLevel(level); err != nil {
		g.setClusterLevelErr(err)
		return nil, err
	}
	g.setClusterLevelErr(nil)

	// Peers and registry entries expire when their writer stops
	// refreshing them. From levelDeleteMarkers on, both buckets leave
	// a delete marker on expiry so every watcher sees the key go
	// (openExpiringBucket); below it the sweeps (sweepPeers,
	// reconciler.sweepLoop) drop expired keys.
	markers := level >= levelDeleteMarkers
	peerTTL, registryTTL := g.clusterTTLs()
	peers, err := openExpiringBucket(ctx, cl.JS, peersBucketName, peerTTL, markers, r, g.isJetStreamLeader)
	if err != nil {
		return nil, err
	}
	reg, err := openExpiringBucket(ctx, cl.JS, registryBucketName, registryTTL, markers, r, g.isJetStreamLeader)
	if err != nil {
		return nil, err
	}

	stable, err := openKV(ctx, cl.JS, jetstream.KeyValueConfig{Bucket: stableBucketName, TTL: stableTTL}, r)
	if err != nil {
		return nil, err
	}
	deprecated, err := openKV(ctx, cl.JS, jetstream.KeyValueConfig{Bucket: deprecatedBucketName, TTL: deprecatedTTL}, r)
	if err != nil {
		return nil, err
	}
	mcpConfig, err := openKV(ctx, cl.JS, jetstream.KeyValueConfig{Bucket: mcpConfigBucketName, TTL: mcpConfigTTL}, r)
	if err != nil {
		return nil, err
	}

	drain, err := openDrainBucket(ctx, cl.JS, r)
	if err != nil {
		return nil, err
	}

	externals, err := openExternalDependentsBucket(ctx, cl.JS, r)
	if err != nil {
		return nil, err
	}

	notifications, err := openNotificationsBucket(ctx, cl.JS, r)
	if err != nil {
		return nil, err
	}

	appKeys, err := openAppKeysBucket(ctx, cl.JS, r)
	if err != nil {
		return nil, err
	}

	// Pulled service profiles (profile_pull.go). No TTL.
	pulled, err := openKV(ctx, cl.JS, jetstream.KeyValueConfig{Bucket: pulledBucketName}, r)
	if err != nil {
		return nil, err
	}
	g.setPulledKV(pulled)

	var schemas jetstream.KeyValue
	if g.sr != nil {
		if schemas, err = openSchemasBucket(ctx, cl.JS, r); err != nil {
			return nil, err
		}
	}
	// gwag-schemas is managed whether or not this node serves the
	// Schema Registry: another node may, and a raise or ForgetPeer
	// must reach every bucket. A missing one is skipped.
	buckets := append(append([]string(nil), replicaBuckets...), schemasBucketName)

	// A bucket created during an evacuation can be placed on the
	// node being evacuated.
	kctx, kcancel := kvCtx(ctx)
	evac, _, _ := getEvacuations(kctx, cl.Conn)
	kcancel()
	for _, b := range buckets {
		g.moveOffEvacuating(ctx, cl.JS, "KV_"+b, evac.Peers)
	}

	selfBytes, err := json.Marshal(peerEntry{
		NodeID:   cl.NodeID,
		Name:     cl.Server.Name(),
		JoinedM:  time.Now().UnixMilli(),
		Version:  binaryVersion(),
		LevelMin: clusterLevelMin,
		LevelMax: clusterLevelMax(),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal self: %w", err)
	}

	tctx, cancel := context.WithCancel(ctx)
	t = &peerTracker{
		gw:                g,
		js:                cl.JS,
		peers:             peers,
		reg:               reg,
		stable:            stable,
		deprecated:        deprecated,
		mcpConfig:         mcpConfig,
		drain:             drain,
		externals:         externals,
		appKeys:           appKeys,
		cluster:           clusterKV,
		notifications:     notifications,
		buckets:           buckets,
		nodeID:            cl.NodeID,
		self:              selfBytes,
		peerTTL:           peerTTL,
		registryTTL:       registryTTL,
		live:              map[string]struct{}{cl.NodeID: {}},
		names:             map[string]string{cl.NodeID: cl.Server.Name()},
		cancel:            cancel,
		done:              make(chan struct{}),
		stableDone:        make(chan struct{}),
		deprecatedDone:    make(chan struct{}),
		mcpConfigDone:     make(chan struct{}),
		drainDone:         make(chan struct{}),
		externalsDone:     make(chan struct{}),
		notificationsDone: make(chan struct{}),
		levelDone:         make(chan struct{}),
	}
	t.currentR.Store(1)
	g.mu.Lock()
	if g.closing.Load() {
		// Close ran while the buckets opened and found no tracker to
		// stop; nothing will stop this one either.
		g.mu.Unlock()
		cancel()
		return nil, errors.New("gateway: closed")
	}
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
		close(t.levelDone)
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
	go t.levelWatchLoop(tctx)
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
			t.applyFeatures(ctx)
			t.autoFinalize(ctx)
		}
	}
}

// kickReconcile runs reconcileReplicas off the refresh and watch
// loops, so a slow raise never delays the peer refresh or the live
// set. A raise on a node that is not the JetStream leader waits for
// the leader first (applyStream), many seconds; run in the watch
// loop it held every peer event behind it, and the live set stayed
// stale that long. A kick while one runs makes it run once more, for
// the newer live set. It retries a raise that failed and picks up a
// replica change another node made.
func (t *peerTracker) kickReconcile(ctx context.Context) {
	t.reconcilePending.Store(true)
	if !t.reconciling.CompareAndSwap(false, true) {
		return
	}
	go func() {
		for {
			for t.reconcilePending.Swap(false) {
				t.reconcileReplicas(ctx)
			}
			t.reconciling.Store(false)
			// A kick between the last Swap and the Store saw
			// reconciling set and returned: run for it.
			if !t.reconcilePending.Load() || !t.reconciling.CompareAndSwap(false, true) {
				return
			}
		}
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
// leaves (openExpiringBucket) arrives here as a Purge. ForgetPeer
// deletes the key, which arrives as a Delete. The reconcile runs off
// this loop (kickReconcile).
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
				t.kickReconcile(ctx)
				continue
			}
			t.mu.Lock()
			switch ev.Operation() {
			case jetstream.KeyValuePut:
				t.live[ev.Key()] = struct{}{}
				var pe peerEntry
				if json.Unmarshal(ev.Value(), &pe) == nil && pe.Name != "" {
					t.names[ev.Key()] = pe.Name
				}
			case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
				delete(t.live, ev.Key())
			}
			t.mu.Unlock()
			t.kickReconcile(ctx)
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
	appKeysBucketName,
	clusterBucketName,
	pulledBucketName,
}

// reconcileReplicas raises every managed bucket and event stream to
// the replica count the cluster policy (WithClusterReplicas) gives
// for the live peers. Strictly monotonic — never lowers; ForgetPeer
// is the only path that shrinks (a peer is only dropped from the live
// set after its entry expired, so a brief loss never shrinks).
// Idempotent if already at target. currentR is first set from the
// buckets' actual replica count, so a raise another node applied (or
// one whose answer this node never got) is not sent again. No raise
// while a node is being evacuated (cluster_evacuate.go): the new
// replicas could be placed on it. The refresh tick retries.
func (t *peerTracker) reconcileReplicas(ctx context.Context) {
	t.replMu.Lock()
	defer t.replMu.Unlock()
	t.mu.Lock()
	desired := t.gw.cfg.clusterReplicas.For(len(t.live))
	t.mu.Unlock()
	cur := t.syncReplicasLocked(ctx)
	// An event stream can be below the buckets: its raise failed in a
	// setReplicas whose bucket raises went through.
	if desired <= cur && !t.gw.eventStreamBelow(ctx, t.js, desired) {
		return
	}
	if t.evacuating(ctx) {
		return
	}
	if err := t.setReplicas(ctx, desired, false); err != nil {
		// Best-effort. The next peer change or refresh tick retries.
		return
	}
	t.currentR.Store(int32(max(cur, desired)))
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
// managed buckets that exist.
func (t *peerTracker) appliedReplicas(ctx context.Context) (int, error) {
	low := 0
	for _, b := range t.buckets {
		cfg, err := readStreamConfig(ctx, t.js, "KV_"+b)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", b, err)
		}
		if cfg == nil {
			continue
		}
		if low == 0 || cfg.Replicas < low {
			low = cfg.Replicas
		}
	}
	if low == 0 {
		return 0, errStreamMissing
	}
	return low, nil
}

// setReplicas sets the replica count of every cluster bucket and
// event stream (applyStream: no write when already there, one writer
// per change, a lost answer verified by reading). If one fails we
// still try the others; caller decides whether to retry. Only the
// replica count changes; the rest of each bucket's config stays.
// lower false raises only: a bucket or stream already above r (a
// node with a higher fixed policy raised it) keeps its count. lower
// true sets r exactly (ForgetPeer). A missing bucket is skipped.
//
// A lower comes from one ForgetPeer call, not from every node
// reacting to the same event, so it writes at once: applyStream's
// leader-first wait would cost up to applyStreamFollowerWait per
// stream for a write no other node sends.
func (t *peerTracker) setReplicas(ctx context.Context, r int, lower bool) error {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	leader := t.gw.isJetStreamLeader
	if lower {
		leader = func() bool { return true }
	}
	var firstErr error
	for _, b := range t.buckets {
		// A raise on a node that is not the meta leader takes seconds
		// per bucket; an evacuation recorded meanwhile stops the rest
		// (reconcileReplicas checked only before the first).
		if !lower && t.evacuating(c) {
			return errRaiseHeld
		}
		_, err := applyStream(c, t.js, "KV_"+b, leader, func(cur *jetstream.StreamConfig) (jetstream.StreamConfig, bool) {
			if cur == nil {
				return jetstream.StreamConfig{}, false
			}
			want := *cur
			want.Replicas = nextReplicas(cur.Replicas, r, lower)
			return want, true
		})
		if err != nil && !errors.Is(err, errStreamMissing) && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", b, err)
		}
	}
	// Named-channel event streams follow the same policy.
	if !lower && t.evacuating(c) {
		return errRaiseHeld
	}
	if err := t.gw.setEventStreamReplicas(c, t.js, r, lower, leader); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// errRaiseHeld: setReplicas stopped a raise because a node is being
// evacuated (cluster_evacuate.go).
var errRaiseHeld = errors.New("replica raise held: a node is being evacuated")

// nextReplicas is the replica count setReplicas gives a bucket or
// stream at cur.
func nextReplicas(cur, r int, lower bool) int {
	if lower {
		return r
	}
	return max(cur, r)
}

// bootReplicaTarget is the replica count for a bucket a booting node
// creates: the count of the gwag-peers bucket, which every gateway
// version keeps and setReplicas changes first, or 1 when there is no
// such bucket yet (a new cluster). The peer tracker raises it from
// there by the policy.
//
// It is read from JetStream, not kept as a separate cluster-wide
// value: the stream configs are what every node, every version
// (v2.11.0 raises its buckets by its own rules) and an operator with
// the nats CLI already read and write. A second copy would drift
// from them and need reconciling anyway.
//
// A failed read is retried for up to bootReplicaReadTimeout. A node
// that has just joined the meta group does not answer stream info
// until it is current with the meta leader (seen: a 3 s timeout right
// after "JetStream is not current with the meta leader"). Taking 1 on
// that error created every bucket the node adds at R1 in a cluster at
// R3, and the raise that follows on a node that is not the meta
// leader takes seconds per bucket.
func bootReplicaTarget(ctx context.Context, js jetstream.JetStream) int {
	deadline := time.Now().Add(bootReplicaReadTimeout)
	for {
		cfg, err := readStreamConfig(ctx, js, "KV_"+peersBucketName)
		if err == nil {
			if cfg == nil || cfg.Replicas < 1 {
				return 1
			}
			return cfg.Replicas
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return 1
		}
		select {
		case <-ctx.Done():
			return 1
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// bootReplicaReadTimeout bounds bootReplicaTarget's retries.
const bootReplicaReadTimeout = 20 * time.Second

// openKV creates bucket cfg at r replicas, or opens it when it exists
// (its config, replicas included, stays as it is). When the cluster
// cannot place r replicas now (fewer servers up than the gwag-peers
// count suggests) the bucket is created at R1 and raised later.
func openKV(ctx context.Context, js jetstream.JetStream, cfg jetstream.KeyValueConfig, r int) (jetstream.KeyValue, error) {
	cfg.Replicas = max(r, 1)
	kv, err := js.CreateKeyValue(ctx, cfg)
	if err != nil && cfg.Replicas > 1 && noPlacementErr(err) {
		cfg.Replicas = 1
		kv, err = js.CreateKeyValue(ctx, cfg)
	}
	if err != nil && !errors.Is(err, jetstream.ErrBucketExists) {
		return nil, fmt.Errorf("%s bucket: %w", cfg.Bucket, err)
	}
	if kv == nil {
		if kv, err = js.KeyValue(ctx, cfg.Bucket); err != nil {
			return nil, fmt.Errorf("%s bucket open: %w", cfg.Bucket, err)
		}
	}
	return kv, nil
}

// noPlacementErr: JetStream cannot place the requested replicas (no
// suitable peers, insufficient resources, replicas > 1 without a
// cluster).
func noPlacementErr(err error) bool {
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode {
	case 10005, 10023, 10074:
		return true
	}
	return false
}

// clusterTTLs returns the peers and registry bucket TTLs.
func (g *Gateway) clusterTTLs() (peer, registry time.Duration) {
	if d := g.cfg.clusterKVTTL; d > 0 {
		return d, d
	}
	return peerTTL, registryTTL
}

// openExpiringBucket opens (creating it at r replicas if needed, see
// openKV) a KV bucket whose entries expire ttl after their last Put,
// with subject delete markers on. JetStream removes an expired message
// without a delete event; the marker it writes instead is what tells
// every watcher (reconciler, peer tracker) that the key is gone.
// Without it a node that was watching keeps the key forever, while a
// node that replays the bucket after the expiry never sees it.
//
// A bucket created by an older gateway has no markers. Its stream
// config is updated in place with markers on and nothing else
// changed (replicas, limits); the server allows turning markers on,
// never off. markers is false below levelDeleteMarkers: the bucket is
// then created, or left, without them, because a gateway older than
// feature levels cannot update a bucket that has them.
func openExpiringBucket(ctx context.Context, js jetstream.JetStream, name string, ttl time.Duration, markers bool, r int, leader func() bool) (jetstream.KeyValue, error) {
	kcfg := jetstream.KeyValueConfig{
		Bucket: name,
		TTL:    ttl,
	}
	if markers {
		// A marker expires with the bucket TTL (MaxAge) whatever
		// its own TTL; ask for the same.
		kcfg.LimitMarkerTTL = ttl
	}
	kv, err := openKV(ctx, js, kcfg, r)
	if err != nil || !markers {
		return kv, err
	}
	if _, err := applyStream(ctx, js, "KV_"+name, leader, func(cur *jetstream.StreamConfig) (jetstream.StreamConfig, bool) {
		if cur == nil {
			return jetstream.StreamConfig{}, false
		}
		want := *cur
		withDeleteMarkers(&want, ttl)
		return want, true
	}); err != nil {
		return nil, fmt.Errorf("%s bucket: turn on delete markers: %w", name, err)
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
	if t.levelDone != nil {
		<-t.levelDone
	}
	t.rec.stop()
}
