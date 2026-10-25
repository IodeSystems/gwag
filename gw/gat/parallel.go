package gat

import (
	"context"
	"fmt"
	"net/http"
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

// MaxOperations caps how many operations one GraphQL request may run,
// aliases and fragments included (n <= 0: no cap). Operations past the
// cap are not dispatched; each fails with a 400 field error, so a
// request aliasing one operation a thousand times runs n of them, not
// a thousand. The count is taken at dispatch — an atomic add in the
// request scope, no parse.
//
// Call before the schema builds. Returns g for chaining.
//
// Stability: experimental
func (g *Gateway) MaxOperations(n int) *Gateway {
	g.maxOps = n
	return g
}

// wrapLimits puts a limitDispatcher in front of every mutation's
// dispatcher, and every query's when ParallelQueries or MaxOperations
// is set. Run by build(), which may run more than once, so
// already-wrapped dispatchers are left alone.
func (g *Gateway) wrapLimits() {
	limits := g.parallel > 0 || g.maxOps > 0
	for _, svc := range g.services {
		for _, op := range svc.FlatOperations() {
			if op.Kind == ir.OpSubscription || (op.Kind == ir.OpQuery && !limits) {
				continue
			}
			d := g.registry.Get(op.SchemaID)
			if d == nil {
				continue
			}
			if _, done := d.(*limitDispatcher); done {
				continue
			}
			ld := &limitDispatcher{inner: d, maxOps: int32(max(g.maxOps, 0))}
			switch op.Kind {
			case ir.OpQuery:
				ld.parallel = int32(max(g.parallel, 0))
			case ir.OpMutation:
				ld.mutation = true
			}
			g.registry.Set(op.SchemaID, ld)
		}
	}
}

// limitDispatcher applies the per-request limits in one hop: the
// operation cap, then (queries only) parallel dispatch. Only the
// GraphQL resolver calls Dispatch on gat's registry, and it hands the
// result straight to the executor — which is what makes returning a
// thunk sound.
type limitDispatcher struct {
	inner    ir.Dispatcher
	maxOps   int32 // 0 = no cap
	parallel int32 // 0 = serial
	mutation bool  // refused over GET
}

func (d *limitDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	s := scopeFrom(ctx)
	if s == nil {
		return d.inner.Dispatch(ctx, args)
	}
	// GraphQL over HTTP forbids mutations on GET. Running them would let
	// any page trigger one with the visitor's cookies (a link or an
	// <img src>; SameSite=Lax still sends cookies on top-level GETs).
	if d.mutation && s.inbound != nil && s.inbound.Method == http.MethodGet {
		return nil, withStatusExtensions(errMutationOverGET)
	}
	if d.maxOps > 0 {
		if n := s.ops.Add(1); n > d.maxOps {
			return nil, withStatusExtensions(&opLimitError{max: d.maxOps})
		}
	}
	if d.parallel == 0 {
		return d.inner.Dispatch(ctx, args)
	}
	if s.inflight.Add(1) > d.parallel {
		s.inflight.Add(-1)
		return d.inner.Dispatch(ctx, args)
	}
	// The executor's two-phase walker calls every resolver of a
	// selection set before awaiting any thunk — func() (interface{},
	// error) — so sibling operations overlap.
	r := &parallelResult{}
	r.wg.Add(1)
	go r.run(ctx, d.inner, args, s)
	return r.await, nil
}

var errMutationOverGET = &httpStatusError{
	status: http.StatusMethodNotAllowed,
	msg:    "gat: mutations are not allowed over GET; use POST",
}

type httpStatusError struct {
	status int
	msg    string
}

func (e *httpStatusError) Error() string  { return e.msg }
func (e *httpStatusError) GetStatus() int { return e.status }

type opLimitError struct{ max int32 }

func (e *opLimitError) Error() string {
	return fmt.Sprintf("gat: request exceeds %d operations", e.max)
}

func (e *opLimitError) GetStatus() int { return http.StatusBadRequest }

// parallelResult is the one allocation a parallel dispatch adds: the
// goroutine writes it, the thunk reads it after wg.Wait.
type parallelResult struct {
	wg  sync.WaitGroup
	val any
	err error
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
