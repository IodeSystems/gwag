package controlclient

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// State is the start-gate state of one binding.
//
// Stability: stable
type State int

const (
	// StateReady: routable; every dependency has a ready instance on
	// at least one accepted line. Also reported for gateways that
	// predate the start gate.
	StateReady State = iota + 1
	// StateWaiting: not routable; a dependency has no ready
	// instance on any accepted line.
	StateWaiting
	// StateDegraded: was ready and keeps serving, but a dependency
	// has since lost every ready instance.
	StateDegraded
)

func (s State) String() string {
	switch s {
	case StateReady:
		return "ready"
	case StateWaiting:
		return "waiting"
	case StateDegraded:
		return "degraded"
	default:
		return "unknown"
	}
}

// DependencyStatus is one declared dependency and the accepted lines
// that have a ready instance now (PresentLines; empty = missing).
//
// Stability: stable
type DependencyStatus struct {
	Service      string
	Lines        []string
	PresentLines []string
}

// BindingStatus is the gateway's start-gate state of one Service of
// the registration.
//
// Stability: stable
type BindingStatus struct {
	Namespace string
	Version   string
	State     State
	Depends   []DependencyStatus
}

// Bindings returns the start-gate state of each Service, from the
// latest Register or Heartbeat response, in Options.Services order.
// Empty when the gateway predates the start gate.
//
// Stability: stable
func (r *Registration) Bindings() []BindingStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]BindingStatus, len(r.bindings))
	copy(out, r.bindings)
	return out
}

// PresentLines returns the lines of dependency service that have a
// ready instance, as of the latest Register or Heartbeat response,
// over every binding that declares it. The order is the declared
// order of the first binding that lists the line. The service picks
// which line to call; the gateway does not rewrite calls. nil when
// no binding declares service or none of its lines is present.
//
// Stability: stable
func (r *Registration) PresentLines(service string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	for _, b := range r.bindings {
		for _, d := range b.Depends {
			if d.Service != service {
				continue
			}
			for _, l := range d.PresentLines {
				if !seen[l] {
					seen[l] = true
					out = append(out, l)
				}
			}
		}
	}
	return out
}

func bindingsFromProto(in []*cpv1.BindingStatus) []BindingStatus {
	out := make([]BindingStatus, 0, len(in))
	for _, b := range in {
		bs := BindingStatus{Namespace: b.GetNamespace(), Version: b.GetVersion(), State: stateFromProto(b.GetState())}
		for _, d := range b.GetDepends() {
			bs.Depends = append(bs.Depends, DependencyStatus{
				Service:      d.GetService(),
				Lines:        append([]string(nil), d.GetLines()...),
				PresentLines: append([]string(nil), d.GetPresentLines()...),
			})
		}
		out = append(out, bs)
	}
	return out
}

func stateFromProto(s cpv1.InstanceState) State {
	switch s {
	case cpv1.InstanceState_INSTANCE_STATE_WAITING:
		return StateWaiting
	case cpv1.InstanceState_INSTANCE_STATE_DEGRADED:
		return StateDegraded
	default:
		return StateReady
	}
}

func (r *Registration) setBindings(in []*cpv1.BindingStatus) {
	b := bindingsFromProto(in)
	r.mu.Lock()
	r.bindings = b
	r.mu.Unlock()
}

// waiting renders the waiting bindings and their missing deps:
// "orders/v1 needs library(v1|v2)". "" when none waits.
func (r *Registration) waiting() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var parts []string
	for _, b := range r.bindings {
		if b.State != StateWaiting {
			continue
		}
		var miss []string
		for _, d := range b.Depends {
			if len(d.PresentLines) == 0 {
				miss = append(miss, d.Service+"("+strings.Join(d.Lines, "|")+")")
			}
		}
		parts = append(parts, fmt.Sprintf("%s/%s needs %s", b.Namespace, b.Version, strings.Join(miss, ", ")))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

const (
	retryMin = 250 * time.Millisecond
	retryMax = 5 * time.Second

	// waitLogEvery repeats the "waiting for" log line while nothing
	// changes, so a stuck service keeps saying why.
	waitLogEvery = 30 * time.Second
)

// retryable reports whether a Register error means "gateway not
// reachable now" (retry) rather than "request refused" (return).
func retryable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted:
		return true
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > retryMax {
		d = retryMax
	}
	return d
}

// registerRetry runs register until it succeeds, a non-retryable
// error comes back, or ctx ends. With several gateway addresses, an
// unreachable one moves to the next; the backoff sleep comes after
// every address failed once.
func (r *Registration) registerRetry(ctx context.Context) error {
	backoff := retryMin
	n := len(r.gws)
	for attempt := 1; ; attempt++ {
		gw := r.gateway()
		err := r.registerOnce(ctx)
		if err == nil {
			return nil
		}
		if !retryable(err) {
			return err
		}
		if n > 1 {
			r.next(gw)
			if attempt%n != 0 {
				r.logger("controlclient: gateway %s unreachable, trying %s: %v", gw.addr, r.gateway().addr, err)
				continue
			}
		}
		r.logger("controlclient: gateway %s unreachable, retrying in %s: %v", r.addrList(), backoff, err)
		if cerr := sleepCtx(ctx, backoff); cerr != nil {
			return fmt.Errorf("controlclient: gateway %s unreachable: %w (last error: %v)", r.addrList(), cerr, err)
		}
		backoff = nextBackoff(backoff)
	}
}

// registerOnce is one register attempt. With several gateway
// addresses it is bounded by registerTimeout, so a gateway that does
// not answer moves on to the next one.
func (r *Registration) registerOnce(ctx context.Context) error {
	if len(r.gws) > 1 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, registerTimeout)
		defer cancel()
	}
	return r.register(ctx)
}

// next moves the registration's gateway to the address after gw.
func (r *Registration) next(gw *gatewayConn) {
	idx := (r.indexOf(gw) + 1) % len(r.gws)
	r.mu.Lock()
	r.cur = idx
	r.mu.Unlock()
}

// addrList names the gateway addresses for log lines.
func (r *Registration) addrList() string {
	addrs := make([]string, len(r.gws))
	for i, g := range r.gws {
		addrs[i] = g.addr
	}
	return strings.Join(addrs, ",")
}

// waitReady polls Heartbeat until no binding is WAITING. Heartbeat
// also keeps the registration alive meanwhile. An eviction re-
// registers; transport errors back off and retry.
func (r *Registration) waitReady(ctx context.Context) error {
	poll := r.opts.ReadyPollInterval
	if poll <= 0 {
		poll = time.Second
	}
	var lastLog string
	var lastLogAt time.Time
	backoff := retryMin
	for {
		w := r.waiting()
		if w == "" {
			if lastLog != "" {
				r.logger("controlclient: dependencies present, starting")
			}
			return nil
		}
		if w != lastLog || time.Since(lastLogAt) >= waitLogEvery {
			r.logger("controlclient: waiting for dependencies: %s", w)
			lastLog, lastLogAt = w, time.Now()
		}
		if err := sleepCtx(ctx, poll); err != nil {
			return fmt.Errorf("controlclient: still waiting for dependencies (%s): %w", w, err)
		}
		r.mu.Lock()
		id := r.id
		r.mu.Unlock()
		gw := r.gateway()
		resp, err := gw.client.Heartbeat(ctx, &cpv1.HeartbeatRequest{RegistrationId: id})
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("controlclient: still waiting for dependencies (%s): %w", w, ctx.Err())
			}
			r.logger("controlclient: heartbeat to %s while waiting: %v", gw.addr, err)
			if len(r.gws) > 1 {
				// Register on the next gateway; the one that holds
				// the registration is gone or partitioned.
				r.next(gw)
				if err := r.registerRetry(ctx); err != nil {
					return err
				}
				continue
			}
			if cerr := sleepCtx(ctx, backoff); cerr != nil {
				return fmt.Errorf("controlclient: still waiting for dependencies (%s): %w", w, cerr)
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = retryMin
		if !resp.GetOk() {
			r.logger("controlclient: gateway evicted us while waiting, re-registering")
			if err := r.registerRetry(ctx); err != nil {
				return err
			}
			continue
		}
		r.setBindings(resp.GetBindings())
	}
}

// deregister drops the registration, best effort, with its own short
// deadline (the caller's ctx may be done already).
func (r *Registration) deregister() {
	r.mu.Lock()
	id := r.id
	r.mu.Unlock()
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = r.gateway().client.Deregister(ctx, &cpv1.DeregisterRequest{RegistrationId: id})
}
