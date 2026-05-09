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
	peersBucketName    = "go-api-gateway-peers"
	registryBucketName = "go-api-gateway-registry"
	stableBucketName   = "go-api-gateway-stable"

	peerTTL     = 30 * time.Second
	peerRefresh = 10 * time.Second

	registryTTL = 30 * time.Second

	// stableTTL is 0 — stable_vN is monotonic per plan §4 and must
	// survive any deregister/restart cycle. The bucket is the only
	// way a fresh node joining mid-life recovers the historical max
	// when the target vN's last replica is currently absent.
	stableTTL = 0

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
	gw     *Gateway
	js     jetstream.JetStream
	peers  jetstream.KeyValue
	reg    jetstream.KeyValue
	stable jetstream.KeyValue

	nodeID string
	self   []byte

	mu      sync.Mutex
	live    map[string]struct{} // active peer NodeIDs (incl. self)

	cancel context.CancelFunc
	done   chan struct{}

	stableDone chan struct{}

	currentR atomic.Int32

	rec *reconciler
}

// startClusterTracking is idempotent — only the first invocation starts
// the goroutines. Subsequent calls return the same tracker.
func (g *Gateway) startClusterTracking(ctx context.Context) (*peerTracker, error) {
	if g.cfg.cluster == nil {
		return nil, errors.New("gateway: cluster is not configured")
	}
	g.mu.Lock()
	if g.peers != nil {
		t := g.peers
		g.mu.Unlock()
		return t, nil
	}
	cl := g.cfg.cluster

	// Both buckets are created at R=1 if they don't exist; live R is
	// raised monotonically by reconcileReplicas as peers join.
	peers, err := cl.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   peersBucketName,
		Replicas: 1,
		TTL:      peerTTL,
	})
	if err != nil && !errors.Is(err, jetstream.ErrBucketExists) {
		g.mu.Unlock()
		return nil, fmt.Errorf("peers bucket: %w", err)
	}
	if peers == nil {
		peers, err = cl.JS.KeyValue(ctx, peersBucketName)
		if err != nil {
			g.mu.Unlock()
			return nil, fmt.Errorf("peers bucket open: %w", err)
		}
	}

	reg, err := cl.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   registryBucketName,
		Replicas: 1,
		TTL:      registryTTL,
	})
	if err != nil && !errors.Is(err, jetstream.ErrBucketExists) {
		g.mu.Unlock()
		return nil, fmt.Errorf("registry bucket: %w", err)
	}
	if reg == nil {
		reg, err = cl.JS.KeyValue(ctx, registryBucketName)
		if err != nil {
			g.mu.Unlock()
			return nil, fmt.Errorf("registry bucket open: %w", err)
		}
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
		gw:         g,
		js:         cl.JS,
		peers:      peers,
		reg:        reg,
		stable:     stable,
		nodeID:     cl.NodeID,
		self:       selfBytes,
		live:       map[string]struct{}{cl.NodeID: {}},
		cancel:     cancel,
		done:       make(chan struct{}),
		stableDone: make(chan struct{}),
	}
	t.currentR.Store(1)
	g.peers = t
	g.mu.Unlock()

	// Initial put of self before launching loops, so reconcile sees us.
	if _, err := peers.Put(ctx, cl.NodeID, selfBytes); err != nil {
		cancel()
		return nil, fmt.Errorf("put self: %w", err)
	}

	rec, err := g.startReconciler(tctx, reg)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start reconciler: %w", err)
	}
	t.rec = rec

	go t.refreshLoop(tctx)
	go t.watchLoop(tctx)
	go t.stableWatchLoop(tctx)
	return t, nil
}

func (t *peerTracker) refreshLoop(ctx context.Context) {
	tk := time.NewTicker(peerRefresh)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, _ = t.peers.Put(c, t.nodeID, t.self)
			cancel()
		}
	}
}

// watchLoop tracks the live peer set and triggers a replica reconcile
// on every change. KV TTL handles departure: a peer that stops
// refreshing has its key auto-expired, which fires a Delete event.
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

// reconcileReplicas raises both managed buckets to min(peers, maxReplicas).
// Strictly monotonic — never lowers; ForgetPeer is the only path that
// shrinks. Idempotent if already at target.
func (t *peerTracker) reconcileReplicas(ctx context.Context) {
	t.mu.Lock()
	desired := len(t.live)
	t.mu.Unlock()
	if desired > maxReplicas {
		desired = maxReplicas
	}
	if desired < 1 {
		desired = 1
	}
	cur := int(t.currentR.Load())
	if desired <= cur {
		return
	}
	if err := t.setReplicas(ctx, desired); err != nil {
		// Best-effort. Next peer change retries.
		return
	}
	t.currentR.Store(int32(desired))
}

// setReplicas updates all three buckets. If one update fails we still
// try the others; caller decides whether to retry.
func (t *peerTracker) setReplicas(ctx context.Context, r int) error {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var firstErr error
	for _, b := range []struct {
		name string
		ttl  time.Duration
	}{
		{peersBucketName, peerTTL},
		{registryBucketName, registryTTL},
		{stableBucketName, stableTTL},
	} {
		_, err := t.js.UpdateKeyValue(c, jetstream.KeyValueConfig{
			Bucket:   b.name,
			Replicas: r,
			TTL:      b.ttl,
		})
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", b.name, err)
		}
	}
	return firstErr
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
	t.rec.stop()
}
