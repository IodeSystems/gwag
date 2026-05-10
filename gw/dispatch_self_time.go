package gateway

import (
	"context"
	"sync/atomic"
	"time"
)

// dispatchAccumulatorKey carries a per-request *dispatchAccumulator
// (Sum of dispatch-time nanoseconds + Count of dispatcher calls).
// Total request wall time minus Sum is the gateway's self-time;
// Count drives the optional WithRequestLog JSON output. Each
// dispatcher (proto / openapi / graphql) calls addDispatchTime which
// updates both atomically; the ingress layer installs the
// accumulator with withDispatchAccumulator and reads it on response
// completion.
type dispatchAccumulatorKey struct{}

// dispatchAccumulator pairs the per-request dispatch-time sum with a
// per-request dispatch count. Both fields are bumped atomically by
// addDispatchTime; ingress layers read both at response completion.
type dispatchAccumulator struct {
	Sum   atomic.Int64
	Count atomic.Int64
}

// withDispatchAccumulator installs a fresh accumulator on ctx and
// returns the new ctx alongside a pointer for the ingress layer to
// read on response completion. Safe to call once per request; nested
// installs replace the inner accumulator (so a single request never
// double-counts).
func withDispatchAccumulator(ctx context.Context) (context.Context, *dispatchAccumulator) {
	accum := &dispatchAccumulator{}
	return context.WithValue(ctx, dispatchAccumulatorKey{}, accum), accum
}

// dispatchAccumulatorFromContext returns the per-request accumulator
// or nil if none has been installed (callers below ingress, in-
// process tests, control-plane RPCs).
func dispatchAccumulatorFromContext(ctx context.Context) *dispatchAccumulator {
	v, _ := ctx.Value(dispatchAccumulatorKey{}).(*dispatchAccumulator)
	return v
}

// addDispatchTime adds d to the per-request dispatch-time accumulator
// in ctx and bumps the dispatch count by one. No-op when no
// accumulator is installed. Called from every dispatcher next to its
// RecordDispatch so the eventual request_self_seconds histogram
// shares its denominator with dispatch_duration_seconds and the
// optional WithRequestLog JSON line carries an accurate count.
func addDispatchTime(ctx context.Context, d time.Duration) {
	if accum := dispatchAccumulatorFromContext(ctx); accum != nil {
		accum.Sum.Add(int64(d))
		accum.Count.Add(1)
	}
}
