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
	// occurred (slot acquired immediately).
	RecordDwell(namespace, version, method string, d time.Duration)

	// RecordBackoff is called when a request is fast-rejected because
	// the pool's queue is saturated. Reason is "queue_full" or
	// "queue_timeout".
	RecordBackoff(namespace, version, method, reason string)

	// SetQueueDepth reflects the current count of requests waiting
	// for a dispatch slot, per pool. Called on enqueue/dequeue.
	SetQueueDepth(namespace, version string, depth int)

	// RecordSubscribeAuth records the outcome of a subscribe-auth
	// attempt. code is the SubscribeAuthCode enum value's String()
	// (e.g. "SUBSCRIBE_AUTH_CODE_OK", "..._SIGNATURE_MISMATCH").
	RecordSubscribeAuth(namespace, version, method, code string)
}

// noopMetrics is the sink used when WithoutMetrics is set.
type noopMetrics struct{}

func (noopMetrics) RecordDispatch(string, string, string, time.Duration, error) {}
func (noopMetrics) RecordDwell(string, string, string, time.Duration)            {}
func (noopMetrics) RecordBackoff(string, string, string, string)                 {}
func (noopMetrics) SetQueueDepth(string, string, int)                            {}
func (noopMetrics) RecordSubscribeAuth(string, string, string, string)           {}

// prometheusMetrics implements Metrics over a Prometheus registry.
// Created by newPrometheusMetrics; the registry is exposed via
// MetricsHandler.
type prometheusMetrics struct {
	registry *prometheus.Registry
	hist     *prometheus.HistogramVec
	dwell    *prometheus.HistogramVec
	backoff  *prometheus.CounterVec
	depth    *prometheus.GaugeVec
	subAuth  *prometheus.CounterVec
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
	}, []string{"namespace", "version", "method"})
	backoff := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "go_api_gateway_pool_backoff_total",
		Help: "Count of dispatches rejected by pool backpressure (queue full or timeout).",
	}, []string{"namespace", "version", "method", "reason"})
	depth := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "go_api_gateway_pool_queue_depth",
		Help: "Current count of dispatches waiting for an in-flight slot.",
	}, []string{"namespace", "version"})
	subAuth := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "go_api_gateway_subscribe_auth_total",
		Help: "Outcomes of subscribe-auth checks (HMAC verify and delegate).",
	}, []string{"namespace", "version", "method", "code"})
	reg.MustRegister(hist, dwell, backoff, depth, subAuth)
	return &prometheusMetrics{
		registry: reg,
		hist:     hist,
		dwell:    dwell,
		backoff:  backoff,
		depth:    depth,
		subAuth:  subAuth,
	}
}

func (m *prometheusMetrics) RecordDispatch(namespace, version, method string, d time.Duration, err error) {
	code := "ok"
	if err != nil {
		code = classifyError(err)
	}
	m.hist.WithLabelValues(namespace, version, method, code).Observe(d.Seconds())
}

func (m *prometheusMetrics) RecordDwell(namespace, version, method string, d time.Duration) {
	m.dwell.WithLabelValues(namespace, version, method).Observe(d.Seconds())
}

func (m *prometheusMetrics) RecordBackoff(namespace, version, method, reason string) {
	m.backoff.WithLabelValues(namespace, version, method, reason).Inc()
}

func (m *prometheusMetrics) SetQueueDepth(namespace, version string, depth int) {
	m.depth.WithLabelValues(namespace, version).Set(float64(depth))
}

func (m *prometheusMetrics) RecordSubscribeAuth(namespace, version, method, code string) {
	m.subAuth.WithLabelValues(namespace, version, method, code).Inc()
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
