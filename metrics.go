package gateway

import (
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc/status"
)

// Metrics is the sink for per-dispatch observations. The default
// implementation (newPrometheusMetrics) records to a histogram exposed
// at MetricsHandler. Plug in a custom impl via WithMetrics for
// integrating with other backends.
type Metrics interface {
	// RecordDispatch is called once per gRPC dispatch with the
	// (namespace, version) of the pool, the gRPC method path, the
	// elapsed duration, and the dispatch error (nil on success).
	RecordDispatch(namespace, version, method string, d time.Duration, err error)

	// RecordDwell is called for every successful slot acquisition
	// with the time spent waiting in the queue. d=0 when no queueing
	// occurred (slot acquired immediately). kind is "unary" or
	// "stream".
	RecordDwell(namespace, version, method, kind string, d time.Duration)

	// RecordBackoff is called when a request is fast-rejected because
	// the pool's queue is saturated. kind is "unary" or "stream".
	// Reason is currently always "wait_timeout".
	RecordBackoff(namespace, version, method, kind, reason string)

	// SetQueueDepth reflects the current count of requests waiting
	// for a dispatch slot, per pool. kind is "unary" or "stream".
	SetQueueDepth(namespace, version, kind string, depth int)

	// SetStreamsInflight reflects the current count of active
	// subscription streams against a pool.
	SetStreamsInflight(namespace, version string, inflight int)

	// SetStreamsInflightTotal reflects the gateway-wide count of
	// active subscription streams across all pools.
	SetStreamsInflightTotal(inflight int)

	// RecordSubscribeAuth records the outcome of a subscribe-auth
	// attempt. code is the SubscribeAuthCode enum value's String()
	// (e.g. "SUBSCRIBE_AUTH_CODE_OK", "..._SIGNATURE_MISMATCH").
	RecordSubscribeAuth(namespace, version, method, code string)

	// RecordAdminAuth records the outcome of an AdminMiddleware
	// auth check. method is the HTTP method (POST/PUT/...). outcome
	// is one of: "ok_delegate" (delegate said OK), "ok_bearer" (boot
	// token matched), "denied_delegate" (delegate said DENIED),
	// "denied_bearer" (no/wrong bearer + no delegate accept),
	// "no_token_configured" (gateway has no boot token).
	RecordAdminAuth(method, outcome string)
}

// noopMetrics is the sink used when WithoutMetrics is set.
type noopMetrics struct{}

func (noopMetrics) RecordDispatch(string, string, string, time.Duration, error) {}
func (noopMetrics) RecordDwell(string, string, string, string, time.Duration)    {}
func (noopMetrics) RecordBackoff(string, string, string, string, string)         {}
func (noopMetrics) SetQueueDepth(string, string, string, int)                    {}
func (noopMetrics) SetStreamsInflight(string, string, int)                       {}
func (noopMetrics) SetStreamsInflightTotal(int)                                  {}
func (noopMetrics) RecordSubscribeAuth(string, string, string, string)           {}
func (noopMetrics) RecordAdminAuth(string, string)                               {}

// prometheusMetrics implements Metrics over a Prometheus registry.
// Created by newPrometheusMetrics; the registry is exposed via
// MetricsHandler.
type prometheusMetrics struct {
	registry     *prometheus.Registry
	hist         *prometheus.HistogramVec
	dwell        *prometheus.HistogramVec
	backoff      *prometheus.CounterVec
	depth        *prometheus.GaugeVec
	streams      *prometheus.GaugeVec
	streamsTotal prometheus.Gauge
	subAuth      *prometheus.CounterVec
	adminAuth    *prometheus.CounterVec
}

func newPrometheusMetrics() *prometheusMetrics {
	reg := prometheus.NewRegistry()
	hist := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "go_api_gateway_dispatch_duration_seconds",
		Help:    "Duration of gRPC dispatches from the GraphQL surface to a backing replica.",
		Buckets: prometheus.DefBuckets,
	}, []string{"namespace", "version", "method", "code"})
	dwell := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "go_api_gateway_pool_queue_dwell_seconds",
		Help: "Time a dispatch waited for an in-flight slot in its pool.",
		// Tighter low-end buckets — well-tuned pools rarely queue.
		Buckets: []float64{0.0001, 0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"namespace", "version", "method", "kind"})
	backoff := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "go_api_gateway_pool_backoff_total",
		Help: "Count of dispatches rejected by pool backpressure (queue full or timeout).",
	}, []string{"namespace", "version", "method", "kind", "reason"})
	depth := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "go_api_gateway_pool_queue_depth",
		Help: "Current count of dispatches waiting for an in-flight slot.",
	}, []string{"namespace", "version", "kind"})
	streams := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "go_api_gateway_pool_streams_inflight",
		Help: "Current count of active subscription streams in a pool.",
	}, []string{"namespace", "version"})
	streamsTotal := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "go_api_gateway_streams_inflight_total",
		Help: "Gateway-wide count of active subscription streams across all pools.",
	})
	subAuth := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "go_api_gateway_subscribe_auth_total",
		Help: "Outcomes of subscribe-auth checks (HMAC verify and delegate).",
	}, []string{"namespace", "version", "method", "code"})
	adminAuth := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "go_api_gateway_admin_auth_total",
		Help: "Outcomes of AdminMiddleware auth checks (delegate + boot-token bearer).",
	}, []string{"method", "outcome"})
	reg.MustRegister(hist, dwell, backoff, depth, streams, streamsTotal, subAuth, adminAuth)
	return &prometheusMetrics{
		registry:     reg,
		hist:         hist,
		dwell:        dwell,
		backoff:      backoff,
		depth:        depth,
		streams:      streams,
		streamsTotal: streamsTotal,
		subAuth:      subAuth,
		adminAuth:    adminAuth,
	}
}

func (m *prometheusMetrics) RecordDispatch(namespace, version, method string, d time.Duration, err error) {
	code := "ok"
	if err != nil {
		code = classifyError(err)
	}
	m.hist.WithLabelValues(namespace, version, method, code).Observe(d.Seconds())
}

func (m *prometheusMetrics) RecordDwell(namespace, version, method, kind string, d time.Duration) {
	m.dwell.WithLabelValues(namespace, version, method, kind).Observe(d.Seconds())
}

func (m *prometheusMetrics) RecordBackoff(namespace, version, method, kind, reason string) {
	m.backoff.WithLabelValues(namespace, version, method, kind, reason).Inc()
}

func (m *prometheusMetrics) SetQueueDepth(namespace, version, kind string, depth int) {
	m.depth.WithLabelValues(namespace, version, kind).Set(float64(depth))
}

func (m *prometheusMetrics) SetStreamsInflight(namespace, version string, inflight int) {
	m.streams.WithLabelValues(namespace, version).Set(float64(inflight))
}

func (m *prometheusMetrics) SetStreamsInflightTotal(inflight int) {
	m.streamsTotal.Set(float64(inflight))
}

func (m *prometheusMetrics) RecordSubscribeAuth(namespace, version, method, code string) {
	m.subAuth.WithLabelValues(namespace, version, method, code).Inc()
}

func (m *prometheusMetrics) RecordAdminAuth(method, outcome string) {
	m.adminAuth.WithLabelValues(method, outcome).Inc()
}

// classifyError maps an error to a stable label value. gRPC status
// codes win when present; otherwise we fall back to gateway-internal
// rejection codes; otherwise "internal".
func classifyError(err error) string {
	if err == nil {
		return "ok"
	}
	if s, ok := status.FromError(err); ok {
		return s.Code().String()
	}
	var rej *rejection
	if errors.As(err, &rej) {
		return rej.Code.String()
	}
	return "internal"
}

// MetricsHandler returns an http.Handler serving the Prometheus
// scrape endpoint. Returns http.NotFoundHandler when metrics are
// disabled (WithoutMetrics) or replaced with a non-Prometheus sink.
func (g *Gateway) MetricsHandler() http.Handler {
	if g.cfg.metrics == nil {
		return http.NotFoundHandler()
	}
	pm, ok := g.cfg.metrics.(*prometheusMetrics)
	if !ok {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(pm.registry, promhttp.HandlerOpts{})
}
