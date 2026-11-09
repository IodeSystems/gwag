package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/iodesystems/gwag/v2/gw/profile"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Instance verification.
//
// A binding that declares ServiceBinding.verify is probed by the node
// that holds its registration (the owner), per instance:
//
//  1. the registered address accepts a TCP connection, and the health
//     URL (when declared) answers 200;
//  2. the spec served at spec_url equals the registered spec in
//     canonical form (profile.Canonical; deprecation marks ignored).
//     GraphQL bindings are introspected again instead.
//
// The start gate (start_gate.go) is the third check: declared depends
// present, subscribed channels declared.
//
// States (cpv1.VerificationState): PENDING before the first probe,
// GREEN when every check passed on the latest probe, RED otherwise.
//
// Routing. An instance that never was GREEN has not passed the gate:
// it is WAITING and joins no pool, exactly like a binding whose deps
// are missing (old clients wait for it the same way). An instance
// that was GREEN and turns RED stays in its pool with its replicas
// flagged (replicaHealth.red): never picked, not counted as ready by
// the start gate or drain coverage. It is reported WAITING with
// verification RED and the reasons. Other green instances of the line
// keep serving. A RED instance is probed again every verifyRetry and
// turns GREEN (routable) on the first passing probe.
//
// Cluster: the owner writes the verification result into the
// registry value (Verification / VerifyReasons). Every node's
// reconciler applies the red flag to its own replicas of the
// instance. A gateway older than verification ignores the fields: it
// routes to a red instance that was green before.
//
// Probes go only to the registered address: a health or spec URL is
// a path on it, or an absolute URL with the same host (Register
// refuses another host). Redirects are not followed.

// VerifyOptions tunes instance verification (WithVerification).
//
// Stability: experimental
type VerifyOptions struct {
	// Interval between probes of a green instance. 0 → 30s.
	Interval time.Duration
	// Retry is the interval between probes of an instance that is
	// not green. 0 → min(Interval, 2s).
	Retry time.Duration
	// Timeout bounds one probe (all checks of one instance). 0 → 5s.
	Timeout time.Duration
	// ProfileHosts, when set, is the allow-list of hosts a pulled
	// profile URL (AddServiceProfile) may name; an entry "*.internal"
	// matches any subdomain of internal. Empty → any host. The
	// pulled service's own address must then be on the profile URL's
	// host too (profile.Resolve), so the list bounds every request a
	// pulled profile makes the gateway send.
	ProfileHosts []string
	// HTTPClient sends the probes. nil → a client with Timeout and no
	// redirects. A client given here should refuse redirects too
	// (profile.NoRedirects).
	HTTPClient *http.Client
}

func (o VerifyOptions) interval() time.Duration {
	if o.Interval > 0 {
		return o.Interval
	}
	return 30 * time.Second
}

func (o VerifyOptions) retry() time.Duration {
	if o.Retry > 0 {
		return o.Retry
	}
	if i := o.interval(); i < 2*time.Second {
		return i
	}
	return 2 * time.Second
}

func (o VerifyOptions) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return 5 * time.Second
}

func (g *Gateway) verifyClient() *http.Client {
	if c := g.cfg.verify.HTTPClient; c != nil {
		return c
	}
	g.verifyHTTPOnce.Do(func() {
		g.verifyHTTP = &http.Client{Timeout: g.cfg.verify.timeout(), CheckRedirect: profile.NoRedirects}
	})
	return g.verifyHTTP
}

// verifySpec is a binding's resolved verify block.
type verifySpec struct {
	addr      string // registered address (dial target)
	healthURL string // absolute, "" = none
	specURL   string // absolute, "" = none
}

// verifyStatus is the latest result for one instance.
type verifyStatus struct {
	state   cpv1.VerificationState
	reasons []string
	atMs    int64
}

func (s verifyStatus) green() bool {
	return s.state == cpv1.VerificationState_VERIFICATION_STATE_GREEN
}

func (s verifyStatus) equal(o verifyStatus) bool {
	if s.state != o.state || len(s.reasons) != len(o.reasons) {
		return false
	}
	for i := range s.reasons {
		if s.reasons[i] != o.reasons[i] {
			return false
		}
	}
	return true
}

// verifyStateName is the JSON / admin name of a state.
func verifyStateName(s cpv1.VerificationState) string {
	switch s {
	case cpv1.VerificationState_VERIFICATION_STATE_PENDING:
		return "pending"
	case cpv1.VerificationState_VERIFICATION_STATE_GREEN:
		return "green"
	case cpv1.VerificationState_VERIFICATION_STATE_RED:
		return "red"
	}
	return ""
}

func verifyStateFromName(s string) cpv1.VerificationState {
	switch s {
	case "pending":
		return cpv1.VerificationState_VERIFICATION_STATE_PENDING
	case "green":
		return cpv1.VerificationState_VERIFICATION_STATE_GREEN
	case "red":
		return cpv1.VerificationState_VERIFICATION_STATE_RED
	}
	return cpv1.VerificationState_VERIFICATION_STATE_UNSPECIFIED
}

// prepVerify resolves b.verify against the registered address. nil
// when the binding declares none. A URL on another host is refused.
func prepVerify(b *cpv1.ServiceBinding, addr string) (*verifySpec, error) {
	v := b.GetVerify()
	if v == nil || (v.GetHealthUrl() == "" && v.GetSpecUrl() == "") {
		return nil, nil
	}
	target := addr
	if ep := b.GetGraphqlEndpoint(); ep != "" {
		target = ep
	}
	vs := &verifySpec{addr: target}
	var err error
	if h := v.GetHealthUrl(); h != "" {
		if vs.healthURL, err = profile.Resolve(target, h); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "controlplane: verify.health_url: %v", err)
		}
	}
	if s := v.GetSpecUrl(); s != "" && b.GetGraphqlEndpoint() == "" {
		if vs.specURL, err = profile.Resolve(target, s); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "controlplane: verify.spec_url: %v", err)
		}
	}
	return vs, nil
}

// probe runs every check of p's verify block once and returns the
// result. Takes no lock.
func (g *Gateway) probe(ctx context.Context, p *preparedBinding) verifyStatus {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.verify.timeout())
	defer cancel()
	vs := p.verify
	var reasons []string
	if err := profile.Reachable(ctx, vs.addr); err != nil {
		reasons = append(reasons, err.Error())
	} else {
		c := g.verifyClient()
		if vs.healthURL != "" {
			if err := profile.Health(ctx, c, vs.healthURL); err != nil {
				reasons = append(reasons, err.Error())
			}
		}
		switch {
		case p.isGraphQL:
			raw, err := fetchIntrospectionBytes(ctx, c, p.graphqlEndpoint)
			if err != nil {
				reasons = append(reasons, fmt.Sprintf("introspect %s: %v", p.graphqlEndpoint, err))
			} else if d := introspectionDiff(p.graphqlIntrospection, raw); d != "" {
				reasons = append(reasons, fmt.Sprintf("introspection of %s differs from the registered schema (%s)", p.graphqlEndpoint, d))
			}
		case vs.specURL != "" && p.isOpenAPI:
			if err := profile.SpecMatches(ctx, c, vs.specURL, profile.KindOpenAPI, p.openAPISpec, nil); err != nil {
				reasons = append(reasons, err.Error())
			}
		case vs.specURL != "":
			if err := profile.SpecMatches(ctx, c, vs.specURL, profile.KindProto, p.protoSource, p.protoImports); err != nil {
				reasons = append(reasons, err.Error())
			}
		}
	}
	st := verifyStatus{state: cpv1.VerificationState_VERIFICATION_STATE_GREEN, atMs: time.Now().UnixMilli()}
	if len(reasons) > 0 {
		st.state = cpv1.VerificationState_VERIFICATION_STATE_RED
		st.reasons = reasons
	}
	return st
}

func introspectionDiff(want, got []byte) string {
	a, _, err := profile.Canonical(profile.KindGraphQL, want, nil, "")
	if err != nil {
		return err.Error()
	}
	b, _, err := profile.Canonical(profile.KindGraphQL, got, nil, "")
	if err != nil {
		return err.Error()
	}
	return profile.Diff(a, b)
}

// probeAll probes every binding of prep that declares verify, in
// parallel, and stores the results on prep. Register calls it before
// taking any lock, so the response already says green or red.
func (g *Gateway) probeAll(ctx context.Context, prep []preparedBinding) {
	var wg sync.WaitGroup
	for i := range prep {
		if prep[i].verify == nil {
			continue
		}
		wg.Add(1)
		go func(p *preparedBinding) {
			defer wg.Done()
			p.vstat = g.probe(ctx, p)
		}(&prep[i])
	}
	wg.Wait()
}

// verified reports whether the binding may pass the gate as far as
// verification goes.
func (p *preparedBinding) verified() bool {
	return p.verify == nil || p.vstat.green()
}

// setReplicaRedLocked sets the red flag on every replica regID owns in
// the (namespace, version) slot. Reports whether any flag changed.
// Caller holds g.mu.
func (g *Gateway) setReplicaRedLocked(key poolKey, regID string, red bool) bool {
	s := g.slots[key]
	if s == nil || regID == "" {
		return false
	}
	changed := false
	set := func(owner string, h *replicaHealth) {
		if owner == regID && h.red.Load() != red {
			h.red.Store(red)
			changed = true
		}
	}
	switch {
	case s.proto != nil:
		if rs := s.proto.replicas.Load(); rs != nil {
			for _, r := range *rs {
				set(r.owner, &r.health)
			}
		}
	case s.openapi != nil:
		if rs := s.openapi.replicas.Load(); rs != nil {
			for _, r := range *rs {
				set(r.owner, &r.health)
			}
		}
	case s.graphql != nil:
		if rs := s.graphql.replicas.Load(); rs != nil {
			for _, r := range *rs {
				set(r.owner, &r.health)
			}
		}
	}
	if changed {
		g.publishServiceChange(adminEventsActionRegistered, key.namespace, key.version, "", uint32(g.readyInstancesLocked(key.namespace, key.version)))
	}
	return changed
}

// verifyTick is the period of the verify loop's scan for due probes.
const verifyTick = 250 * time.Millisecond

// verifyLoop probes each local binding that declares verify when it
// is due. Stops with the gateway.
func (cp *controlPlane) verifyLoop() {
	t := time.NewTicker(verifyTick)
	defer t.Stop()
	inflight := map[*regBinding]bool{}
	var mu sync.Mutex
	for {
		select {
		case <-cp.gw.life.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		type job struct {
			reg *registration
			b   *regBinding
		}
		var jobs []job
		cp.mu.Lock()
		for _, reg := range cp.regs {
			for _, b := range reg.bindings {
				if b.prep.verify == nil || now.Before(b.nextProbe) {
					continue
				}
				mu.Lock()
				busy := inflight[b]
				if !busy {
					inflight[b] = true
				}
				mu.Unlock()
				if !busy {
					jobs = append(jobs, job{reg, b})
				}
			}
		}
		cp.mu.Unlock()
		for _, j := range jobs {
			go func(j job) {
				st := cp.gw.probe(cp.gw.life, &j.b.prep)
				cp.applyVerify(j.reg, j.b, st)
				mu.Lock()
				delete(inflight, j.b)
				mu.Unlock()
			}(j)
		}
	}
}

// nextProbeAfter is when a binding with status st is probed again.
func (g *Gateway) nextProbeAfter(st verifyStatus) time.Time {
	if st.green() {
		return time.Now().Add(g.cfg.verify.interval())
	}
	return time.Now().Add(g.cfg.verify.retry())
}

// applyVerify records a probe result for one binding: the gate
// instance, the replica red flag (standalone) or the registry value
// (cluster), and the log line on a change.
func (cp *controlPlane) applyVerify(reg *registration, b *regBinding, st verifyStatus) {
	g := cp.gw
	cp.mu.Lock()
	if cp.regs[reg.id] != reg {
		cp.mu.Unlock()
		return
	}
	b.nextProbe = g.nextProbeAfter(st)
	prev := b.prep.vstat
	b.prep.vstat = st
	changed := !prev.equal(st)
	red := !b.waiting && !st.green()
	g.mu.Lock()
	if reg.kvKeys == nil {
		if inst := g.gate.instances[standaloneGateID(reg.id, b.prep.key())]; inst != nil {
			inst.vstat = st
			inst.red = red
		}
		if !b.waiting {
			g.setReplicaRedLocked(b.prep.key(), reg.id, red)
		}
	}
	g.mu.Unlock()
	b.red = red
	if changed {
		b.val.setVerify(st)
	}
	cp.mu.Unlock()
	if changed {
		cp.logVerify(reg.id, &b.prep, prev, st)
		g.lifecycle.observeVerify(b.prep.namespace, b.prep.version, prev.state, st.state)
		if reg.kvKeys != nil {
			cp.rePutBinding(reg, b)
		}
	}
	if st.green() && b.waiting {
		g.kickGate()
	}
}

func (cp *controlPlane) logVerify(regID string, p *preparedBinding, prev, st verifyStatus) {
	switch {
	case st.green() && prev.state != st.state:
		cp.logf("controlplane: verify: %s/%s (registration %s) green", p.namespace, p.version, regID)
	case !st.green():
		cp.logf("controlplane: verify: %s/%s (registration %s) red: %s", p.namespace, p.version, regID, strings.Join(st.reasons, "; "))
	}
}

// rePutBinding writes b's current registry value (cluster), ordered
// against heartbeat re-Puts by putMu.
func (cp *controlPlane) rePutBinding(reg *registration, b *regBinding) {
	kv := cp.gw.registryKV()
	if kv == nil {
		return
	}
	reg.putMu.Lock()
	defer reg.putMu.Unlock()
	cp.mu.Lock()
	if cp.regs[reg.id] != reg || b.kvIdx < 0 || b.kvIdx >= len(reg.kvKeys) {
		cp.mu.Unlock()
		return
	}
	raw, err := json.Marshal(b.val)
	if err != nil {
		cp.mu.Unlock()
		return
	}
	ref := &reg.kvKeys[b.kvIdx]
	ref.value = raw
	k := replicaKey(ref.namespace, ref.version, ref.replicaID)
	cp.mu.Unlock()
	kctx, cancel := context.WithTimeout(cp.gw.life, 2*kvCallTimeout)
	defer cancel()
	if err := kvPutRetry(kctx, kv, k, raw); err != nil {
		cp.logf("controlplane: verify: put %s: %v", k, err)
	}
}

// setVerify copies st into the registry value.
func (v *registryValue) setVerify(st verifyStatus) {
	v.Verification = verifyStateName(st.state)
	v.VerifyReasons = append([]string(nil), st.reasons...)
	v.VerifiedUnixMs = st.atMs
}

func (v *registryValue) verifyStatus() verifyStatus {
	return verifyStatus{
		state:   verifyStateFromName(v.Verification),
		reasons: append([]string(nil), v.VerifyReasons...),
		atMs:    v.VerifiedUnixMs,
	}
}

// unroutable reports whether a joined replica of this value must not
// be picked: verification declared and not green.
func (v *registryValue) unroutable() bool {
	return v.Verification != "" && v.Verification != "green"
}

// verifyCounts returns, per (namespace, version), how many known
// instances are in each verification state (pending, green, red);
// instances that declare no verify are not counted. For the
// service_verification gauge. Caller holds g.mu.
func (g *Gateway) verifyCountsLocked() map[poolKey]map[string]int {
	out := map[poolKey]map[string]int{}
	for _, inst := range g.gate.instances {
		name := verifyStateName(inst.vstat.state)
		if name == "" {
			continue
		}
		m := out[inst.key]
		if m == nil {
			m = map[string]int{}
			out[inst.key] = m
		}
		m[name]++
	}
	return out
}

// adminVerifyState is the admin API name: "none" when not verified.
func adminVerifyState(s cpv1.VerificationState) string {
	if n := verifyStateName(s); n != "" {
		return n
	}
	return "none"
}
