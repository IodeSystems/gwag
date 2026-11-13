package gateway

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Usage report: per (caller, namespace, version, operation, UTC day)
// counts of calls, units, rejections and upstream 429s. Each node
// counts in memory and adds its counts to the stored day row every
// flush interval: in the gwag-usage bucket in a cluster (compare and
// set, so the rows are cluster totals), in memory standalone. Rows
// older than the retention are dropped (the bucket's max age in a
// cluster).

// UsageOptions configures the usage report (WithUsage).
//
// Stability: experimental
type UsageOptions struct {
	// Retention is how long day rows are kept. Zero → 35 days. In a
	// cluster it is the gwag-usage bucket's max age, set when the
	// first node creates the bucket.
	Retention time.Duration
	// FlushInterval is how often a node adds its counts to the stored
	// rows (and to long-window quota counters). Zero → 5s.
	FlushInterval time.Duration
}

// WithUsage records per (caller, namespace, version, operation, day)
// usage of every upstream call and serves it at GET
// /admin/usage (`gwag ctl usage`). Off by default. See docs/quota.md.
//
// Stability: experimental
func WithUsage(opts UsageOptions) Option {
	return func(cfg *config) { cfg.usage = &opts }
}

const (
	defaultUsageRetention = 35 * 24 * time.Hour
	defaultFlushInterval  = 5 * time.Second
)

func (o UsageOptions) retention() time.Duration {
	if o.Retention > 0 {
		return o.Retention
	}
	return defaultUsageRetention
}

func (o UsageOptions) flushInterval() time.Duration {
	if o.FlushInterval > 0 {
		return o.FlushInterval
	}
	return defaultFlushInterval
}

// usage outcomes.
type usageKind uint8

const (
	usageOK usageKind = iota
	usageError
	usageLimited     // own rate limit
	usageQuota       // built-in quota
	usageUpstream429 // upstream answered 429, or the service was paused
)

// usageCounts is one row's counts. Calls counts every call that
// reached the gate; units the cost of calls sent upstream.
type usageCounts struct {
	Calls       uint64 `json:"calls"`
	Units       uint64 `json:"units"`
	RateLimited uint64 `json:"rateLimited" doc:"Refused by the gateway's rate limit."`
	Quota       uint64 `json:"quotaExceeded" doc:"Refused by a built-in quota."`
	Upstream429 uint64 `json:"upstream429" doc:"Upstream answered 429, or the call was refused while the service was paused after one."`
	Errors      uint64 `json:"errors" doc:"Other failed calls."`
}

func (c *usageCounts) add(o usageCounts) {
	c.Calls += o.Calls
	c.Units += o.Units
	c.RateLimited += o.RateLimited
	c.Quota += o.Quota
	c.Upstream429 += o.Upstream429
	c.Errors += o.Errors
}

func (c usageCounts) zero() bool { return c == usageCounts{} }

type usageKey struct {
	caller string
	day    int32 // days since the Unix epoch, UTC
}

type usageCell struct {
	calls, units, limited, quota, up429, errors atomic.Uint64
}

func (c *usageCell) swap() usageCounts {
	return usageCounts{
		Calls:       c.calls.Swap(0),
		Units:       c.units.Swap(0),
		RateLimited: c.limited.Swap(0),
		Quota:       c.quota.Swap(0),
		Upstream429: c.up429.Swap(0),
		Errors:      c.errors.Swap(0),
	}
}

func epochDay(t time.Time) int32 { return int32(t.Unix() / 86400) }

func dayString(d int32) string {
	return time.Unix(int64(d)*86400, 0).UTC().Format("2006-01-02")
}

// record counts one call on the gate. units is charged for calls sent
// upstream (ok, error, upstream 429).
func (gt *opGate) record(caller string, now time.Time, kind usageKind, units uint32) {
	if gt.l.usage == nil {
		return
	}
	k := usageKey{caller: caller, day: epochDay(now)}
	v, ok := gt.usage.Load(k)
	if !ok {
		v, _ = gt.usage.LoadOrStore(k, &usageCell{})
	}
	c := v.(*usageCell)
	c.calls.Add(1)
	switch kind {
	case usageOK:
		c.units.Add(uint64(units))
	case usageError:
		c.units.Add(uint64(units))
		c.errors.Add(1)
	case usageUpstream429:
		c.units.Add(uint64(units))
		c.up429.Add(1)
	case usageLimited:
		c.limited.Add(1)
	case usageQuota:
		c.quota.Add(1)
	}
}

// usageID names one stored row.
type usageID struct {
	Day       string
	Caller    string
	Namespace string
	Version   string
	Operation string
}

// usageRecorder holds the standalone rows and the flush state.
type usageRecorder struct {
	l    *upstreamLimits
	opts UsageOptions

	mu   sync.Mutex
	rows map[usageID]usageCounts // standalone store
	// unsent are deltas a cluster flush could not store; retried on
	// the next flush.
	unsent map[usageID]usageCounts
}

func newUsageRecorder(l *upstreamLimits, opts UsageOptions) *usageRecorder {
	return &usageRecorder{l: l, opts: opts, rows: map[usageID]usageCounts{}, unsent: map[usageID]usageCounts{}}
}

// collect swaps every gate's cells into deltas by row.
func (u *usageRecorder) collect(now time.Time) map[usageID]usageCounts {
	l := u.l
	l.mu.Lock()
	gates := make([]*opGate, 0, len(l.gates))
	for _, gt := range l.gates {
		gates = append(gates, gt)
	}
	l.mu.Unlock()
	today := epochDay(now)
	out := map[usageID]usageCounts{}
	for _, gt := range gates {
		gt.usage.Range(func(k, v any) bool {
			uk := k.(usageKey)
			d := v.(*usageCell).swap()
			if uk.day < today {
				// Past day: nothing more lands in this cell after the
				// swap except a call racing the day change; it is
				// counted on the next flush if it does.
				gt.usage.Delete(k)
			}
			if d.zero() {
				return true
			}
			id := usageID{Day: dayString(uk.day), Caller: uk.caller, Namespace: gt.scope.Namespace, Version: gt.scope.Version, Operation: gt.scope.Operation}
			cur := out[id]
			cur.add(d)
			out[id] = cur
			return true
		})
	}
	return out
}

// flushLocal adds deltas to the standalone rows and drops rows past
// retention.
func (u *usageRecorder) flushLocal(now time.Time, deltas map[usageID]usageCounts) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for id, d := range deltas {
		cur := u.rows[id]
		cur.add(d)
		u.rows[id] = cur
	}
	oldest := dayString(epochDay(now.Add(-u.opts.retention())))
	for id := range u.rows {
		if id.Day < oldest {
			delete(u.rows, id)
		}
	}
}

// usageRow is one row of the report.
type usageRow struct {
	Day       string `json:"day" doc:"UTC day, YYYY-MM-DD."`
	Caller    string `json:"caller"`
	Namespace string `json:"namespace"`
	Version   string `json:"version"`
	Operation string `json:"operation"`
	usageCounts
}

// usageFilter selects report rows. Days are YYYY-MM-DD, inclusive.
type usageFilter struct {
	From, To  string
	Caller    string
	Namespace string
	Version   string
}

func (f usageFilter) match(id usageID) bool {
	if id.Day < f.From || id.Day > f.To {
		return false
	}
	if f.Caller != "" && id.Caller != f.Caller {
		return false
	}
	if f.Namespace != "" && id.Namespace != f.Namespace {
		return false
	}
	if f.Version != "" && id.Version != f.Version {
		return false
	}
	return true
}

func sortUsageRows(rows []usageRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case a.Day != b.Day:
			return a.Day < b.Day
		case a.Caller != b.Caller:
			return a.Caller < b.Caller
		case a.Namespace != b.Namespace:
			return a.Namespace < b.Namespace
		case a.Version != b.Version:
			return a.Version < b.Version
		}
		return a.Operation < b.Operation
	})
}

// report flushes this node's counts and returns the matching rows.
func (u *usageRecorder) report(ctx context.Context, f usageFilter) ([]usageRow, error) {
	if err := u.l.flush(ctx); err != nil {
		return nil, err
	}
	if c := u.l.cl.Load(); c != nil {
		return c.usageRows(ctx, f)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []usageRow
	for id, c := range u.rows {
		if f.match(id) {
			out = append(out, usageRow{Day: id.Day, Caller: id.Caller, Namespace: id.Namespace, Version: id.Version, Operation: id.Operation, usageCounts: c})
		}
	}
	sortUsageRows(out)
	return out, nil
}

// usageCSVHeader is the CSV report's header row.
var usageCSVHeader = []string{"day", "caller", "namespace", "version", "operation", "calls", "units", "rate_limited", "quota_exceeded", "upstream_429", "errors"}

func writeUsageCSV(w io.Writer, rows []usageRow) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(usageCSVHeader); err != nil {
		return err
	}
	u := func(n uint64) string { return strconv.FormatUint(n, 10) }
	for _, r := range rows {
		if err := cw.Write([]string{r.Day, r.Caller, r.Namespace, r.Version, r.Operation,
			u(r.Calls), u(r.Units), u(r.RateLimited), u(r.Quota), u(r.Upstream429), u(r.Errors)}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func usageCSV(rows []usageRow) []byte {
	var b bytes.Buffer
	_ = writeUsageCSV(&b, rows)
	return b.Bytes()
}

// defaultUsageRange is the last 7 UTC days, today included.
func defaultUsageRange(now time.Time) (from, to string) {
	d := epochDay(now)
	return dayString(d - 6), dayString(d)
}

// parseUsageDay checks a YYYY-MM-DD day.
func parseUsageDay(s string) (string, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return "", fmt.Errorf("day %q: want YYYY-MM-DD", s)
	}
	return t.Format("2006-01-02"), nil
}

// ---- flush ----------------------------------------------------------

// flush adds this node's usage and long-window quota counts to the
// store. Serialized; the flush loop and report both call it.
func (l *upstreamLimits) flush(ctx context.Context) error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	now := time.Now()
	var firstErr error
	if u := l.usage; u != nil {
		deltas := u.collect(now)
		if c := l.cl.Load(); c != nil {
			u.mu.Lock()
			for id, d := range u.unsent {
				cur := deltas[id]
				cur.add(d)
				deltas[id] = cur
			}
			u.unsent = map[usageID]usageCounts{}
			u.mu.Unlock()
			failed := c.addUsage(ctx, deltas)
			if len(failed) > 0 {
				u.mu.Lock()
				for id, d := range failed {
					cur := u.unsent[id]
					cur.add(d)
					u.unsent[id] = cur
				}
				u.mu.Unlock()
				firstErr = fmt.Errorf("usage: %d rows not stored; retried on the next flush", len(failed))
			}
		} else {
			u.flushLocal(now, deltas)
		}
	}
	if c := l.cl.Load(); c != nil {
		l.mu.Lock()
		rules := make([]*quotaRule, 0, len(l.rules))
		for _, r := range l.rules {
			rules = append(rules, r)
		}
		l.mu.Unlock()
		for _, r := range rules {
			for _, p := range r.takePending(now) {
				total, err := c.addCounter(ctx, p)
				r.finishFlush(p, total, err == nil)
				if err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
	} else {
		l.mu.Lock()
		for _, r := range l.rules {
			r.takePending(now) // drops past windows
		}
		l.mu.Unlock()
	}
	return firstErr
}

// flushLoop runs flush every interval until ctx ends, and updates the
// live peer count every second.
func (l *upstreamLimits) loop(ctx context.Context) {
	interval := defaultFlushInterval
	if l.usage != nil {
		interval = l.usage.opts.flushInterval()
	}
	flushT := time.NewTicker(interval)
	defer flushT.Stop()
	peerT := time.NewTicker(time.Second)
	defer peerT.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = l.flush(fctx)
			cancel()
			return
		case <-flushT.C:
			fctx, cancel := context.WithTimeout(ctx, interval+kvCallTimeout)
			_ = l.flush(fctx)
			cancel()
		case <-peerT.C:
			if l.g.cfg.cluster != nil {
				l.setPeers(l.g.splitPeerCount())
			}
		}
	}
}

// splitPeerCount is the peer count the split divides by: the larger
// of the peers bucket's live set and the NATS routes this node holds.
// The peers bucket lags a death by its TTL (30s; the survivors keep
// the smaller share meanwhile) and a node that just joined has not
// read it yet; routes show a joining node within seconds. Taking the
// larger errs toward a smaller share.
func (g *Gateway) splitPeerCount() int {
	if g.cfg.cluster == nil {
		return 1
	}
	n := g.livePeerCount()
	g.mu.Lock()
	t := g.peers
	g.mu.Unlock()
	if t != nil {
		if k := len(t.LivePeers()); k > n {
			n = k
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}
