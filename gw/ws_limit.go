package gateway

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// WSLimitOptions configures WithWSLimit. All caps are scoped per
// remote peer IP (r.RemoteAddr with port stripped). Operators
// terminating WebSockets behind a reverse proxy / load balancer see
// the proxy address as the peer; in that posture the proxy is
// typically the right rate-limit boundary anyway, but
// TrustedIPs lets operators carve out their own proxy address so
// the cap applies elsewhere.
//
// Zero-valued fields mean "no cap" (consistent with WithUploadLimit).
//
// Stability: stable
type WSLimitOptions struct {
	// MaxPerIP caps concurrent graphql-transport-ws connections from
	// a single peer IP. 0 = unlimited.
	MaxPerIP int

	// RatePerSec is the per-IP token-bucket refill rate for
	// WebSocket Upgrade requests. 0 = unlimited.
	RatePerSec int

	// Burst is the per-IP token-bucket capacity. 0 defaults to
	// RatePerSec (one second's worth of headroom).
	Burst int

	// TrustedIPs lists peer IPs that bypass both caps. Plain IP
	// strings ("127.0.0.1", "::1") — CIDR support is not part of
	// v1. Use this for cluster-internal traffic or for a reverse
	// proxy that already enforces its own limits.
	TrustedIPs []string
}

// WithWSLimit installs per-peer caps on the GraphQL WebSocket
// subscription upgrade path. MaxPerIP bounds concurrent connections;
// RatePerSec / Burst bound the upgrade rate. Operators behind a
// reverse proxy / CDN that already terminates WebSockets can leave
// this unset; operators running gwag at the edge should enable it as
// the minimum DoS guard on Upgrade — see docs/operations.md.
//
// Stability: stable
func WithWSLimit(opts WSLimitOptions) Option {
	return func(cfg *config) { cfg.wsLimit = opts }
}

// wsLimiter enforces WSLimitOptions. Constructed lazily in New() when
// any cap is non-zero; nil otherwise, so the hot path's nil check is
// the fast path.
type wsLimiter struct {
	maxPerIP int
	rate     float64
	burst    float64
	trusted  map[string]struct{}

	mu      sync.Mutex
	counts  map[string]int
	buckets map[string]*tokenBucket

	now func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newWSLimiter(opts WSLimitOptions) *wsLimiter {
	if opts.MaxPerIP <= 0 && opts.RatePerSec <= 0 {
		return nil
	}
	burst := float64(opts.Burst)
	if burst <= 0 {
		burst = float64(opts.RatePerSec)
	}
	trusted := make(map[string]struct{}, len(opts.TrustedIPs))
	for _, ip := range opts.TrustedIPs {
		trusted[ip] = struct{}{}
	}
	return &wsLimiter{
		maxPerIP: opts.MaxPerIP,
		rate:     float64(opts.RatePerSec),
		burst:    burst,
		trusted:  trusted,
		counts:   map[string]int{},
		buckets:  map[string]*tokenBucket{},
		now:      time.Now,
	}
}

// peerIP returns r.RemoteAddr with the port stripped. Empty string
// when RemoteAddr is unparseable (httptest never sets that path, but
// in-process callers might) — the limiter treats empty as "skip".
func peerIP(r *http.Request) string {
	if r == nil || r.RemoteAddr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// acquire admits a WebSocket upgrade for r or returns ok=false with a
// reason label suitable for metric / 429 body. When ok=true, the
// caller must call release exactly once when the connection ends.
//
// reasons: "max_per_ip", "rate_limit". "" when ok=true.
func (l *wsLimiter) acquire(r *http.Request) (release func(), reason string, ok bool) {
	if l == nil {
		return func() {}, "", true
	}
	ip := peerIP(r)
	if ip == "" {
		return func() {}, "", true
	}
	if _, isTrusted := l.trusted[ip]; isTrusted {
		return func() {}, "", true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Rate limit first — cheaper to bail without touching the count
	// map, and a flooding caller never grows counts past zero.
	if l.rate > 0 {
		now := l.now()
		b, exists := l.buckets[ip]
		if !exists {
			b = &tokenBucket{tokens: l.burst, last: now}
			l.buckets[ip] = b
		} else {
			elapsed := now.Sub(b.last).Seconds()
			if elapsed > 0 {
				b.tokens += elapsed * l.rate
				if b.tokens > l.burst {
					b.tokens = l.burst
				}
				b.last = now
			}
		}
		if b.tokens < 1 {
			return nil, "rate_limit", false
		}
		b.tokens--
	}

	if l.maxPerIP > 0 {
		if l.counts[ip] >= l.maxPerIP {
			// Refund the token we just spent — the operator's intent
			// is "if we'll reject anyway, don't dock the bucket too".
			if b, ok := l.buckets[ip]; ok {
				b.tokens++
				if b.tokens > l.burst {
					b.tokens = l.burst
				}
			}
			return nil, "max_per_ip", false
		}
		l.counts[ip]++
	}

	release = func() {
		if l.maxPerIP <= 0 {
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.counts[ip]--
		if l.counts[ip] <= 0 {
			delete(l.counts, ip)
		}
	}
	return release, "", true
}
