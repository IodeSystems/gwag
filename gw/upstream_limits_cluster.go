package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Cluster side of upstream limits.
//
//	gwag-limits          rate limits, quotas, costs set by operators.
//	                     No TTL. Every node watches it.
//	gwag-quota-counters  long-window quota counters, one key per
//	                     (quota, caller, window); max age 2 x the
//	                     longest window (48h).
//	gwag-usage           usage rows, one key per (day, caller,
//	                     namespace, version, operation); max age the
//	                     usage retention.
//
// Pauses after an upstream 429 travel on the core NATS subject
// gwag.upstream.pause, not in a bucket: a pause lasts seconds to
// minutes, a node that misses one (it was restarting) gets its own
// 429 from the upstream and pauses then, and a publish costs no
// JetStream write on the path of a failing call. The message carries
// the remaining time, not a wall-clock instant, so node clock skew
// does not matter.

const (
	limitsBucketName        = "gwag-limits"
	quotaCountersBucketName = "gwag-quota-counters"
	usageBucketName         = "gwag-usage"

	quotaCountersTTL = 2 * maxQuotaWindow

	upstreamPauseSubject = "gwag.upstream.pause"
)

// limitsCluster is the bound cluster state of upstreamLimits.
type limitsCluster struct {
	l        *upstreamLimits
	nc       *nats.Conn
	nodeID   string
	limits   jetstream.KeyValue
	counters jetstream.KeyValue
	usage    jetstream.KeyValue
}

// limitsBuckets are the three buckets, opened by startClusterTracking.
type limitsBuckets struct {
	limits, counters, usage jetstream.KeyValue
}

func openLimitsBuckets(ctx context.Context, js jetstream.JetStream, r int, usageTTL time.Duration) (limitsBuckets, error) {
	var b limitsBuckets
	var err error
	if b.limits, err = openKV(ctx, js, jetstream.KeyValueConfig{Bucket: limitsBucketName}, r); err != nil {
		return b, err
	}
	if b.counters, err = openKV(ctx, js, jetstream.KeyValueConfig{Bucket: quotaCountersBucketName, TTL: quotaCountersTTL}, r); err != nil {
		return b, err
	}
	if b.usage, err = openKV(ctx, js, jetstream.KeyValueConfig{Bucket: usageBucketName, TTL: usageTTL}, r); err != nil {
		return b, err
	}
	return b, nil
}

// usageBucketTTL is the gwag-usage max age a node creates the bucket
// with.
func (g *Gateway) usageBucketTTL() time.Duration {
	if g.cfg.usage != nil {
		return g.cfg.usage.retention()
	}
	return defaultUsageRetention
}

// bindCluster starts the limits watch and the pause listener. ctx is
// the peer tracker's.
func (l *upstreamLimits) bindCluster(ctx context.Context, nc *nats.Conn, nodeID string, b limitsBuckets) {
	c := &limitsCluster{l: l, nc: nc, nodeID: nodeID, limits: b.limits, counters: b.counters, usage: b.usage}
	if sub, err := nc.Subscribe(upstreamPauseSubject, c.onPause); err == nil {
		go func() {
			<-ctx.Done()
			_ = sub.Unsubscribe()
		}()
	} else {
		slog.Warn("gwag: upstream pause listener", "err", err)
	}
	l.cl.Store(c)
	l.setPeers(l.g.splitPeerCount())
	go c.watchLoop(ctx)
}

type pauseMsg struct {
	Namespace string `json:"namespace"`
	Version   string `json:"version"`
	Ms        int64  `json:"ms"`
	Node      string `json:"node"`
}

func (c *limitsCluster) publishPause(ns, ver string, d time.Duration) {
	b, _ := json.Marshal(pauseMsg{Namespace: ns, Version: ver, Ms: d.Milliseconds(), Node: c.nodeID})
	_ = c.nc.Publish(upstreamPauseSubject, b)
}

func (c *limitsCluster) onPause(m *nats.Msg) {
	var p pauseMsg
	if json.Unmarshal(m.Data, &p) != nil || p.Node == c.nodeID || p.Ms <= 0 {
		return
	}
	if c.l.applyPause(p.Namespace, p.Version, time.Duration(p.Ms)*time.Millisecond) {
		c.l.metrics.pause(limitScope{Namespace: p.Namespace, Version: p.Version}, "peer")
	}
}

// watchLoop keeps the operator records in step with gwag-limits.
func (c *limitsCluster) watchLoop(ctx context.Context) {
	for ctx.Err() == nil {
		w, err := c.limits.WatchAll(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		recs := map[string]limitRecord{}
		ready := false
		for e := range w.Updates() {
			if e == nil {
				ready = true
				c.l.setAdmin(cloneRecords(recs))
				continue
			}
			if e.Operation() != jetstream.KeyValuePut {
				delete(recs, e.Key())
			} else {
				var r limitRecord
				if json.Unmarshal(e.Value(), &r) == nil {
					recs[e.Key()] = r
				}
			}
			if ready {
				c.l.setAdmin(cloneRecords(recs))
			}
		}
		_ = w.Stop()
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func cloneRecords(m map[string]limitRecord) map[string]limitRecord {
	out := make(map[string]limitRecord, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// kvAdd adds to the value under key by compare and set, reading the
// current value from the stream leader. Returns the stored value.
func kvAdd[T any](ctx context.Context, nc *nats.Conn, kv jetstream.KeyValue, bucket, key string, add func(*T)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		val, rev, ok, err := kvLeaderEntry(ctx, nc, bucket, key)
		if err != nil {
			return zero, err
		}
		var cur T
		if ok {
			if err := json.Unmarshal(val, &cur); err != nil {
				return zero, fmt.Errorf("%s %s: %w", bucket, key, err)
			}
		}
		add(&cur)
		b, err := json.Marshal(cur)
		if err != nil {
			return zero, err
		}
		if rev != 0 {
			_, err = kv.Update(ctx, key, b, rev)
		} else {
			_, err = kv.Create(ctx, key, b)
		}
		if err == nil {
			return cur, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return zero, fmt.Errorf("%s %s: compare and set: %w", bucket, key, lastErr)
}

type counterValue struct {
	N uint64 `json:"n"`
}

// addCounter adds p's delta to its cluster counter and returns the
// total.
func (c *limitsCluster) addCounter(ctx context.Context, p pendingFlush) (uint64, error) {
	key := p.counterKey()
	if p.delta == 0 {
		kctx, cancel := kvCtx(ctx)
		defer cancel()
		val, ok, err := kvLeaderGet(kctx, c.nc, quotaCountersBucketName, key)
		if err != nil || !ok {
			return 0, err
		}
		var v counterValue
		if err := json.Unmarshal(val, &v); err != nil {
			return 0, err
		}
		return v.N, nil
	}
	kctx, cancel := kvCtx(ctx)
	defer cancel()
	v, err := kvAdd(kctx, c.nc, c.counters, quotaCountersBucketName, key, func(v *counterValue) { v.N += p.delta })
	return v.N, err
}

// usageKV is a stored usage row.
type usageKV struct {
	Caller    string `json:"caller"`
	Namespace string `json:"namespace"`
	Version   string `json:"version"`
	Operation string `json:"operation"`
	usageCounts
}

func usageRowKey(id usageID) string {
	raw := id.Caller + "\x00" + id.Namespace + "\x00" + id.Version + "\x00" + id.Operation
	return "u." + strings.ReplaceAll(id.Day, "-", "") + "." + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// addUsage adds deltas to the stored rows; returns the ones it could
// not store.
func (c *limitsCluster) addUsage(ctx context.Context, deltas map[usageID]usageCounts) map[usageID]usageCounts {
	failed := map[usageID]usageCounts{}
	for id, d := range deltas {
		kctx, cancel := kvCtx(ctx)
		_, err := kvAdd(kctx, c.nc, c.usage, usageBucketName, usageRowKey(id), func(v *usageKV) {
			v.Caller, v.Namespace, v.Version, v.Operation = id.Caller, id.Namespace, id.Version, id.Operation
			v.usageCounts.add(d)
		})
		cancel()
		if err != nil {
			failed[id] = d
		}
	}
	return failed
}

// usageRows reads the stored rows of each day in f's range. A watch
// per day (ordered consumer on the stream, last value per key)
// returns every row of the day in one pass.
func (c *limitsCluster) usageRows(ctx context.Context, f usageFilter) ([]usageRow, error) {
	from, err := time.Parse("2006-01-02", f.From)
	if err != nil {
		return nil, err
	}
	to, err := time.Parse("2006-01-02", f.To)
	if err != nil {
		return nil, err
	}
	var out []usageRow
	for d := from; !d.After(to); d = d.Add(24 * time.Hour) {
		day := d.Format("2006-01-02")
		rows, err := c.usageDay(ctx, day)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			id := usageID{Day: day, Caller: r.Caller, Namespace: r.Namespace, Version: r.Version, Operation: r.Operation}
			if f.match(id) {
				out = append(out, usageRow{Day: day, Caller: r.Caller, Namespace: r.Namespace, Version: r.Version, Operation: r.Operation, usageCounts: r.usageCounts})
			}
		}
	}
	sortUsageRows(out)
	return out, nil
}

func (c *limitsCluster) usageDay(ctx context.Context, day string) ([]usageKV, error) {
	wctx, cancel := context.WithTimeout(ctx, 4*kvCallTimeout)
	defer cancel()
	w, err := c.usage.Watch(wctx, "u."+strings.ReplaceAll(day, "-", "")+".>", jetstream.IgnoreDeletes())
	if err != nil {
		return nil, fmt.Errorf("usage %s: %w", day, err)
	}
	defer func() { _ = w.Stop() }()
	var out []usageKV
	for {
		select {
		case <-wctx.Done():
			return nil, fmt.Errorf("usage %s: %w", day, wctx.Err())
		case e, ok := <-w.Updates():
			if !ok {
				return out, nil
			}
			if e == nil {
				return out, nil
			}
			var v usageKV
			if json.Unmarshal(e.Value(), &v) == nil {
				out = append(out, v)
			}
		}
	}
}

// ---- operator records -----------------------------------------------

// errLimitsUnavailable: the cluster store is not bound yet.
var errLimitsUnavailable = errors.New("cluster limits store not ready")

// putRecord stores an operator record: in gwag-limits in a cluster
// (every node applies it from its watch), in memory standalone.
func (l *upstreamLimits) putRecord(ctx context.Context, r limitRecord) error {
	if err := r.validate(); err != nil {
		return err
	}
	if l.g.cfg.cluster == nil {
		recs := l.adminRecords()
		recs[r.key()] = r
		l.setAdmin(recs)
		return nil
	}
	c := l.cl.Load()
	if c == nil {
		return errLimitsUnavailable
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout)
	defer cancel()
	return kvPutRetry(kctx, c.limits, r.key(), b)
}

// deleteRecord removes an operator record. Returns whether it existed.
func (l *upstreamLimits) deleteRecord(ctx context.Context, key string) (bool, error) {
	if l.g.cfg.cluster == nil {
		recs := l.adminRecords()
		_, ok := recs[key]
		delete(recs, key)
		l.setAdmin(recs)
		return ok, nil
	}
	c := l.cl.Load()
	if c == nil {
		return false, errLimitsUnavailable
	}
	kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout)
	defer cancel()
	_, ok, err := kvLeaderGet(kctx, c.nc, limitsBucketName, key)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return true, kvDeleteRetry(kctx, c.limits, key)
}

// readRecords reads the operator records from the gwag-limits leader
// (standalone: memory). The admin list and the state export use it.
func (l *upstreamLimits) readRecords(ctx context.Context) (map[string]limitRecord, error) {
	if l.g.cfg.cluster == nil {
		return l.adminRecords(), nil
	}
	c := l.cl.Load()
	if c == nil {
		return nil, errLimitsUnavailable
	}
	kctx, cancel := context.WithTimeout(ctx, 4*kvCallTimeout)
	defer cancel()
	keys, err := kvLeaderKeys(kctx, c.nc, limitsBucketName)
	if err != nil {
		return nil, err
	}
	out := map[string]limitRecord{}
	for _, k := range keys {
		v, ok, err := kvLeaderGet(kctx, c.nc, limitsBucketName, k)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var r limitRecord
		if err := json.Unmarshal(v, &r); err != nil {
			return nil, fmt.Errorf("%s %s: %w", limitsBucketName, k, err)
		}
		out[k] = r
	}
	return out, nil
}
