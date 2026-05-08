package gateway

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// BackpressureConfig is the per-dispatcher knob bag for
// BackpressureMiddleware. One BackpressureConfig describes one
// (pool / source, operation) coordinate: Sem and Queueing live on
// the pool / source struct (so least-loaded picks across replicas
// share state); Namespace/Version/Label go into metric labels.
//
// Sem == nil means unbounded (MaxInflight=0) → the middleware is a
// no-op pass-through. When Sem != nil, Queueing must be non-nil
// and Metrics must be set (use noopMetrics if needed).
type BackpressureConfig struct {
	Sem         chan struct{}
	Queueing    *atomic.Int32
	MaxWaitTime time.Duration
	Metrics     Metrics

	Namespace string
	Version   string
	Label     string // method label for metrics; format-specific (e.g. proto path, HTTP "GET /foo")
	Kind      string // metric kind label; today always "unary" — streams have their own path
}

// BackpressureMiddleware returns the middleware that wraps an
// ir.Dispatcher with the slot-acquisition + queue-depth + dwell-
// metric prologue currently duplicated inline in three places
// (gw/schema.go buildPoolMethodField, gw/openapi.go field resolver,
// gw/graphql_mirror.go forwarding resolver).
//
// Behavior on slot acquisition timeout: returns Reject with
// CodeResourceExhausted. The resolver boundary in gw/ keeps
// translating that to the GraphQL error envelope as it does today;
// the cutover (steps 3+) replaces the inline prologues but doesn't
// change observable behavior.
func BackpressureMiddleware(cfg BackpressureConfig) ir.DispatcherMiddleware {
	return func(next ir.Dispatcher) ir.Dispatcher {
		if cfg.Sem == nil {
			return next
		}
		return ir.DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
			release, err := acquireBackpressureSlot(ctx, cfg)
			if err != nil {
				return nil, err
			}
			defer release()
			return next.Dispatch(ctx, args)
		})
	}
}

