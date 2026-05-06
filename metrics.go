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
}

// noopMetrics is the sink used when WithoutMetrics is set.
type noopMetrics struct{}

func (noopMetrics) RecordDispatch(string, string, string, time.Duration, error) {}

// prometheusMetrics implements Metrics on top of a single histogram
// vector. Created by newPrometheusMetrics; the *prometheus.Registry it
// owns is exposed by MetricsHandler.
type prometheusMetrics struct {
	registry *prometheus.Registry
	hist     *prometheus.HistogramVec
}

func newPrometheusMetrics() *prometheusMetrics {
	reg := prometheus.NewRegistry()
	hist := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "go_api_gateway_dispatch_duration_seconds",
		Help:    "Duration of gRPC dispatches from the GraphQL surface to a backing replica.",
		Buckets: prometheus.DefBuckets,
	}, []string{"namespace", "version", "method", "code"})
	reg.MustRegister(hist)
	return &prometheusMetrics{registry: reg, hist: hist}
}

func (m *prometheusMetrics) RecordDispatch(namespace, version, method string, d time.Duration, err error) {
	code := "ok"
	if err != nil {
		code = classifyError(err)
	}
	m.hist.WithLabelValues(namespace, version, method, code).Observe(d.Seconds())
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
