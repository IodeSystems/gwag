package gateway

import (
	"strconv"
	"sync"
	"time"
)

// Built-in quotas: per caller-id limits over a fixed window, set by
// operators (`gwag ctl quota set`). The caller id is whatever caller-id
// mode the gateway runs (docs/caller-identity.md); "unknown" when none
// resolves.
//
// A quota names a scope (every service, a namespace, a version, an
// operation) and either one caller or the default for every caller
// without a quota of its own at that scope. A call is checked against
// every scope that covers it; each scope uses the caller's own quota
// when there is one, else the default. Each caller counts separately
// against a default quota.
//
// Windows are fixed and aligned to the Unix epoch (a 1m window starts
// on the minute, 1d at 00:00 UTC). Accuracy in a cluster:
//
//   - windows under 1m: each node allows limit / live peers (rounded
//     down, at least 1) on its own. Exact standalone; in a cluster a
//     caller whose calls land unevenly on the nodes gets less than
//     the limit, never more (except when limit < peers: then up to
//     one per node).
//   - windows of 1m and more: one counter per (quota, caller, window)
//     in the gwag-quota-counters bucket. Each node counts locally and
//     adds its count to the cluster counter every flush interval
//     (compare and set), reading back the total. A node rejects when
//     the last total it read plus its own unflushed count reaches the
//     limit, so the cluster can go over by what the other nodes
//     counted since their last flush.
//
// WithQuota (the _quota_auth/v1 delegate) replaces built-in quotas:
// with it configured, quota records are stored and listed but not
// checked.

// quotaScope is one scope's quotas: the default and per caller.
type quotaScope struct {
	def      *quotaRule
	byCaller map[string]*quotaRule
}

func (qs *quotaScope) ruleFor(caller string) *quotaRule {
	if r, ok := qs.byCaller[caller]; ok {
		return r
	}
	return qs.def
}

// quotaRule is one quota record with its counters on this node.
type quotaRule struct {
	id     string // record key
	scope  limitScope
	caller string // "" for the default quota

	mu       sync.Mutex
	limit    uint64
	per      time.Duration
	units    bool
	counters map[string]*quotaCounter // by caller
}

func newQuotaRule(key string, r limitRecord) *quotaRule {
	return &quotaRule{
		id:       key,
		scope:    r.scope(),
		caller:   r.Caller,
		limit:    r.Limit,
		per:      r.per(),
		units:    r.Units,
		counters: map[string]*quotaCounter{},
	}
}

func (qr *quotaRule) update(r limitRecord) {
	qr.mu.Lock()
	defer qr.mu.Unlock()
	if qr.per != r.per() {
		qr.counters = map[string]*quotaCounter{}
	}
	qr.limit, qr.per, qr.units = r.Limit, r.per(), r.Units
}

func (qr *quotaRule) perDur() time.Duration {
	qr.mu.Lock()
	defer qr.mu.Unlock()
	return qr.per
}

// shortWindow: below this a quota is split across nodes; at or above
// it the cluster counter is used.
const shortWindow = time.Minute

// quotaCounter is one caller's count in the current window.
type quotaCounter struct {
	window int64  // window start, unix ms
	used   uint64 // counted on this node and not yet flushed
	known  uint64 // cluster total as last read (long windows)
}

// quotaDebit undoes one debit (a later check of the same call failed).
type quotaDebit struct {
	rule   *quotaRule
	caller string
	window int64
	units  uint64
}

func (d quotaDebit) refund() {
	qr := d.rule
	qr.mu.Lock()
	defer qr.mu.Unlock()
	c := qr.counters[d.caller]
	if c == nil || c.window != d.window {
		return
	}
	if c.used >= d.units {
		c.used -= d.units
	}
}

// debit counts units for caller now. ok false: over the limit, retry
// is the time to the window's end. n is the live peer count;
// clustered selects the cluster counter for long windows.
func (qr *quotaRule) debit(caller string, units uint64, now time.Time, n int, clustered bool) (quotaDebit, time.Duration, bool) {
	qr.mu.Lock()
	defer qr.mu.Unlock()
	perMs := qr.per.Milliseconds()
	if perMs <= 0 {
		return quotaDebit{}, 0, true
	}
	ms := now.UnixMilli()
	w := ms - ms%perMs
	c := qr.counters[caller]
	if c == nil {
		c = &quotaCounter{window: w}
		qr.counters[caller] = c
	}
	if c.window != w {
		*c = quotaCounter{window: w}
	}
	retry := time.Duration(w+perMs-ms) * time.Millisecond
	var over bool
	if clustered && qr.per >= shortWindow {
		over = c.known+c.used+units > qr.limit
	} else {
		lim := qr.limit
		if clustered && n > 1 {
			lim = qr.limit / uint64(n)
			if lim < 1 {
				lim = 1
			}
		}
		over = c.used+units > lim
	}
	if over {
		return quotaDebit{}, retry, false
	}
	c.used += units
	return quotaDebit{rule: qr, caller: caller, window: w, units: units}, 0, true
}

// pendingFlush is one long-window counter's unflushed count.
type pendingFlush struct {
	rule   *quotaRule
	caller string
	window int64
	delta  uint64
}

// takePending returns the long-window counters of the current window
// (each one adds its unflushed count, or reads the total when there
// is none), and drops counters of past windows. The delta stays in
// used until finishFlush, so checks keep counting it.
func (qr *quotaRule) takePending(now time.Time) []pendingFlush {
	qr.mu.Lock()
	defer qr.mu.Unlock()
	if qr.per < shortWindow {
		// Short windows never flush; drop counters of past windows.
		perMs := qr.per.Milliseconds()
		ms := now.UnixMilli()
		w := ms - ms%perMs
		for k, c := range qr.counters {
			if c.window != w {
				delete(qr.counters, k)
			}
		}
		return nil
	}
	perMs := qr.per.Milliseconds()
	ms := now.UnixMilli()
	w := ms - ms%perMs
	var out []pendingFlush
	for caller, c := range qr.counters {
		if c.window != w {
			delete(qr.counters, caller)
			continue
		}
		out = append(out, pendingFlush{rule: qr, caller: caller, window: w, delta: c.used})
	}
	return out
}

// finishFlush applies a flush's result: delta is now in the cluster
// total, which is total.
func (qr *quotaRule) finishFlush(p pendingFlush, total uint64, ok bool) {
	qr.mu.Lock()
	defer qr.mu.Unlock()
	c := qr.counters[p.caller]
	if c == nil || c.window != p.window {
		return
	}
	if ok {
		if c.used >= p.delta {
			c.used -= p.delta
		} else {
			c.used = 0
		}
		c.known = total
	}
}

// counterKey is the KV key of a long-window counter.
func (p pendingFlush) counterKey() string {
	return "q." + hashKey(p.rule.id) + "." + hashKey(p.caller) + "." + strconv.FormatInt(p.window, 10)
}
