package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// Upstream limits: gwag as the single way out to a rate-limited API.
//
//   - rate limit: a token bucket per (namespace, version, operation)
//     scope, set in code (RateLimit, OperationRateLimit) or by an
//     operator (admin API, `gwag limit set`). In a cluster each node
//     gets rps / live peers and burst / live peers, recomputed when
//     the live peer count changes. A call waits for a token up to the
//     backpressure MaxWaitTime, then fails ResourceExhausted (HTTP
//     429) with a Retry-After from the bucket.
//   - upstream 429: an HTTP upstream answering 429, or a gRPC upstream
//     answering ResourceExhausted with RetryInfo, pauses the service
//     (namespace, version) on every node for its Retry-After. Calls
//     during the pause fail 429 with the remaining time.
//   - quotas: per caller-id limits over a window (upstream_quota.go).
//   - costs: units an operation counts against quotas and in usage.
//   - usage: per (caller, namespace, version, operation, day) counts
//     (upstream_usage.go).
//
// The gate runs in the caller's goroutine on state that belongs to
// one operation or one service: a limited or paused upstream never
// delays a call to another service.

// limitScope names what a limit covers. Version "" covers every
// version of the namespace together; Operation "" every operation.
// Namespace "" (quotas only) covers every service.
type limitScope struct {
	Namespace string
	Version   string
	Operation string
}

func (s limitScope) String() string {
	out := s.Namespace
	if out == "" {
		out = "*"
	}
	if s.Version != "" {
		out += ":" + s.Version
	}
	if s.Operation != "" {
		out += " " + s.Operation
	}
	return out
}

// limitRecord kinds.
const (
	limitKindRate  = "rate"
	limitKindQuota = "quota"
	limitKindCost  = "cost"
)

// limitRecord is one rate limit, quota or cost, as stored in the
// gwag-limits bucket (or in memory standalone) and as set in code.
type limitRecord struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Version   string `json:"version,omitempty"`
	Operation string `json:"operation,omitempty"`
	// Caller is the caller id of a quota; empty with Default.
	Caller  string `json:"caller,omitempty"`
	Default bool   `json:"default,omitempty"`

	RPS   float64 `json:"rps,omitempty"`
	Burst int     `json:"burst,omitempty"`

	Limit uint64 `json:"limit,omitempty"`
	PerMs int64  `json:"perMs,omitempty"`
	Units bool   `json:"units,omitempty"`

	Cost uint32 `json:"cost,omitempty"`

	SetBy     string `json:"setBy,omitempty"`
	SetUnixMs int64  `json:"setUnixMs,omitempty"`
}

func (r limitRecord) scope() limitScope {
	return limitScope{Namespace: r.Namespace, Version: r.Version, Operation: r.Operation}
}

// key is the record's KV key: kind plus the base64url of its scope
// (and caller for a quota), so any caller id fits the key charset.
func (r limitRecord) key() string {
	id := r.Namespace + "\x00" + r.Version + "\x00" + r.Operation
	if r.Kind == limitKindQuota {
		if r.Default {
			id += "\x00\x01"
		} else {
			id += "\x00" + r.Caller
		}
	}
	return r.Kind + "." + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func (r limitRecord) per() time.Duration { return time.Duration(r.PerMs) * time.Millisecond }

// validate checks a record before it is stored.
func (r limitRecord) validate() error {
	if r.Operation != "" && r.Namespace == "" {
		return errors.New("an operation needs a service")
	}
	if r.Version != "" && r.Namespace == "" {
		return errors.New("a version needs a service")
	}
	if r.Version != "" {
		if _, _, err := parseVersion(r.Version); err != nil {
			return err
		}
	}
	switch r.Kind {
	case limitKindRate:
		if r.Namespace == "" {
			return errors.New("a rate limit needs a service")
		}
		if !(r.RPS > 0) || math.IsInf(r.RPS, 0) {
			return errors.New("rps must be positive")
		}
		if r.Burst < 1 {
			return errors.New("burst must be at least 1")
		}
	case limitKindQuota:
		if r.Default == (r.Caller != "") {
			return errors.New("a quota needs exactly one of caller or default")
		}
		if r.Limit == 0 {
			return errors.New("limit must be positive")
		}
		if p := r.per(); p < time.Second || p > maxQuotaWindow {
			return fmt.Errorf("per must be between 1s and %s", maxQuotaWindow)
		}
	case limitKindCost:
		if r.Namespace == "" || r.Operation == "" {
			return errors.New("a cost needs a service and an operation")
		}
		if r.Cost == 0 {
			return errors.New("cost must be at least 1")
		}
	default:
		return fmt.Errorf("unknown kind %q", r.Kind)
	}
	return nil
}

// maxQuotaWindow bounds a quota's window; the quota counters bucket
// keeps a window's counter for twice this long.
const maxQuotaWindow = 24 * time.Hour

// RateLimit caps calls to this service at rps per second with burst,
// cluster-wide: each node gets rps and burst divided by the live peer
// count. Callers over the limit wait for a token up to the
// backpressure MaxWaitTime, then fail ResourceExhausted (HTTP 429 with
// Retry-After). An operator limit for the same scope (`gwag limit
// set`) replaces this one. Applies to AddProto*, AddOpenAPI*,
// AddGraphQL and AddMCP. See docs/quota.md.
//
// Stability: experimental
func RateLimit(rps float64, burst int) ServiceOption {
	return func(c *serviceConfig) {
		c.limits = append(c.limits, limitRecord{Kind: limitKindRate, RPS: rps, Burst: burst})
	}
}

// OperationRateLimit is RateLimit for one operation (OpenAPI
// operationId, proto method name, GraphQL field, MCP tool name). It
// applies in addition to a service RateLimit: a call takes a token
// from both.
//
// Stability: experimental
func OperationRateLimit(op string, rps float64, burst int) ServiceOption {
	return func(c *serviceConfig) {
		c.limits = append(c.limits, limitRecord{Kind: limitKindRate, Operation: op, RPS: rps, Burst: burst})
	}
}

// OperationCost sets the units one call to op counts against quotas
// that count units (`gwag quota set --units`) and in the usage
// report. Default 1. An operator cost for the same operation (`gwag
// cost set`) replaces this one.
//
// Stability: experimental
func OperationCost(op string, units uint32) ServiceOption {
	return func(c *serviceConfig) {
		c.limits = append(c.limits, limitRecord{Kind: limitKindCost, Operation: op, Cost: units})
	}
}

// WithUpstream429Pause sets how long an upstream that answers 429
// without a Retry-After is paused. Default 1s; 0 pauses only when the
// upstream sends Retry-After. Retry-After values are capped at 1h.
//
// Stability: experimental
func WithUpstream429Pause(d time.Duration) Option {
	return func(cfg *config) {
		if d < 0 {
			d = 0
		}
		cfg.pause429Default = d
		cfg.pause429Set = true
	}
}

const (
	defaultPause429 = time.Second
	maxPause429     = time.Hour
)

// ---- state ----------------------------------------------------------

// upstreamLimits is the gateway's limits, quotas, costs, pauses and
// usage. mu guards the maps; the hot path reads only a gate's own
// atomics and its buckets.
type upstreamLimits struct {
	g *Gateway

	mu      sync.Mutex
	code    map[string]limitRecord // set in code, by record key
	admin   map[string]limitRecord // set by operators, by record key
	gates   map[limitScope]*opGate // one per (ns, ver, op) dispatcher
	buckets map[limitScope]*rateBucket
	rules   map[string]*quotaRule // by record key
	pauses  map[poolKey]*pauseState

	// peers is the live peer count the split uses (>= 1).
	peers atomic.Int32

	// quotaOff: WithQuota (the _quota_auth delegate) is configured,
	// which replaces built-in quotas.
	quotaOff bool

	pause429 time.Duration

	usage *usageRecorder // nil: usage off

	cl atomic.Pointer[limitsCluster] // nil standalone or before the cluster bound

	flushMu sync.Mutex

	metrics *limitMetrics // nil without the default sink
}

func newUpstreamLimits(g *Gateway) *upstreamLimits {
	l := &upstreamLimits{
		g:        g,
		code:     map[string]limitRecord{},
		admin:    map[string]limitRecord{},
		gates:    map[limitScope]*opGate{},
		buckets:  map[limitScope]*rateBucket{},
		rules:    map[string]*quotaRule{},
		pauses:   map[poolKey]*pauseState{},
		quotaOff: g.cfg.quota != nil,
		pause429: defaultPause429,
	}
	if g.cfg.pause429Set {
		l.pause429 = g.cfg.pause429Default
	}
	l.peers.Store(1)
	if g.cfg.usage != nil {
		l.usage = newUsageRecorder(l, *g.cfg.usage)
	}
	return l
}

// setCode records the limits and costs a registration declared for
// (ns, ver). A later registration of the same service replaces them.
func (l *upstreamLimits) setCode(ns, ver string, recs []limitRecord) error {
	if len(recs) == 0 {
		// A replica registered without limit options keeps the
		// limits an earlier registration declared.
		return nil
	}
	for i := range recs {
		recs[i].Namespace, recs[i].Version = ns, ver
		if err := recs[i].validate(); err != nil {
			return fmt.Errorf("%s %s: %w", recs[i].Kind, recs[i].scope(), err)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	for k, r := range l.code {
		if r.Namespace == ns && r.Version == ver {
			delete(l.code, k)
			changed = true
		}
	}
	for _, r := range recs {
		l.code[r.key()] = r
		changed = true
	}
	if changed {
		l.recompileLocked()
	}
	return nil
}

// setAdmin replaces the operator records (from the KV watch, or the
// standalone store) and recompiles.
func (l *upstreamLimits) setAdmin(recs map[string]limitRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.admin = recs
	l.recompileLocked()
}

// adminRecords returns a copy of the operator records.
func (l *upstreamLimits) adminRecords() map[string]limitRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]limitRecord, len(l.admin))
	for k, v := range l.admin {
		out[k] = v
	}
	return out
}

// effectiveLocked is code overlaid by admin, by record key.
func (l *upstreamLimits) effectiveLocked() map[string]limitRecord {
	out := make(map[string]limitRecord, len(l.code)+len(l.admin))
	for k, v := range l.code {
		out[k] = v
	}
	for k, v := range l.admin {
		out[k] = v
	}
	return out
}

// gate returns the gate for one dispatcher, creating it on first use.
// Gates outlive schema rebuilds so buckets, counters and usage carry
// over.
func (l *upstreamLimits) gate(ns, ver, op string) *opGate {
	if l == nil {
		return nil
	}
	s := limitScope{Namespace: ns, Version: ver, Operation: op}
	l.mu.Lock()
	defer l.mu.Unlock()
	if gt, ok := l.gates[s]; ok {
		return gt
	}
	gt := &opGate{l: l, scope: s, pause: l.pauseLocked(poolKey{namespace: ns, version: ver})}
	if l.metrics != nil {
		gt.waitObs = l.metrics.wait.WithLabelValues(ns, ver)
	}
	l.gates[s] = gt
	l.compileGateLocked(gt, l.effectiveLocked())
	return gt
}

func (l *upstreamLimits) pauseLocked(k poolKey) *pauseState {
	p, ok := l.pauses[k]
	if !ok {
		p = &pauseState{}
		l.pauses[k] = p
	}
	return p
}

// recompileLocked rebuilds every gate's configuration.
func (l *upstreamLimits) recompileLocked() {
	eff := l.effectiveLocked()
	// Buckets: create or update the ones a record names; drop the rest.
	want := map[limitScope]limitRecord{}
	for _, r := range eff {
		if r.Kind == limitKindRate {
			want[r.scope()] = r
		}
	}
	n := int(l.peers.Load())
	for s, r := range want {
		b, ok := l.buckets[s]
		if !ok {
			b = newRateBucket(s)
			l.buckets[s] = b
		}
		b.set(rateSpec{rps: r.RPS, burst: r.Burst}, n)
	}
	for s := range l.buckets {
		if _, ok := want[s]; !ok {
			delete(l.buckets, s)
		}
	}
	// Quota rules: keep counters of a rule whose key is unchanged.
	keep := map[string]bool{}
	for k, r := range eff {
		if r.Kind != limitKindQuota {
			continue
		}
		keep[k] = true
		qr, ok := l.rules[k]
		if !ok {
			qr = newQuotaRule(k, r)
			l.rules[k] = qr
		} else {
			qr.update(r)
		}
	}
	for k := range l.rules {
		if !keep[k] {
			delete(l.rules, k)
		}
	}
	for _, gt := range l.gates {
		l.compileGateLocked(gt, eff)
	}
}

// rateScopes are the scopes whose rate limits apply to (ns, ver, op),
// broadest first.
func rateScopes(s limitScope) []limitScope {
	return []limitScope{
		{Namespace: s.Namespace},
		{Namespace: s.Namespace, Version: s.Version},
		{Namespace: s.Namespace, Operation: s.Operation},
		{Namespace: s.Namespace, Version: s.Version, Operation: s.Operation},
	}
}

// quotaScopes adds the every-service scope.
func quotaScopes(s limitScope) []limitScope {
	return append([]limitScope{{}}, rateScopes(s)...)
}

func (l *upstreamLimits) compileGateLocked(gt *opGate, eff map[string]limitRecord) {
	cfg := &gateCfg{cost: 1}
	seen := map[limitScope]bool{}
	for _, s := range rateScopes(gt.scope) {
		if seen[s] {
			continue
		}
		seen[s] = true
		if b, ok := l.buckets[s]; ok {
			cfg.rate = append(cfg.rate, b)
		}
	}
	// Cost: the most specific record wins.
	for _, s := range []limitScope{
		{Namespace: gt.scope.Namespace, Version: gt.scope.Version, Operation: gt.scope.Operation},
		{Namespace: gt.scope.Namespace, Operation: gt.scope.Operation},
	} {
		if r, ok := eff[limitRecord{Kind: limitKindCost, Namespace: s.Namespace, Version: s.Version, Operation: s.Operation}.key()]; ok {
			cfg.cost = r.Cost
			break
		}
	}
	if !l.quotaOff {
		seen = map[limitScope]bool{}
		for _, s := range quotaScopes(gt.scope) {
			if seen[s] {
				continue
			}
			seen[s] = true
			qs := &quotaScope{}
			for _, qr := range l.rules {
				if qr.scope != s {
					continue
				}
				if qr.caller == "" {
					qs.def = qr
				} else {
					if qs.byCaller == nil {
						qs.byCaller = map[string]*quotaRule{}
					}
					qs.byCaller[qr.caller] = qr
				}
			}
			if qs.def != nil || qs.byCaller != nil {
				cfg.quotas = append(cfg.quotas, qs)
			}
		}
	}
	// An upstream 429 pauses the service only when it has a rate
	// limit: the limit marks it as an upstream whose limit the
	// gateway must respect. Other services keep relaying 429 to the
	// caller, which suits a per-user 429 of an internal service.
	for s := range l.buckets {
		if s.Namespace == gt.scope.Namespace && (s.Version == "" || s.Version == gt.scope.Version) {
			cfg.honor429 = true
			break
		}
	}
	cfg.active = len(cfg.rate) > 0 || len(cfg.quotas) > 0 || l.usage != nil
	gt.cfg.Store(cfg)
}

// setPeers applies a new live peer count to every bucket.
func (l *upstreamLimits) setPeers(n int) {
	if n < 1 {
		n = 1
	}
	if int(l.peers.Swap(int32(n))) == n {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range l.buckets {
		b.reapply(n)
	}
}

// pausedFor reports the remaining pause of (ns, ver), 0 when none.
func (l *upstreamLimits) pausedFor(ns, ver string) time.Duration {
	l.mu.Lock()
	p := l.pauses[poolKey{namespace: ns, version: ver}]
	l.mu.Unlock()
	if p == nil {
		return 0
	}
	return p.remaining(time.Now())
}

// applyPause pauses (ns, ver) for d from now (never shortens a pause
// in force). Returns whether the pause was extended.
func (l *upstreamLimits) applyPause(ns, ver string, d time.Duration) bool {
	if d <= 0 {
		return false
	}
	if d > maxPause429 {
		d = maxPause429
	}
	l.mu.Lock()
	p := l.pauseLocked(poolKey{namespace: ns, version: ver})
	l.mu.Unlock()
	return p.extend(time.Now().Add(d))
}

// ---- rate buckets ---------------------------------------------------

type rateSpec struct {
	rps   float64
	burst int
}

// rateBucket is one scope's token bucket on this node.
type rateBucket struct {
	scope limitScope
	mu    sync.Mutex
	spec  rateSpec
	lim   *rate.Limiter
}

func newRateBucket(s limitScope) *rateBucket {
	return &rateBucket{scope: s, lim: rate.NewLimiter(0, 1)}
}

// nodeShare is this node's share of spec with n live peers: rps / n,
// and burst / n rounded down, at least 1.
func (s rateSpec) nodeShare(n int) (rate.Limit, int) {
	if n < 1 {
		n = 1
	}
	b := s.burst / n
	if b < 1 {
		b = 1
	}
	return rate.Limit(s.rps / float64(n)), b
}

func (b *rateBucket) set(spec rateSpec, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	first := b.spec == (rateSpec{})
	b.spec = spec
	r, burst := spec.nodeShare(n)
	if first {
		// Start full, like a fresh limiter.
		b.lim = rate.NewLimiter(r, burst)
		return
	}
	now := time.Now()
	b.lim.SetLimitAt(now, r)
	b.lim.SetBurstAt(now, burst)
}

func (b *rateBucket) reapply(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, burst := b.spec.nodeShare(n)
	now := time.Now()
	b.lim.SetLimitAt(now, r)
	b.lim.SetBurstAt(now, burst)
}

func (b *rateBucket) limiter() *rate.Limiter {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lim
}

// ---- pauses ---------------------------------------------------------

// pauseState is one service's pause: unix nanos until which no call
// is sent, 0 when none was set.
type pauseState struct {
	until atomic.Int64
}

func (p *pauseState) remaining(now time.Time) time.Duration {
	u := p.until.Load()
	if u == 0 {
		return 0
	}
	if d := time.Duration(u - now.UnixNano()); d > 0 {
		return d
	}
	return 0
}

func (p *pauseState) extend(t time.Time) bool {
	n := t.UnixNano()
	for {
		cur := p.until.Load()
		if cur >= n {
			return false
		}
		if p.until.CompareAndSwap(cur, n) {
			return true
		}
	}
}

// ---- the gate -------------------------------------------------------

// gateCfg is a gate's compiled configuration; replaced whole.
type gateCfg struct {
	rate   []*rateBucket
	quotas []*quotaScope
	cost   uint32
	// honor429: an upstream 429 pauses the service.
	honor429 bool
	// active: anything to check or record besides the pause.
	active bool
}

// opGate is the pre-dispatch check of one (namespace, version,
// operation).
type opGate struct {
	l     *upstreamLimits
	scope limitScope
	pause *pauseState
	cfg   atomic.Pointer[gateCfg]
	usage sync.Map // usageKey → *usageCell

	waitObs interface{ Observe(float64) }
}

// gateTicket carries what enter decided to done.
type gateTicket struct {
	cfg    *gateCfg
	caller string
	skip   bool
}

// enter runs the checks before a call goes upstream: pause, quotas,
// rate limit (waiting for a token up to MaxWaitTime). A nil error
// means the call may go; pass the ticket to done with its outcome.
func (gt *opGate) enter(ctx context.Context) (gateTicket, error) {
	cfg := gt.cfg.Load()
	now := time.Now()
	paused := gt.pause.remaining(now)
	if (cfg == nil || !cfg.active) && paused == 0 {
		return gateTicket{skip: true}, nil
	}
	l := gt.l
	t := gateTicket{cfg: cfg}
	if len(cfg.quotas) > 0 || l.usage != nil {
		t.caller = resolveCallerID(ctx, l.g.cfg.callerIDExtractor, l.g.cfg.callerHeaders)
	}
	if paused > 0 {
		l.metrics.reject(gt.scope, "paused")
		gt.record(t.caller, now, usageUpstream429, 0)
		return t, &rejection{
			Code:       CodeResourceExhausted,
			Msg:        fmt.Sprintf("%s/%s: upstream answered 429; paused, retry after %s", gt.scope.Namespace, gt.scope.Version, roundRetry(paused)),
			RetryAfter: roundRetry(paused),
			status:     429,
		}
	}
	var debits []quotaDebit
	if len(cfg.quotas) > 0 {
		n := int(l.peers.Load())
		clustered := l.g.cfg.cluster != nil
		for _, qs := range cfg.quotas {
			qr := qs.ruleFor(t.caller)
			if qr == nil {
				continue
			}
			units := uint64(1)
			if qr.units {
				units = uint64(cfg.cost)
			}
			d, retry, ok := qr.debit(t.caller, units, now, n, clustered)
			if !ok {
				for _, prev := range debits {
					prev.refund()
				}
				l.metrics.reject(gt.scope, "quota")
				gt.record(t.caller, now, usageQuota, 0)
				return t, &rejection{
					Code:       CodeResourceExhausted,
					Msg:        quotaExceededMsg(t.caller, gt.scope, qr),
					RetryAfter: roundRetry(retry),
					status:     429,
				}
			}
			debits = append(debits, d)
		}
	}
	if len(cfg.rate) > 0 {
		if err := gt.takeTokens(ctx, cfg.rate, now); err != nil {
			for _, d := range debits {
				d.refund()
			}
			var rej *rejection
			if errors.As(err, &rej) {
				l.metrics.reject(gt.scope, "rate_limit")
				gt.record(t.caller, now, usageLimited, 0)
			}
			return t, err
		}
	}
	return t, nil
}

// takeTokens reserves one token from every bucket and waits for the
// latest. A wait past MaxWaitTime cancels the reservations and fails
// with the wait as Retry-After.
func (gt *opGate) takeTokens(ctx context.Context, buckets []*rateBucket, now time.Time) error {
	maxWait := gt.l.g.cfg.backpressure.MaxWaitTime
	var res [4]*rate.Reservation
	rs := res[:0]
	var wait time.Duration
	cancel := func() {
		for _, r := range rs {
			r.CancelAt(now)
		}
	}
	for _, b := range buckets {
		r := b.limiter().ReserveN(now, 1)
		if !r.OK() {
			cancel()
			return Reject(CodeResourceExhausted, "rate limit: no token can be granted")
		}
		rs = append(rs, r)
		if d := r.DelayFrom(now); d > wait {
			wait = d
		}
	}
	if wait == 0 {
		return nil
	}
	if wait > maxWait {
		cancel()
		return &rejection{
			Code:       CodeResourceExhausted,
			Msg:        fmt.Sprintf("rate limit: %s: retry after %s", gt.scope, roundRetry(wait)),
			RetryAfter: roundRetry(wait),
			status:     429,
		}
	}
	tm := time.NewTimer(wait)
	defer tm.Stop()
	select {
	case <-tm.C:
		if gt.waitObs != nil {
			gt.waitObs.Observe(wait.Seconds())
		}
		return nil
	case <-ctx.Done():
		for _, r := range rs {
			r.Cancel()
		}
		return ctx.Err()
	}
}

// done records a call that passed enter: usage, and a pause when the
// upstream answered 429.
func (gt *opGate) done(t gateTicket, err error) {
	if t.skip && err == nil {
		return
	}
	cfg := t.cfg
	if cfg == nil {
		cfg = gt.cfg.Load()
	}
	kind := usageOK
	if err != nil {
		kind = usageError
		if d, ok := upstreamThrottled(err); ok {
			kind = usageUpstream429
			if cfg != nil && cfg.honor429 {
				gt.upstream429(d)
			}
		}
	}
	if t.skip {
		return
	}
	gt.record(t.caller, time.Now(), kind, cfg.cost)
}

// upstream429 pauses the service after an upstream 429 and tells the
// other nodes.
func (gt *opGate) upstream429(retry time.Duration) {
	l := gt.l
	if retry <= 0 {
		retry = l.pause429
	}
	if retry <= 0 {
		return
	}
	if retry > maxPause429 {
		retry = maxPause429
	}
	if l.applyPause(gt.scope.Namespace, gt.scope.Version, retry) {
		l.metrics.pause(gt.scope, "local")
		if c := l.cl.Load(); c != nil {
			c.publishPause(gt.scope.Namespace, gt.scope.Version, retry)
		}
	}
}

// roundRetry rounds a wait up to whole seconds (Retry-After's unit).
func roundRetry(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	return ((d + time.Second - 1) / time.Second) * time.Second
}

func quotaExceededMsg(caller string, s limitScope, qr *quotaRule) string {
	unit := "calls"
	if qr.units {
		unit = "units"
	}
	return fmt.Sprintf("quota exceeded for caller=%s %s: %d %s per %s", caller, s, qr.limit, unit, formatPer(qr.perDur()))
}

// gateMiddleware wraps a dispatcher with the gate.
func (l *upstreamLimits) gateMiddleware(ns, ver, op string) ir.DispatcherMiddleware {
	gt := l.gate(ns, ver, op)
	return func(next ir.Dispatcher) ir.Dispatcher {
		if gt == nil {
			return next
		}
		if an, ok := next.(ir.AppendDispatcher); ok {
			return &gateAppendWrapper{gt: gt, next: an}
		}
		return ir.DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
			t, err := gt.enter(ctx)
			if err != nil {
				return nil, err
			}
			out, err := next.Dispatch(ctx, args)
			gt.done(t, err)
			return out, err
		})
	}
}

type gateAppendWrapper struct {
	gt   *opGate
	next ir.AppendDispatcher
}

func (w *gateAppendWrapper) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	t, err := w.gt.enter(ctx)
	if err != nil {
		return nil, err
	}
	out, err := w.next.Dispatch(ctx, args)
	w.gt.done(t, err)
	return out, err
}

func (w *gateAppendWrapper) DispatchAppend(ctx context.Context, args map[string]any, dst []byte) ([]byte, error) {
	t, err := w.gt.enter(ctx)
	if err != nil {
		return dst, err
	}
	out, err := w.next.DispatchAppend(ctx, args, dst)
	w.gt.done(t, err)
	return out, err
}

var _ ir.AppendDispatcher = (*gateAppendWrapper)(nil)

// gateHandler wraps a proto Handler (the gRPC ingress route, which
// does not run the dispatcher middlewares) with the gate.
func gateHandler(gt *opGate, next Handler) Handler {
	if gt == nil {
		return next
	}
	return func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
		t, err := gt.enter(ctx)
		if err != nil {
			return nil, err
		}
		resp, err := next(ctx, req)
		gt.done(t, err)
		return resp, err
	}
}

// ---- listing --------------------------------------------------------

// limitView is a record as the admin API lists it.
type limitView struct {
	limitRecord
	Source string `json:"source" doc:"code (a ServiceOption) or admin (set through the admin API; replaces a code value for the same scope)."`
}

// listEffective returns the effective records sorted by kind, scope
// and caller, and the pauses in force. admin are the operator records
// (read from the stream leader); nil uses this node's copy.
func (l *upstreamLimits) listEffective(admin map[string]limitRecord) ([]limitView, []pauseView) {
	l.mu.Lock()
	if admin == nil {
		admin = l.admin
	}
	var out []limitView
	for k, r := range l.code {
		if _, ok := admin[k]; ok {
			continue
		}
		out = append(out, limitView{limitRecord: r, Source: "code"})
	}
	for _, r := range admin {
		out = append(out, limitView{limitRecord: r, Source: "admin"})
	}
	now := time.Now()
	var pauses []pauseView
	for k, p := range l.pauses {
		if d := p.remaining(now); d > 0 {
			pauses = append(pauses, pauseView{Namespace: k.namespace, Version: k.version, RemainingMs: d.Milliseconds(), Until: time.Unix(0, p.until.Load()).UTC().Format(time.RFC3339)})
		}
	}
	l.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.scope() != b.scope() {
			return a.scope().String() < b.scope().String()
		}
		return a.Caller < b.Caller
	})
	sort.Slice(pauses, func(i, j int) bool {
		if pauses[i].Namespace != pauses[j].Namespace {
			return pauses[i].Namespace < pauses[j].Namespace
		}
		return pauses[i].Version < pauses[j].Version
	})
	return out, pauses
}

type pauseView struct {
	Namespace   string `json:"namespace"`
	Version     string `json:"version"`
	RemainingMs int64  `json:"remainingMs"`
	Until       string `json:"until" doc:"RFC 3339."`
}

// hashKey is a short stable id for a string used inside KV keys.
func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:12])
}

// parseServiceArg splits "ns" or "ns:vN".
func parseServiceArg(s string) (ns, ver string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", nil
	}
	ns, ver, _ = strings.Cut(s, ":")
	if err := validateNS(ns); err != nil {
		return "", "", err
	}
	if ver != "" {
		v, _, err := parseVersion(ver)
		if err != nil {
			return "", "", err
		}
		ver = v
	}
	return ns, ver, nil
}

// formatPer prints a quota window the way it is set: 1d, 1h, 1m, 30s.
func formatPer(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	case d >= time.Hour && d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d >= time.Minute && d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return d.String()
}

// parsePer reads a quota window: a Go duration or Nd.
func parsePer(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("per %q: %w", s, err)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
