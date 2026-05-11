package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
)

// connPool round-robins gRPC dispatches across N grpc.ClientConn
// instances to the same upstream address. Implements
// grpc.ClientConnInterface so call sites (proto_dispatcher.go,
// existing replica.conn field) stay typed against the gRPC API
// without per-call branching.
//
// Why multiple conns to one address: HTTP/2 caps concurrent
// streams per connection (default 100); past that, streams queue
// on the conn's transport-side send mutex. At dispatch saturation
// the profile showed `newClientStreamWithParams` taking ~23% of
// allocated bytes and the transport mutex contention is implicit
// in the queueing. Round-robin across N conns spreads the
// stream-creation load and the transport mutex.
//
// Pool size is chosen at dial time and fixed for the pool's
// lifetime. Size=1 collapses to the original single-conn behavior;
// the pool wrapper still exists in the type chain but adds no
// runtime cost (the fast-path branch checks len once).
type connPool struct {
	conns []*grpc.ClientConn
	// next is the round-robin cursor. Atomic increment + modulo
	// across the conn slice; uniform distribution under contention.
	next atomic.Uint32
}

// dialConnPool dials `size` independent gRPC ClientConn instances
// to `addr` with the provided DialOptions. Returns an error if any
// individual dial fails (already-opened conns are closed before
// returning so the caller doesn't leak file descriptors).
func dialConnPool(addr string, size int, opts ...grpc.DialOption) (*connPool, error) {
	if size <= 0 {
		size = 1
	}
	conns := make([]*grpc.ClientConn, 0, size)
	for i := 0; i < size; i++ {
		c, err := grpc.NewClient(addr, opts...)
		if err != nil {
			for _, prev := range conns {
				_ = prev.Close()
			}
			return nil, fmt.Errorf("dial %s (conn %d/%d): %w", addr, i+1, size, err)
		}
		conns = append(conns, c)
	}
	return &connPool{conns: conns}, nil
}

// pick returns the next ClientConn in round-robin order. Hot path:
// single atomic increment, modulo. Pool of size 1 short-circuits.
func (p *connPool) pick() *grpc.ClientConn {
	if len(p.conns) == 1 {
		return p.conns[0]
	}
	i := p.next.Add(1) - 1
	return p.conns[int(i)%len(p.conns)]
}

// Size reports the configured pool size for tests + diagnostics.
func (p *connPool) Size() int { return len(p.conns) }

// Invoke + NewStream satisfy grpc.ClientConnInterface so the pool
// is a drop-in for replica.conn.
func (p *connPool) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	return p.pick().Invoke(ctx, method, args, reply, opts...)
}

func (p *connPool) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return p.pick().NewStream(ctx, desc, method, opts...)
}

// Close shuts down every underlying conn. Returns the first error
// encountered but continues closing the rest so a partial failure
// doesn't leak file descriptors.
func (p *connPool) Close() error {
	var first error
	for _, c := range p.conns {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// lazyConnPool defers dial-N until first use. Drop-in for the
// previous lazyConn; the `size` field controls fan-out. Boot-time
// AddProto(To("host:port"), ...) paths get the pool benefit without
// needing the option configured eagerly.
type lazyConnPool struct {
	addr string
	size int
	dial func(addr string, size int) (*connPool, error)

	once sync.Once
	pool *connPool
	err  error
}

func (l *lazyConnPool) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	pool, err := l.resolve()
	if err != nil {
		return err
	}
	return pool.Invoke(ctx, method, args, reply, opts...)
}

func (l *lazyConnPool) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	pool, err := l.resolve()
	if err != nil {
		return nil, err
	}
	return pool.NewStream(ctx, desc, method, opts...)
}

func (l *lazyConnPool) resolve() (*connPool, error) {
	l.once.Do(func() {
		if l.dial == nil {
			l.err = errors.New("lazyConnPool: no dialer configured")
			return
		}
		l.pool, l.err = l.dial(l.addr, l.size)
	})
	return l.pool, l.err
}
