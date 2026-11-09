package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Upstream SLA metrics (default sink only), per upstream attempt:
//
//	upstream_requests_total{namespace,version,method,code}
//	    One per attempt that was sent. code: the upstream's HTTP status
//	    ("200", "429"), the gRPC code name for proto upstreams, "none"
//	    when no response came (connect error, timeout). Unlike
//	    dispatch_duration_seconds_count it leaves out calls the gateway
//	    refused (rate limit, quota, pause, backpressure), counts each
//	    retry, and keeps the HTTP status that the dispatch code folds
//	    (429 and 503 are both a gRPC code there).
//	upstream_ttfb_seconds{namespace,version,method}
//	    HTTP: request written → first response byte (httptrace
//	    GotFirstResponseByte; covers connect when a new connection is
//	    dialed). gRPC: Invoke start → response (header and message
//	    arrive together on a unary call; the gateway does not observe
//	    the header alone). Not recorded without a response.
//	upstream_request_bytes / upstream_response_bytes{namespace,version,method}
//	    Body bytes. HTTP request: Content-Length (streamed multipart
//	    bodies have none and are not recorded). gRPC: protobuf message
//	    size.

type slaMetrics struct {
	requests *prometheus.CounterVec
	ttfb     *prometheus.HistogramVec
	reqSize  *prometheus.HistogramVec
	respSize *prometheus.HistogramVec
}

func newSLAMetrics(reg *prometheus.Registry, p string) *slaMetrics {
	m := &slaMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: p + "_upstream_requests_total",
			Help: "Upstream attempts sent, by the upstream's answer: HTTP status, gRPC code name, or none (no response). Gateway refusals are not counted; retries are.",
		}, []string{"namespace", "version", "method", "code"}),
		ttfb: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    p + "_upstream_ttfb_seconds",
			Help:    "Time to the upstream's first response byte. HTTP: from sending (connect included when dialed); gRPC: from Invoke to the response.",
			Buckets: prometheus.DefBuckets,
		}, []string{"namespace", "version", "method"}),
		reqSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    p + "_upstream_request_bytes",
			Help:    "Upstream request body bytes (HTTP Content-Length; gRPC message size).",
			Buckets: sizeBuckets,
		}, []string{"namespace", "version", "method"}),
		respSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    p + "_upstream_response_bytes",
			Help:    "Upstream response body bytes (HTTP body; gRPC message size).",
			Buckets: sizeBuckets,
		}, []string{"namespace", "version", "method"}),
	}
	reg.MustRegister(m.requests, m.ttfb, m.reqSize, m.respSize)
	return m
}

// upstreamObs collects one HTTP attempt's observations. The dispatch
// functions fill it from ctx (upstreamObsFrom).
type upstreamObs struct {
	start     time.Time
	first     atomic.Int64 // unix nanos of the first response byte
	reqBytes  int64        // -1: unknown
	respBytes int64
	status    int // 0: no response
	trace     httptrace.ClientTrace
}

type upstreamObsKey struct{}

var upstreamObsPool = sync.Pool{New: func() any {
	o := &upstreamObs{}
	o.trace.GotFirstResponseByte = func() { o.first.Store(time.Now().UnixNano()) }
	return o
}}

// startUpstreamObs returns an observation attached to ctx, or nil and
// ctx when the default sink is off.
func (tm *trafficMetrics) startUpstreamObs(ctx context.Context) (*upstreamObs, context.Context) {
	if tm == nil || tm.sla == nil {
		return nil, ctx
	}
	o := upstreamObsPool.Get().(*upstreamObs)
	o.start = time.Time{}
	o.first.Store(0)
	o.reqBytes, o.respBytes, o.status = -1, 0, 0
	ctx = httptrace.WithClientTrace(ctx, &o.trace)
	return o, context.WithValue(ctx, upstreamObsKey{}, o)
}

func upstreamObsFrom(ctx context.Context) *upstreamObs {
	o, _ := ctx.Value(upstreamObsKey{}).(*upstreamObs)
	return o
}

// sent marks the request as about to be written.
func (o *upstreamObs) sent(req *http.Request) {
	if o == nil {
		return
	}
	o.start = time.Now()
	o.reqBytes = req.ContentLength
	if req.Body == nil || req.Body == http.NoBody {
		o.reqBytes = 0
	}
}

func (o *upstreamObs) answered(status int, n int) {
	if o == nil {
		return
	}
	o.status = status
	o.respBytes = int64(n)
}

// finishUpstreamObs records o and returns it to the pool.
func (tm *trafficMetrics) finishUpstreamObs(ns, ver, method string, o *upstreamObs) {
	if o == nil {
		return
	}
	if !o.start.IsZero() {
		m := tm.sla
		code := "none"
		if o.status != 0 {
			code = strconv.Itoa(o.status)
		}
		m.requests.WithLabelValues(ns, ver, method, code).Inc()
		if f := o.first.Load(); f > 0 {
			m.ttfb.WithLabelValues(ns, ver, method).Observe(time.Duration(f - o.start.UnixNano()).Seconds())
		}
		if o.reqBytes >= 0 {
			m.reqSize.WithLabelValues(ns, ver, method).Observe(float64(o.reqBytes))
		}
		if o.status != 0 {
			m.respSize.WithLabelValues(ns, ver, method).Observe(float64(o.respBytes))
		}
	}
	upstreamObsPool.Put(o)
}

// observeGRPC records one proto upstream attempt. respBytes < 0: no
// response message.
func (tm *trafficMetrics) observeGRPC(ns, ver, method string, err error, d time.Duration, reqBytes, respBytes int) {
	if tm == nil || tm.sla == nil {
		return
	}
	m := tm.sla
	code := status.Code(err)
	m.requests.WithLabelValues(ns, ver, method, code.String()).Inc()
	m.reqSize.WithLabelValues(ns, ver, method).Observe(float64(reqBytes))
	if grpcAnswered(err) {
		m.ttfb.WithLabelValues(ns, ver, method).Observe(d.Seconds())
	}
	if respBytes >= 0 {
		m.respSize.WithLabelValues(ns, ver, method).Observe(float64(respBytes))
	}
}

// grpcAnswered: the upstream sent a response (a message, or a status
// of its own). Unavailable, DeadlineExceeded and Canceled usually
// come from the client side.
func grpcAnswered(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return false
	}
	return true
}

// ---- 429 / Retry-After ----------------------------------------------

// parseRetryAfter reads a Retry-After header: delay seconds or an
// HTTP date. 0 when absent or unreadable.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		if n > int64(maxPause429/time.Second) {
			return maxPause429
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// markThrottled marks rej as an upstream 429 with the response's
// Retry-After, which the caller also gets.
func markThrottled(rej *rejection, h http.Header) {
	rej.throttled = true
	if d := parseRetryAfter(h.Get("Retry-After"), time.Now()); d > 0 {
		rej.RetryAfter = d
	}
}

// upstreamThrottled reports whether err is an upstream's "slow down":
// an HTTP 429, or a gRPC ResourceExhausted carrying RetryInfo. d is
// the upstream's retry delay, 0 when it gave none.
func upstreamThrottled(err error) (time.Duration, bool) {
	var rej *rejection
	if errors.As(err, &rej) {
		if rej.throttled {
			return rej.RetryAfter, true
		}
		return 0, false
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		return 0, false
	}
	for _, d := range st.Details() {
		if ri, ok := d.(*errdetails.RetryInfo); ok {
			if rd := ri.GetRetryDelay(); rd != nil && rd.IsValid() {
				return rd.AsDuration(), true
			}
			return 0, true
		}
	}
	return 0, false
}

// ---- limit metrics --------------------------------------------------

// limitMetrics are the rate-limit, quota and pause series (default
// sink only).
type limitMetrics struct {
	rejections *prometheus.CounterVec
	pauses     *prometheus.CounterVec
	wait       *prometheus.HistogramVec
}

func newLimitMetrics(reg *prometheus.Registry, p string, l *upstreamLimits) *limitMetrics {
	m := &limitMetrics{
		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: p + "_limit_rejections_total",
			Help: "Calls the gateway refused before sending them upstream: reason=rate_limit (no token within MaxWaitTime) | quota (built-in quota) | paused (the service answered 429).",
		}, []string{"namespace", "version", "reason"}),
		pauses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: p + "_upstream_pauses_total",
			Help: "Pauses of a service after an upstream 429: origin=local (this node got the 429) | peer (another node's).",
		}, []string{"namespace", "version", "origin"}),
		wait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    p + "_rate_limit_wait_seconds",
			Help:    "Time a call waited for a rate-limit token (calls that did not wait are not recorded).",
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"namespace", "version"}),
	}
	split := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: p + "_rate_limit_split_peers",
		Help: "Peer count this node divides rate limits and short-window quotas by (1 standalone).",
	}, func() float64 { return float64(l.peers.Load()) })
	reg.MustRegister(m.rejections, m.pauses, m.wait, split)
	return m
}

func (m *limitMetrics) reject(s limitScope, reason string) {
	if m != nil {
		m.rejections.WithLabelValues(s.Namespace, s.Version, reason).Inc()
	}
}

func (m *limitMetrics) pause(s limitScope, origin string) {
	if m != nil {
		m.pauses.WithLabelValues(s.Namespace, s.Version, origin).Inc()
	}
}
