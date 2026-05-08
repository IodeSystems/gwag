package gateway

import (
	"context"
	"fmt"
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
			waitStart := time.Now()
			select {
			case cfg.Sem <- struct{}{}:
				cfg.Metrics.RecordDwell(cfg.Namespace, cfg.Version, cfg.Label, cfg.Kind, time.Since(waitStart))
			default:
				depth := int(cfg.Queueing.Add(1))
				cfg.Metrics.SetQueueDepth(cfg.Namespace, cfg.Version, cfg.Kind, depth)
				dwell, err := waitForSlot(ctx, cfg.Sem, cfg.MaxWaitTime)
				now := int(cfg.Queueing.Add(-1))
				cfg.Metrics.SetQueueDepth(cfg.Namespace, cfg.Version, cfg.Kind, now)
				cfg.Metrics.RecordDwell(cfg.Namespace, cfg.Version, cfg.Label, cfg.Kind, dwell)
				if err != nil {
					cfg.Metrics.RecordBackoff(cfg.Namespace, cfg.Version, cfg.Label, cfg.Kind, "wait_timeout")
					return nil, Reject(CodeResourceExhausted, fmt.Sprintf("%s/%s: %s", cfg.Namespace, cfg.Version, err.Error()))
				}
			}
			defer func() { <-cfg.Sem }()
			return next.Dispatch(ctx, args)
		})
	}
}
