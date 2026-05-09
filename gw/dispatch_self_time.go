package gateway

import (
	"context"
	"sync/atomic"
	"time"
)

// dispatchAccumulatorKey carries a per-request *atomic.Int64 of
// nanoseconds spent inside Dispatch implementations. Total request
// wall time minus this value is the gateway's self-time. Each
// dispatcher (proto / openapi / graphql) adds the same duration it
// already feeds RecordDispatch; the ingress layer installs the
// accumulator with withDispatchAccumulator and reads it on response
// completion.
type dispatchAccumulatorKey struct{}

// withDispatchAccumulator installs a fresh *atomic.Int64 on ctx and
// returns the new ctx alongside a pointer for the ingress layer to
// read on response completion. Safe to call once per request; nested
// installs replace the inner accumulator (so a single request never
// double-counts).
func withDispatchAccumulator(ctx context.Context) (context.Context, *atomic.Int64) {
	accum := new(atomic.Int64)
	return context.WithValue(ctx, dispatchAccumulatorKey{}, accum), accum
}

// dispatchAccumulatorFromContext returns the per-request accumulator
// or nil if none has been installed (callers below ingress, in-
// process tests, control-plane RPCs).
func dispatchAccumulatorFromContext(ctx context.Context) *atomic.Int64 {
	v, _ := ctx.Value(dispatchAccumulatorKey{}).(*atomic.Int64)
	return v
}

// addDispatchTime adds d to the per-request dispatch-time accumulator
// in ctx. No-op when no accumulator is installed. Called from every
// dispatcher next to its RecordDispatch so the eventual
// request_self_seconds histogram shares its denominator with
// dispatch_duration_seconds.
func addDispatchTime(ctx context.Context, d time.Duration) {
	if accum := dispatchAccumulatorFromContext(ctx); accum != nil {
		accum.Add(int64(d))
	}
}
