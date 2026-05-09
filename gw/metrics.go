package gateway

import (
	"context"
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
	// RecordDispatch is called once per dispatch (gRPC pool, OpenAPI
	// source, or downstream-GraphQL source) with the (namespace,
	// version) — version is "v1" for OpenAPI / GraphQL sources which
	// have no version axis — the method label (gRPC method path,
	// "<HTTP_METHOD> <pathTemplate>" for OpenAPI, or
	// "<query|mutation> <fieldName>" for downstream GraphQL), the
	// elapsed duration, and the dispatch error (nil on success). ctx
	// carries the inbound request so impls can look up caller-id via
	// callerFromContext (plan §5).
	RecordDispatch(ctx context.Context, namespace, version, method string, d time.Duration, err error)

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

	// RecordSignAuth records the outcome of the SignSubscriptionToken
	// bearer check. code is one of: "in_process" (no gRPC peer; gate
	// bypassed), "ok_signer" / "ok_bearer" (signer-secret or admin
	// token matched), "denied_bearer" (bearer present but neither
	// matched), "missing_bearer" (no Authorization metadata),
	// "no_token_configured" (gateway has neither signer-secret nor
	// admin token — shouldn't happen in normal use).
	RecordSignAuth(code string)

	// RecordAdminAuth records the outcome of an AdminMiddleware
	// auth check. method is the HTTP method (POST/PUT/...). outcome
	// is one of: "ok_delegate" (delegate said OK), "ok_bearer" (boot
	// token matched), "denied_delegate" (delegate said DENIED),
	// "denied_bearer" (no/wrong bearer + no delegate accept),
	// "no_token_configured" (gateway has no boot token).
	RecordAdminAuth(method, outcome string)

	// RecordGraphQLSubFanout records a downstream-GraphQL
	// subscription fanout transition. event is "open" (first local
	// subscriber for an (operation, variables) tuple — broker created
	// a new upstream subscription) or "close" (last local consumer
	// left, upstream completed/errored, or broker tore down).
	// Operation hash is intentionally not surfaced as a label —
	// cardinality is namespace-only.
	RecordGraphQLSubFanout(namespace, event string)

	// SetGraphQLSubFanoutsActive reflects the current count of active
	// downstream-GraphQL subscription fanouts (distinct upstream
	// subscriptions) for a source.
	SetGraphQLSubFanoutsActive(namespace string, active int)

	// RecordRequest is called once per inbound request, after the
	// response has been written, with the wall-clock total and the
	// gateway's self-time (total minus the per-request dispatch
	// accumulator). ingress identifies the entry point: "graphql"
	// (gw.Handler()), "http" (IngressHandler), "grpc"
	// (GRPCUnknownHandler). No namespace label — one request can fan
	// out to many — see plan §3.
	RecordRequest(ingress string, total, self time.Duration)
}

// noopMetrics is the sink used when WithoutMetrics is set.
type noopMetrics struct{}

func (noopMetrics) RecordDispatch(context.Context, string, string, string, time.Duration, error) {
}
func (noopMetrics) RecordDwell(string, string, string, string, time.Duration)   {}
func (noopMetrics) RecordBackoff(string, string, string, string, string)        {}
func (noopMetrics) SetQueueDepth(string, string, string, int)                   {}
func (noopMetrics) SetStreamsInflight(string, string, int)                      {}
func (noopMetrics) SetStreamsInflightTotal(int)                                 {}
func (noopMetrics) RecordSubscribeAuth(string, string, string, string)          {}
func (noopMetrics) RecordSignAuth(string)                                       {}
func (noopMetrics) RecordAdminAuth(string, string)                              {}
func (noopMetrics) RecordGraphQLSubFanout(string, string)                       {}
func (noopMetrics) SetGraphQLSubFanoutsActive(string, int)                      {}
func (noopMetrics) RecordRequest(string, time.Duration, time.Duration)          {}

// prometheusMetrics implements Metrics over a Prometheus registry.
// Created by newPrometheusMetrics; the registry is exposed via
// MetricsHandler.
type prometheusMetrics struct {
	registry      *prometheus.Registry
	hist          *prometheus.HistogramVec
	dwell         *prometheus.HistogramVec
	backoff       *prometheus.CounterVec
	depth         *prometheus.GaugeVec
	streams       *prometheus.GaugeVec
	streamsTotal  prometheus.Gauge
	subAuth       *prometheus.CounterVec
	signAuth      *prometheus.CounterVec
	adminAuth     *prometheus.CounterVec
	gqlSubFanout  *prometheus.CounterVec
	gqlSubActive  *prometheus.GaugeVec
	reqDuration   *prometheus.HistogramVec
	reqSelf       *prometheus.HistogramVec
	callerHeaders []string
}

func newPrometheusMetrics() *prometheusMetrics {
	reg := prometheus.NewRegistry()
	hist := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "go_api_gateway_dispatch_duration_seconds",
		Help:    "Duration of dispatches (gRPC pools, OpenAPI sources, downstream-GraphQL sources) from the GraphQL surface to a backing replica. caller is extracted via WithCallerHeaders; defaults to \"unknown\".",
		Buckets: prometheus.DefBuckets,
	}, []string{"namespace", "version", "method", "code", "caller"})
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
	signAuth := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "go_api_gateway_sign_auth_total",
		Help: "Outcomes of SignSubscriptionToken bearer checks (signer-secret + admin-token gate).",
	}, []string{"code"})
	adminAuth := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "go_api_gateway_admin_auth_total",
		Help: "Outcomes of AdminMiddleware auth checks (delegate + boot-token bearer).",
	}, []string{"method", "outcome"})
	gqlSubFanout := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "go_api_gateway_graphql_sub_fanout_total",
		Help: "Downstream-GraphQL subscription fanout open/close events. One open per upstream subscribe; one close when the last local consumer leaves.",
	}, []string{"namespace", "event"})
	gqlSubActive := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "go_api_gateway_graphql_sub_fanouts_active",
		Help: "Current count of active downstream-GraphQL subscription fanouts (distinct upstream subscriptions) per source.",
	}, []string{"namespace"})
	reqDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "go_api_gateway_request_duration_seconds",
		Help:    "Wall-clock duration of inbound requests, by ingress entry point (graphql, http, grpc).",
		Buckets: prometheus.DefBuckets,
	}, []string{"ingress"})
	reqSelf := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "go_api_gateway_request_self_seconds",
		Help:    "Per-request gateway self-time: wall-clock total minus the per-request dispatch accumulator. Pair with request_duration_seconds for the upstream slice.",
		Buckets: prometheus.DefBuckets,
	}, []string{"ingress"})
	reg.MustRegister(hist, dwell, backoff, depth, streams, streamsTotal, subAuth, signAuth, adminAuth, gqlSubFanout, gqlSubActive, reqDuration, reqSelf)
	return &prometheusMetrics{
		registry:     reg,
		hist:         hist,
		dwell:        dwell,
		backoff:      backoff,
		depth:        depth,
		streams:      streams,
		streamsTotal: streamsTotal,
		subAuth:      subAuth,
		signAuth:     signAuth,
		adminAuth:    adminAuth,
		gqlSubFanout: gqlSubFanout,
		gqlSubActive: gqlSubActive,
		reqDuration:  reqDuration,
		reqSelf:      reqSelf,
	}
}

func (m *prometheusMetrics) RecordDispatch(ctx context.Context, namespace, version, method string, d time.Duration, err error) {
	code := "ok"
	if err != nil {
		code = classifyError(err)
	}
	m.hist.WithLabelValues(namespace, version, method, code, callerFromContext(ctx, m.callerHeaders)).Observe(d.Seconds())
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

func (m *prometheusMetrics) RecordSignAuth(code string) {
	m.signAuth.WithLabelValues(code).Inc()
}

func (m *prometheusMetrics) RecordAdminAuth(method, outcome string) {
	m.adminAuth.WithLabelValues(method, outcome).Inc()
}

func (m *prometheusMetrics) RecordGraphQLSubFanout(namespace, event string) {
	m.gqlSubFanout.WithLabelValues(namespace, event).Inc()
}

func (m *prometheusMetrics) SetGraphQLSubFanoutsActive(namespace string, active int) {
	m.gqlSubActive.WithLabelValues(namespace).Set(float64(active))
}

func (m *prometheusMetrics) RecordRequest(ingress string, total, self time.Duration) {
	m.reqDuration.WithLabelValues(ingress).Observe(total.Seconds())
	if self < 0 {
		// Clock skew or accumulator races on extremely short requests
		// can momentarily push self negative; clamp so the histogram
		// stays interpretable.
		self = 0
	}
	m.reqSelf.WithLabelValues(ingress).Observe(self.Seconds())
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
//
// Unwraps statsRecordingMetrics — cfg.metrics is wrapped at New() so
// every RecordDispatch also feeds the in-process stats registry; the
// Prometheus registry itself sits on the underlying impl.
func (g *Gateway) MetricsHandler() http.Handler {
	if g.cfg.metrics == nil {
		return http.NotFoundHandler()
	}
	m := g.cfg.metrics
	if w, ok := m.(*statsRecordingMetrics); ok {
		m = w.Metrics
	}
	pm, ok := m.(*prometheusMetrics)
	if !ok {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(pm.registry, promhttp.HandlerOpts{})
}
