package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Upstream call policy shared by the three unary dispatch kinds
// (proto pool, OpenAPI source, downstream-GraphQL source):
//
//   - timeout: optional per-call deadline (Timeout / WithUpstreamTimeout).
//   - retry: only when the request provably never reached an upstream,
//     and only onto a replica not yet tried this call.
//   - ejection: a replica with ejectAfter consecutive failures is
//     skipped for ejectFor, then given traffic again. When every
//     candidate is ejected the pick fails open and uses one anyway.

const (
	// defaultEjectAfter / defaultEjectFor are the ejection defaults
	// New applies unless WithReplicaEjection overrides them.
	defaultEjectAfter = 5
	defaultEjectFor   = 10 * time.Second

	// maxUpstreamAttempts caps attempts per dispatch (first try plus
	// retries). Each retry also needs a replica not yet tried, so a
	// 2-replica pool makes at most 2 attempts.
	maxUpstreamAttempts = 3
)

// upstreamPolicy is resolved once per pool / source at registration
// (service option → gateway default) and read by every dispatch.
type upstreamPolicy struct {
	timeout    time.Duration // 0 → no per-call deadline
	ejectAfter int32         // 0 → ejection off
	ejectFor   time.Duration
}

// upstreamPolicyFor resolves the per-service timeout against the
// gateway default and attaches the gateway-wide ejection settings.
func (g *Gateway) upstreamPolicyFor(timeout time.Duration) upstreamPolicy {
	if timeout <= 0 {
		timeout = g.cfg.upstreamTimeout
	}
	return upstreamPolicy{
		timeout:    timeout,
		ejectAfter: int32(g.cfg.ejectAfter),
		ejectFor:   g.cfg.ejectFor,
	}
}

// replicaHealth is the per-replica ejection state. Embedded in
// replica, openAPIReplica and graphQLReplica.
type replicaHealth struct {
	fails        atomic.Int32
	ejectedUntil atomic.Int64 // unix nanos; 0 → never ejected / reset
}

// success resets the failure streak. One atomic load on the common
// path (no streak); stores only when there is something to clear.
func (h *replicaHealth) success() {
	if h.fails.Load() != 0 {
		h.fails.Store(0)
		h.ejectedUntil.Store(0)
	}
}

// failure extends the streak and (re-)ejects the replica once the
// streak reaches p.ejectAfter. A replica readmitted after cooldown
// keeps its streak, so its first failure ejects it again.
func (h *replicaHealth) failure(p *upstreamPolicy) {
	if p.ejectAfter <= 0 {
		return
	}
	if h.fails.Add(1) >= p.ejectAfter {
		h.ejectedUntil.Store(time.Now().Add(p.ejectFor).UnixNano())
	}
}

// healthReplica is what pickHealthy needs from a replica type.
type healthReplica interface {
	comparable
	healthState() *replicaHealth
	inflightN() int32
}

// pickHealthy returns the least-loaded replica of rs that is not in
// tried, preferring replicas that are not ejected. The scan starts at
// hint so ties rotate (hint 0 → first-wins). ok is false only when
// every replica is in tried. time.Now is read only when some
// replica carries an ejection timestamp.
func pickHealthy[R healthReplica](rs []R, hint uint64, tried []R) (best R, ok bool) {
	n := len(rs)
	if n == 0 {
		return best, false
	}
	idx := int(hint % uint64(n))
	var now int64
	var bestN int32
	bestEjected := false
	for i := 0; i < n; i++ {
		r := rs[idx]
		idx++
		if idx == n {
			idx = 0
		}
		if len(tried) > 0 && containsReplica(tried, r) {
			continue
		}
		ejected := false
		if until := r.healthState().ejectedUntil.Load(); until != 0 {
			if now == 0 {
				now = time.Now().UnixNano()
			}
			ejected = now < until
		}
		load := r.inflightN()
		switch {
		case !ok:
		case bestEjected && !ejected:
		case ejected == bestEjected && load < bestN:
		default:
			continue
		}
		best, bestN, bestEjected, ok = r, load, ejected, true
	}
	return best, ok
}

func containsReplica[R comparable](rs []R, r R) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

// withUpstreamTimeout derives the per-call context. No allocation
// when the policy has no timeout.
func withUpstreamTimeout(ctx context.Context, p *upstreamPolicy) (context.Context, context.CancelFunc) {
	if p.timeout <= 0 {
		return ctx, noopCancel
	}
	return context.WithTimeout(ctx, p.timeout)
}

func noopCancel() {}

// msDuration converts a wire-format millisecond count.
func msDuration(ms uint32) time.Duration { return time.Duration(ms) * time.Millisecond }

// upstreamTimedOut reports whether callCtx expired because of the
// policy timeout (as opposed to the caller's own ctx ending).
func upstreamTimedOut(parent, callCtx context.Context, p *upstreamPolicy) bool {
	return p.timeout > 0 && parent.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded)
}

// grpcDeadlineSlack is how close to callCtx's deadline a gRPC
// DeadlineExceeded must arrive to count as the policy timeout.
const grpcDeadlineSlack = 20 * time.Millisecond

// grpcUpstreamTimedOut is upstreamTimedOut for a gRPC call. grpc-go
// sends the deadline upstream as grpc-timeout, so the upstream's
// deadline can fire and reset the stream before callCtx's own timer
// does: err is DeadlineExceeded while callCtx.Err() is still nil. A
// DeadlineExceeded well before the deadline is the upstream's own.
func grpcUpstreamTimedOut(parent, callCtx context.Context, p *upstreamPolicy, err error) bool {
	if upstreamTimedOut(parent, callCtx, p) {
		return true
	}
	if p.timeout <= 0 || parent.Err() != nil || status.Code(err) != codes.DeadlineExceeded {
		return false
	}
	deadline, ok := callCtx.Deadline()
	return ok && time.Until(deadline) <= grpcDeadlineSlack
}

// ---------------------------------------------------------------------
// "Never reached the upstream" probes
// ---------------------------------------------------------------------

// grpcPeerProbe detects a gRPC call that never got a transport
// stream. grpc-go fills a grpc.Peer call option only after the
// stream exists (PeerCallOption.after runs only when
// attempt.transportStream != nil), so a failed call whose probe is
// still empty never sent its headers. grpc-go's own transparent
// retry already covers streams refused on the same ClientConn; this
// probe covers moving to a different replica.
//
// Pooled so multi-replica dispatch doesn't allocate a peer.Peer +
// CallOption per call.
type grpcPeerProbe struct {
	p    peer.Peer
	opts []grpc.CallOption
}

var grpcPeerProbePool = sync.Pool{New: func() any {
	pr := &grpcPeerProbe{}
	pr.opts = []grpc.CallOption{grpc.Peer(&pr.p)}
	return pr
}}

func acquireGRPCPeerProbe() *grpcPeerProbe {
	pr := grpcPeerProbePool.Get().(*grpcPeerProbe)
	pr.p = peer.Peer{}
	return pr
}

func releaseGRPCPeerProbe(pr *grpcPeerProbe) {
	pr.p = peer.Peer{}
	grpcPeerProbePool.Put(pr)
}

// grpcNotSent reports whether a failed Invoke provably never left
// the gateway: status Unavailable and no transport stream was made.
func (pr *grpcPeerProbe) grpcNotSent(err error) bool {
	return pr.p.Addr == nil && status.Code(err) == codes.Unavailable
}

// grpcConnReportsPeer reports whether conn forwards CallOptions to a
// real grpc-go ClientConn, so the peer probe is trustworthy. Caller-
// supplied ClientConnInterface fakes or wrappers might drop the
// option; for those the gateway never retries.
func grpcConnReportsPeer(conn grpc.ClientConnInterface) bool {
	switch conn.(type) {
	case *grpc.ClientConn, *connPool, *lazyConnPool, *lazyConn:
		return true
	}
	return false
}

// grpcCountsAsFailure classifies an Invoke error for ejection:
// Unavailable (connect failure, upstream down / draining) counts.
// Policy timeouts are counted by the caller.
func grpcCountsAsFailure(err error) bool {
	return status.Code(err) == codes.Unavailable
}

// httpConnProbe detects an HTTP request that never got a
// connection. httptrace's GotConn fires for every connection the
// transport obtains (new or reused, HTTP/1 and HTTP/2), before any
// request bytes are written. No GotConn → nothing was sent, even if
// the transport retried internally.
type httpConnProbe struct {
	gotConn atomic.Bool
	trace   httptrace.ClientTrace
}

var httpConnProbePool = sync.Pool{New: func() any {
	pr := &httpConnProbe{}
	pr.trace.GotConn = func(httptrace.GotConnInfo) { pr.gotConn.Store(true) }
	return pr
}}

func acquireHTTPConnProbe(ctx context.Context) (*httpConnProbe, context.Context) {
	pr := httpConnProbePool.Get().(*httpConnProbe)
	pr.gotConn.Store(false)
	return pr, httptrace.WithClientTrace(ctx, &pr.trace)
}

func releaseHTTPConnProbe(pr *httpConnProbe) { httpConnProbePool.Put(pr) }

// httpClientTraceable reports whether c sends through net/http's own
// Transport, which fires httptrace hooks. A custom RoundTripper may
// deliver a request without firing GotConn; for those the gateway
// never retries.
func httpClientTraceable(c *http.Client) bool {
	if c == nil || c.Transport == nil {
		return true
	}
	_, ok := c.Transport.(*http.Transport)
	return ok
}

// upstreamClass is how an HTTP dispatch error classifies for
// ejection. Set on the *rejection at the client.Do site.
type upstreamClass uint8

const (
	upstreamClassNone      upstreamClass = iota // not an upstream health signal (4xx, local errors)
	upstreamClassTransport                      // client.Do / body read failed
	upstreamClass5xx                            // upstream answered 5xx
)

// rejectUpstream is Reject plus the ejection class.
func rejectUpstream(class upstreamClass, code Code, msg string) error {
	return &rejection{Code: code, Msg: msg, upstream: class}
}

// rejectUpstreamStatus rejects with an upstream's HTTP error status,
// kept for the gRPC ingress's status → code mapping.
func rejectUpstreamStatus(status int, msg string) error {
	return &rejection{Code: httpStatusToCode(status), Msg: msg, upstream: httpStatusClass(status), status: status}
}

// httpStatusClass: 5xx counts toward ejection, 4xx does not.
func httpStatusClass(status int) upstreamClass {
	if status >= 500 {
		return upstreamClass5xx
	}
	return upstreamClassNone
}

// httpCountsAsFailure reports whether err (from dispatchOpenAPIRaw /
// dispatchGraphQL) is an upstream health failure.
func httpCountsAsFailure(err error) bool {
	var rej *rejection
	if errors.As(err, &rej) {
		return rej.upstream != upstreamClassNone
	}
	return false
}

// httpTransportFailure reports whether err came from client.Do or
// the body read (not an HTTP status).
func httpTransportFailure(err error) bool {
	var rej *rejection
	if errors.As(err, &rej) {
		return rej.upstream == upstreamClassTransport
	}
	return false
}
