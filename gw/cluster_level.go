package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Cluster feature levels.
//
// Nodes of two binary versions run side by side during an upgrade.
// A change to state the nodes share (a KV value format, a bucket or
// stream config) can break the older nodes: they cannot read the new
// format, or NATS refuses their writes. Such a change is gated on the
// cluster's active feature level, an integer stored in the
// gwag-cluster KV bucket. An upgrade then has two stages:
//
//  1. Expand. Every node runs the new binary at the old level: it
//     reads the old and the new format and writes the old one.
//     Rolling back to the old binary is safe.
//  2. Contract. An operator raises the level (FinalizeCluster,
//     `gwag ctl cluster finalize`, or WithClusterAutoFinalize). From then
//     on the nodes write the new format, and the old binary is
//     refused.
//
// Each binary supports the levels [clusterLevelMin, clusterLevelMax()]
// and publishes both in its peers entry. A node whose range does not
// contain the active level reports health "incompatible" and does not
// start cluster tracking. A gateway older than feature levels
// publishes neither and counts as [0, 0]; it cannot check the level,
// so the operator must not start one after a raise.
//
// Adding a level: append a clusterFeature with the next Level, gate
// the new writes on g.clusterFeatureOn(Level), and keep reading the
// old format. Apply converts existing shared state; it runs on every
// node when the level is reached, so it must be idempotent. Raise
// clusterLevelMin only when the old format reader is removed; a binary
// with a higher minimum cannot join a cluster still below it.

const (
	clusterBucketName = "gwag-cluster"
	clusterLevelKey   = "level"

	// clusterLevelMin is the lowest active level this binary runs at.
	clusterLevelMin = 0

	// levelDeleteMarkers: the gwag-registry and gwag-peers buckets
	// keep subject delete markers. A gateway before feature levels
	// updates those buckets with a config that has no markers, which
	// NATS refuses once they are on ("message TTL status can not be
	// disabled").
	levelDeleteMarkers = 1

	// levelAdminSessions: the gwag-admin-sessions bucket holds the
	// key that signs admin session cookies and the revocation
	// cutoffs (admin_session_store.go). Below it a cluster refuses
	// sessions: a node of an older binary would not check the
	// cutoffs, and would not manage the bucket's replicas.
	levelAdminSessions = 2
)

// clusterFeature is one feature level.
type clusterFeature struct {
	Level int
	Name  string
	// Apply converts shared state when the active level reaches
	// Level. Runs on every node, at start when the level is already
	// there and when a finalize raises it; must be idempotent. Nil
	// when there is nothing to convert.
	Apply func(ctx context.Context, t *peerTracker) error
}

// clusterFeatures is every feature level, by Level ascending.
var clusterFeatures = []clusterFeature{
	{Level: levelDeleteMarkers, Name: "kv-delete-markers", Apply: applyDeleteMarkers},
	{Level: levelAdminSessions, Name: "admin-sessions", Apply: applyAdminSessions},
}

// clusterLevelMax is the highest level this binary supports.
func clusterLevelMax() int {
	max := clusterLevelMin
	for _, f := range clusterFeatures {
		if f.Level > max {
			max = f.Level
		}
	}
	return max
}

// clusterLevelRecord is the value of the level key.
type clusterLevelRecord struct {
	Level     int    `json:"level"`
	SetUnixMs int64  `json:"set_unix_ms"`
	SetBy     string `json:"set_by,omitempty"`
}

// binaryVersion is the gateway module version, or "devel".
func binaryVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "devel"
}

// clusterFeatureOn reports whether the cluster's active level is at
// least level. Standalone gateways run at clusterLevelMax.
func (g *Gateway) clusterFeatureOn(level int) bool {
	return int(g.clusterLevel.Load()) >= level
}

// ClusterLevel is the cluster's active feature level as this node
// last read it. Standalone gateways report the highest level they
// support.
//
// Stability: experimental
func (g *Gateway) ClusterLevel() int { return int(g.clusterLevel.Load()) }

// checkClusterLevel returns the error a node reports when it cannot
// run at the active level.
func checkClusterLevel(active int) error {
	if active > clusterLevelMax() {
		return fmt.Errorf("cluster feature level %d is above the highest level this binary supports (%d, version %s): the cluster was finalized by a newer binary; run that version or newer", active, clusterLevelMax(), binaryVersion())
	}
	if active < clusterLevelMin {
		return fmt.Errorf("cluster feature level %d is below the lowest level this binary supports (%d, version %s): finalize the cluster to level %d with the current binary first (gwag ctl cluster finalize)", active, clusterLevelMin, binaryVersion(), clusterLevelMin)
	}
	return nil
}

// setClusterLevelErr records (or clears, with nil) why this node
// cannot run at the active level. Health reports it.
func (g *Gateway) setClusterLevelErr(err error) {
	if err == nil {
		g.clusterLevelErr.Store(nil)
		return
	}
	s := err.Error()
	g.clusterLevelErr.Store(&s)
}

// openClusterBucket creates (at r replicas, see openKV) or opens the gwag-cluster bucket.
func openClusterBucket(ctx context.Context, js jetstream.JetStream, r int) (jetstream.KeyValue, error) {
	return openKV(ctx, js, jetstream.KeyValueConfig{Bucket: clusterBucketName}, r)
}

// loadClusterLevel reads the active level, writing the first value
// when there is none: the lowest max level among the peers entries
// present (a gateway older than feature levels counts as 0), or this
// binary's max level when there are none. So a node of this version
// joining a cluster of older nodes starts at their level, and a new
// cluster starts at the highest level.
func loadClusterLevel(ctx context.Context, nc *nats.Conn, js jetstream.JetStream, kv jetstream.KeyValue, self string) (int, error) {
	if rec, _, err := getClusterLevel(ctx, nc); err == nil {
		return rec.Level, nil
	} else if !errors.Is(err, jetstream.ErrKeyNotFound) {
		return 0, err
	}
	level := clusterLevelMax()
	if _, err := js.KeyValue(ctx, peersBucketName); err == nil {
		kctx, cancel := kvCtx(ctx)
		entries, err := readPeerEntries(kctx, nc)
		cancel()
		if err != nil {
			return 0, err
		}
		for _, pe := range entries {
			if pe.LevelMax < level {
				level = pe.LevelMax
			}
		}
	} else if !errors.Is(err, jetstream.ErrBucketNotFound) {
		return 0, fmt.Errorf("peers bucket: %w", err)
	}
	b, _ := json.Marshal(clusterLevelRecord{Level: level, SetUnixMs: time.Now().UnixMilli(), SetBy: self})
	if _, err := kv.Create(ctx, clusterLevelKey, b); err != nil {
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return 0, fmt.Errorf("cluster level: %w", err)
		}
		rec, _, err := getClusterLevel(ctx, nc)
		if err != nil {
			return 0, err
		}
		return rec.Level, nil
	}
	return level, nil
}

// getClusterLevel reads the level record and its revision from the
// stream leader (kvLeaderEntry), not with a direct get that a replica
// behind the leader can answer with an older level, or without the
// key. A bucket that was just created has no leader for a moment: a
// failed lookup is retried for up to kvCallTimeout.
func getClusterLevel(ctx context.Context, nc *nats.Conn) (clusterLevelRecord, uint64, error) {
	var (
		v   []byte
		rev uint64
		ok  bool
		err error
	)
	deadline := time.Now().Add(kvCallTimeout)
	for {
		kctx, cancel := kvCtx(ctx)
		v, rev, ok, err = kvLeaderEntry(kctx, nc, clusterBucketName, clusterLevelKey)
		cancel()
		if !errors.Is(err, errKVLookup) || ctx.Err() != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return clusterLevelRecord{}, 0, fmt.Errorf("cluster level: %w", err)
	}
	if !ok {
		return clusterLevelRecord{}, 0, jetstream.ErrKeyNotFound
	}
	var rec clusterLevelRecord
	if err := json.Unmarshal(v, &rec); err != nil {
		return clusterLevelRecord{}, 0, fmt.Errorf("cluster level: %w", err)
	}
	return rec, rev, nil
}

// readPeerEntries returns every entry of the peers bucket, as the
// stream leader holds it (kv_leader.go). Not KeyValue.Keys/Get: a
// direct get may be answered by a replica that is behind or
// rebuilding its store, which shows an expired or deleted peer as
// live, or misses a live one. Either is wrong here: the first blocks
// a finalize after the peer was forgotten, the second sets a level a
// live peer cannot run at.
func readPeerEntries(ctx context.Context, nc *nats.Conn) ([]peerEntry, error) {
	keys, err := kvLeaderKeys(ctx, nc, peersBucketName)
	if err != nil {
		return nil, fmt.Errorf("peers: %w", err)
	}
	var out []peerEntry
	for _, k := range keys {
		v, ok, err := kvLeaderGet(ctx, nc, peersBucketName, k)
		if err != nil {
			// Not "no peer": skipping an old peer here would set
			// a level it cannot run at.
			return nil, fmt.Errorf("peers %s: %w", k, err)
		}
		if !ok {
			continue // deleted (ForgetPeer) or expired since the listing
		}
		var pe peerEntry
		if json.Unmarshal(v, &pe) == nil {
			out = append(out, pe)
		}
	}
	return out, nil
}

// levelWatchLoop follows the level key. A level outside this
// binary's range turns health "incompatible"; a raise applies the
// features up to it.
func (t *peerTracker) levelWatchLoop(ctx context.Context) {
	defer close(t.levelDone)
	g := t.gw
	for {
		w, err := t.cluster.Watch(ctx, clusterLevelKey)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(time.Second)
			continue
		}
		for ev := range w.Updates() {
			if ev == nil || ev.Operation() != jetstream.KeyValuePut {
				continue
			}
			var rec clusterLevelRecord
			if json.Unmarshal(ev.Value(), &rec) != nil {
				continue
			}
			prev := int(g.clusterLevel.Swap(int32(rec.Level)))
			if err := checkClusterLevel(rec.Level); err != nil {
				g.setClusterLevelErr(err)
				g.cfg.cluster.Server.Warnf("gateway: %v", err)
				continue
			}
			g.setClusterLevelErr(nil)
			if rec.Level != prev {
				g.cfg.cluster.Server.Warnf("gateway: cluster feature level %d (was %d)", rec.Level, prev)
			}
			t.applyFeatures(ctx)
		}
		_ = w.Stop()
		if ctx.Err() != nil {
			return
		}
	}
}

// applyFeatures runs Apply for every feature at or below the active
// level that this node has not applied yet. A failure is retried on
// the next call (each peer refresh).
func (t *peerTracker) applyFeatures(ctx context.Context) {
	t.levelMu.Lock()
	defer t.levelMu.Unlock()
	active := int(t.gw.clusterLevel.Load())
	for _, f := range clusterFeatures {
		if f.Level > active || f.Level <= t.appliedLevel {
			continue
		}
		if f.Apply != nil {
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := f.Apply(c, t)
			cancel()
			if err != nil {
				t.gw.cfg.cluster.Server.Warnf("gateway: cluster feature %s (level %d): %v", f.Name, f.Level, err)
				return
			}
		}
		t.appliedLevel = f.Level
	}
}

// applyDeleteMarkers turns subject delete markers on for the two
// expiring buckets (see openExpiringBucket).
func applyDeleteMarkers(ctx context.Context, t *peerTracker) error {
	peerTTL, registryTTL := t.gw.clusterTTLs()
	for _, b := range []struct {
		name string
		ttl  time.Duration
	}{{peersBucketName, peerTTL}, {registryBucketName, registryTTL}} {
		if _, err := applyStream(ctx, t.js, "KV_"+b.name, t.gw.isJetStreamLeader, func(cur *jetstream.StreamConfig) (jetstream.StreamConfig, bool) {
			if cur == nil {
				return jetstream.StreamConfig{}, false
			}
			want := *cur
			withDeleteMarkers(&want, b.ttl)
			return want, true
		}); err != nil {
			return fmt.Errorf("%s: %w", b.name, err)
		}
	}
	return nil
}

// withDeleteMarkers turns delete markers on in cfg, if they are off.
// The server allows turning them on in place, never off.
func withDeleteMarkers(cfg *jetstream.StreamConfig, ttl time.Duration) {
	if cfg.SubjectDeleteMarkerTTL > 0 {
		return
	}
	cfg.AllowMsgTTL = true
	cfg.SubjectDeleteMarkerTTL = cfg.MaxAge
	if cfg.SubjectDeleteMarkerTTL < time.Second {
		cfg.SubjectDeleteMarkerTTL = ttl
	}
}

// FinalizeResult is the outcome of FinalizeClusterLevel.
type finalizeResult struct {
	Previous int
	Active   int
	Blockers []string
}

// errFinalizeBlocked is returned when a live peer cannot run at the
// requested level.
var errFinalizeBlocked = errors.New("cluster: a live peer cannot run at the requested level")

// finalizeClusterLevel raises the active level to level (0 = the
// highest level this binary supports). Live peers are the entries of
// the peers bucket; one whose binary cannot run at the level blocks
// the raise. dryRun reports without writing.
func (g *Gateway) finalizeClusterLevel(ctx context.Context, level int, dryRun bool) (finalizeResult, error) {
	g.mu.Lock()
	t := g.peers
	g.mu.Unlock()
	if t == nil {
		return finalizeResult{}, errors.New("cluster: not configured or not started")
	}
	kctx, cancel := kvCtx(ctx)
	defer cancel()
	rec, rev, err := getClusterLevel(kctx, t.gw.cfg.cluster.Conn)
	if err != nil {
		return finalizeResult{}, err
	}
	res := finalizeResult{Previous: rec.Level, Active: rec.Level}
	entries, err := readPeerEntries(kctx, t.gw.cfg.cluster.Conn)
	if err != nil {
		return res, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	target := level
	if target == 0 {
		target = clusterLevelMax()
	}
	if target < rec.Level {
		return res, fmt.Errorf("cluster: level %d is below the active level %d; the level never goes down", target, rec.Level)
	}
	if target == rec.Level {
		return res, nil
	}
	for _, pe := range entries {
		if pe.LevelMax < target {
			res.Blockers = append(res.Blockers, fmt.Sprintf("%s %s max %d", peerLabel(pe), peerVersion(pe), pe.LevelMax))
		}
	}
	if len(res.Blockers) > 0 {
		return res, fmt.Errorf("%w %d: %s", errFinalizeBlocked, target, strings.Join(res.Blockers, "; "))
	}
	if dryRun {
		res.Active = target
		return res, nil
	}
	b, _ := json.Marshal(clusterLevelRecord{Level: target, SetUnixMs: time.Now().UnixMilli(), SetBy: g.cfg.cluster.Server.Name()})
	if _, err := t.cluster.Update(kctx, clusterLevelKey, b, rev); err != nil {
		return res, fmt.Errorf("cluster level: %w (changed concurrently? read again)", err)
	}
	res.Active = target
	g.clusterLevel.Store(int32(target))
	t.applyFeatures(ctx)
	return res, nil
}

// autoFinalize raises the level when every live peer supports a
// higher one (WithClusterAutoFinalize). Called on the peer refresh.
func (t *peerTracker) autoFinalize(ctx context.Context) {
	active := int(t.gw.clusterLevel.Load())
	if !t.gw.cfg.clusterAutoFinalize || active >= clusterLevelMax() {
		return
	}
	kctx, cancel := kvCtx(ctx)
	entries, err := readPeerEntries(kctx, t.gw.cfg.cluster.Conn)
	cancel()
	if err != nil {
		return
	}
	target := clusterLevelMax()
	for _, pe := range entries {
		if pe.LevelMax < target {
			target = pe.LevelMax
		}
	}
	if target <= active {
		return
	}
	res, err := t.gw.finalizeClusterLevel(ctx, target, false)
	if err == nil && res.Active > res.Previous {
		t.gw.cfg.cluster.Server.Warnf("gateway: auto-finalize raised the cluster feature level %d -> %d", res.Previous, res.Active)
	}
}

func peerLabel(pe peerEntry) string {
	if pe.Name != "" {
		return pe.Name
	}
	return pe.NodeID
}

func peerVersion(pe peerEntry) string {
	if pe.Version == "" {
		return "(no level support)"
	}
	return pe.Version
}

// WithClusterAutoFinalize raises the cluster feature level as soon
// as every live peer supports a higher one, instead of waiting for
// FinalizeCluster. The raise ends the rollback window: after it, the
// previous binary is refused. A peer that is down at that moment is
// not counted. Off by default.
//
// Stability: experimental
func WithClusterAutoFinalize() Option {
	return func(cfg *config) { cfg.clusterAutoFinalize = true }
}
