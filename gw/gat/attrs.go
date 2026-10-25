package gat

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
)

// requestScope is gat's per-inbound-request state: the transport
// request huma middlewares see, the attribute bag, and (GraphQL HTTP
// only) the Set-Cookie sink. It lives inside scopeCtx so one
// allocation carries all three — the context node included.
//
// Not pooled on purpose: a handler may keep ctx past the response
// (`go audit(ctx)`), and a recycled scope would hand that goroutine
// the next client's session.
type requestScope struct {
	// inbound is the request huma middlewares see. Private and separate
	// from WithHTTPRequest on purpose: that key also turns on
	// Authorization forwarding to OpenAPI upstreams, which the
	// middleware runner must not switch on as a side effect.
	inbound *http.Request
	attrs   *Attrs      // own, or an enclosing scope's
	cookies *cookieSink // &sink on transports that emit cookies, else nil
	own     Attrs
	sink    cookieSink
	// cache merges the operations' cache headers (GraphQL HTTP only,
	// alongside the cookie sink).
	cache cacheMerge
	// inflight counts operations of this request running on their own
	// goroutine (ParallelQueries); ops counts operations dispatched
	// (MaxOperations).
	inflight atomic.Int32
	ops      atomic.Int32
}

type scopeKey struct{}

// scopeCtx is a context node that answers scopeKey itself instead of
// wrapping a context.WithValue around a separately allocated scope.
type scopeCtx struct {
	context.Context
	s requestScope
}

func (c *scopeCtx) Value(key any) any {
	if _, ok := key.(scopeKey); ok {
		return &c.s
	}
	return c.Context.Value(key)
}

// newScope opens a request scope under parent. An enclosing scope's
// bag is reused (AttrsMiddleware around the mux, then gat's handler),
// so one inbound request never has two.
func newScope(parent context.Context, inbound *http.Request, cookies bool) (context.Context, *requestScope) {
	c := &scopeCtx{Context: parent}
	c.s.inbound = inbound
	if outer := scopeFrom(parent); outer != nil {
		c.s.attrs = outer.attrs
	} else {
		c.s.attrs = &c.s.own
	}
	if cookies {
		c.s.cookies = &c.s.sink
	}
	return c, &c.s
}

func scopeFrom(ctx context.Context) *requestScope {
	s, _ := ctx.Value(scopeKey{}).(*requestScope)
	return s
}

// Attrs is a request-scoped attribute bag. Every operation of one
// inbound request sees the same bag: all fields of a fused GraphQL
// query, the middleware and handler of one gRPC call, every
// subscription on one graphql-transport-ws connection. A huma
// middleware that resolves a session stores it here once and the
// sibling operations read it instead of resolving it again.
//
// gat scopes its own surfaces (Handler, RegisterGRPC,
// graphql-transport-ws). REST requests reach huma without passing
// through gat, so wrap the mux with AttrsMiddleware to give them a
// bag. An enclosing bag is reused, never replaced, so wrapping the
// whole mux also makes the GraphQL handler share it.
//
// A nil *Attrs (no bag in the context) is usable: nothing is stored
// and LockIfAbsentAndFill calls fill every time.
//
// The first attrInline keys cost no allocation; a wait channel is
// made only when a caller actually has to wait. Concurrent-safe.
//
// Stability: experimental
type Attrs struct {
	mu     sync.Mutex
	inline [attrInline]attrEntry
	n      int
	more   map[any]*attrEntry
}

// attrInline is how many keys Attrs holds without allocating. A
// request typically carries a session, a client id, a counter.
const attrInline = 4

type attrEntry struct {
	key    any
	val    any
	err    error
	filled bool
	// wait is made by the first caller that finds the entry unfilled,
	// and closed by the filler. Nil when nobody waited.
	wait chan struct{}
}

// lookup returns key's entry or nil. a.mu must be held.
func (a *Attrs) lookup(key any) *attrEntry {
	for i := 0; i < a.n; i++ {
		if a.inline[i].key == key {
			return &a.inline[i]
		}
	}
	return a.more[key]
}

// insert adds an unfilled entry for key. a.mu must be held.
func (a *Attrs) insert(key any) *attrEntry {
	if a.n < attrInline {
		e := &a.inline[a.n]
		a.n++
		e.key = key
		return e
	}
	if a.more == nil {
		a.more = make(map[any]*attrEntry)
	}
	e := &attrEntry{key: key}
	a.more[key] = e
	return e
}

// AttrsFrom returns the request's attribute bag, or nil outside a
// request gat (or AttrsMiddleware) scoped.
//
// Stability: experimental
func AttrsFrom(ctx context.Context) *Attrs {
	if s := scopeFrom(ctx); s != nil {
		return s.attrs
	}
	return nil
}

// AttrsMiddleware gives every request through next an attribute bag,
// so REST handlers and their huma middlewares get one too.
//
// Stability: experimental
func AttrsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := newScope(r.Context(), nil, false)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Get returns the value stored under key. A fill still in progress
// reads as absent, and so does a fill that failed.
//
// Stability: experimental
func (a *Attrs) Get(key any) (any, bool) {
	if a == nil {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.lookup(key); e != nil && e.filled && e.err == nil {
		return e.val, true
	}
	return nil, false
}

// PutIfAbsent stores v under key unless the key is already present
// (stored or being filled). It returns the value now under key and
// whether it was already there. A key being filled reports loaded with
// a nil value; use LockIfAbsentAndFill to wait for it instead.
//
// Stability: experimental
func (a *Attrs) PutIfAbsent(key, v any) (actual any, loaded bool) {
	if a == nil {
		return v, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.lookup(key); e != nil {
		return e.val, true
	}
	e := a.insert(key)
	e.val, e.filled = v, true
	return v, false
}

// LockIfAbsentAndFill returns the value under key, computing it with
// fill if absent. The first caller locks the key and runs fill; callers
// that arrive while it runs wait for its result instead of running
// their own. The result is kept for the rest of the request, errors
// included, so every operation sees the same answer (one session
// lookup, one auth failure). A panicking fill is reported to the
// waiters as an error and re-panics in the caller that ran it.
//
// A waiter whose ctx ends stops waiting and returns ctx.Err(); the
// fill carries on for the others. fill must not call
// LockIfAbsentAndFill for its own key: it would wait on itself.
//
// Stability: experimental
func (a *Attrs) LockIfAbsentAndFill(ctx context.Context, key any, fill func() (any, error)) (any, error) {
	if a == nil {
		return fill()
	}
	a.mu.Lock()
	if e := a.lookup(key); e != nil {
		if e.filled {
			v, err := e.val, e.err
			a.mu.Unlock()
			return v, err
		}
		if e.wait == nil {
			e.wait = make(chan struct{})
		}
		wait := e.wait
		a.mu.Unlock()
		select {
		case <-wait:
			a.mu.Lock()
			defer a.mu.Unlock()
			return e.val, e.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	e := a.insert(key)
	a.mu.Unlock()

	var (
		v         any
		err       error
		completed bool
	)
	defer func() {
		a.mu.Lock()
		if !completed {
			err = fmt.Errorf("gat: fill for %v panicked", key)
		}
		e.val, e.err, e.filled = v, err, true
		if e.wait != nil {
			close(e.wait)
		}
		a.mu.Unlock()
	}()
	v, err = fill()
	completed = true
	return v, err
}

// LockIfAbsentAndFill is the typed form of (*Attrs).LockIfAbsentAndFill
// on the request's bag. A value stored under key with a different type
// is an error, not a panic.
//
// Stability: experimental
func LockIfAbsentAndFill[T any](ctx context.Context, key any, fill func() (T, error)) (T, error) {
	v, err := AttrsFrom(ctx).LockIfAbsentAndFill(ctx, key, func() (any, error) { return fill() })
	if err != nil {
		var zero T
		return zero, err
	}
	t, ok := v.(T)
	if !ok && v != nil {
		var zero T
		return zero, fmt.Errorf("gat: attr %v holds %T, not %T", key, v, zero)
	}
	return t, nil
}
