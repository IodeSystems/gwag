package gat

import (
	"context"
	"fmt"
	"sync"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// ParallelQueries opts this gateway into running the query operations
// of one GraphQL request concurrently, up to n at a time per request
// (n <= 0: off, the default). A fused query whose operations each wait
// on a database then takes as long as the slowest one, not the sum.
//
// Mutations stay serial, as the GraphQL spec requires. gRPC and REST
// calls carry one operation each and are unaffected. Beyond n, further
// operations run inline on the request goroutine — the cap needs no
// extra allocation per request.
//
// Opt-in because handlers, their huma middlewares and the
// ObserveDispatch observer then run concurrently: state they shared
// per request without a lock while dispatch was serial becomes a data
// race. Share per-request state through Attrs / LockIfAbsentAndFill.
//
// Call before the schema builds. Returns g for chaining.
//
// Stability: experimental
func (g *Gateway) ParallelQueries(n int) *Gateway {
	g.parallel = n
	return g
}

// wrapParallel swaps every query operation's dispatcher for a
// parallelDispatcher. Run by build(), which may run more than once, so
// already-wrapped dispatchers are left alone.
func (g *Gateway) wrapParallel() {
	if g.parallel <= 0 {
		return
	}
	for _, svc := range g.services {
		for _, op := range svc.FlatOperations() {
			if op.Kind != ir.OpQuery {
				continue
			}
			d := g.registry.Get(op.SchemaID)
			if d == nil {
				continue
			}
			if _, done := d.(*parallelDispatcher); done {
				continue
			}
			g.registry.Set(op.SchemaID, &parallelDispatcher{inner: d, limit: int32(g.parallel)})
		}
	}
}

// parallelDispatcher runs its inner dispatcher on a goroutine and
// returns a thunk — func() (interface{}, error) — in place of the
// result. The executor's two-phase walker calls every resolver of a
// selection set before awaiting any thunk, so sibling operations
// overlap. Only the GraphQL resolver calls Dispatch on gat's registry,
// and it hands the result straight to the executor, which is what
// makes returning a thunk sound.
type parallelDispatcher struct {
	inner ir.Dispatcher
	limit int32
}

// parallelResult is the one allocation a parallel dispatch adds: the
// goroutine writes it, the thunk reads it after wg.Wait.
type parallelResult struct {
	wg  sync.WaitGroup
	val any
	err error
}

func (d *parallelDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	s := scopeFrom(ctx)
	if s == nil {
		return d.inner.Dispatch(ctx, args)
	}
	if s.inflight.Add(1) > d.limit {
		s.inflight.Add(-1)
		return d.inner.Dispatch(ctx, args)
	}
	r := &parallelResult{}
	r.wg.Add(1)
	go r.run(ctx, d.inner, args, s)
	return r.await, nil
}

func (r *parallelResult) run(ctx context.Context, inner ir.Dispatcher, args map[string]any, s *requestScope) {
	defer func() {
		// A panic here would take the process down: the executor's
		// recover only covers the request goroutine.
		if p := recover(); p != nil {
			r.err = fmt.Errorf("gat: operation panicked: %v", p)
		}
		s.inflight.Add(-1)
		r.wg.Done()
	}()
	r.val, r.err = inner.Dispatch(ctx, args)
}

func (r *parallelResult) await() (interface{}, error) {
	r.wg.Wait()
	return r.val, r.err
}
