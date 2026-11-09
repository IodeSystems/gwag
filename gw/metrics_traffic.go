package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// Operator-facing traffic, rejection, upstream-reliability and
// registry metrics. They live on the default Prometheus sink only:
// the Metrics interface is stable and gains no methods, so a custom
// sink (WithMetrics) sees none of them, and WithoutMetrics allocates
// none (Gateway.tm is nil and every entry point checks it).

// ingressID indexes the per-ingress handles.
type ingressID uint8

const (
	ingGraphQL ingressID = iota
	ingGraphQLWS
	ingHTTP
	ingGRPC
	ingConnect
	ingMCP
	ingressN
)

var ingressNames = [ingressN]string{"graphql", "graphql_ws", "http", "grpc", "connect", "mcp"}

type opType uint8

const (
	opNone opType = iota
	opQuery
	opMutation
	opSubscription
	opN
)

var opNames = [opN]string{"", "query", "mutation", "subscription"}

// The code label is the HTTP status number ("200", "413"), or for a
// gRPC-protocol response the gRPC code name ("OK", "NotFound").
// Handles are cached by index: HTTP statuses 100..599 take 0..499,
// gRPC codes 0..16 take 500..516.
const (
	httpCodeSlots = 500
	grpcCodeSlots = 17
	codeSlots     = httpCodeSlots + grpcCodeSlots
)

var codeLabels [codeSlots]string

func init() {
	for i := 0; i < httpCodeSlots; i++ {
		codeLabels[i] = strconv.Itoa(i + 100)
	}
	for i := 0; i < grpcCodeSlots; i++ {
		codeLabels[httpCodeSlots+i] = codes.Code(i).String()
	}
}

func httpCodeIdx(status int) int {
	if status < 100 || status > 599 {
		return -1
	}
	return status - 100
}

func grpcCodeIdx(c codes.Code) int {
	if int(c) >= grpcCodeSlots {
		return -1
	}
	return httpCodeSlots + int(c)
}

// durTable caches request_duration_seconds observers by (op, code).
type durTable [opN][codeSlots]atomic.Pointer[prometheus.Observer]

var sizeBuckets = prometheus.ExponentialBuckets(64, 4, 12) // 64 B .. 256 MiB

type trafficMetrics struct {
	dur    *prometheus.HistogramVec
	durTab [ingressN]atomic.Pointer[durTable]

	self      [ingressN]prometheus.Observer
	inflight  [ingressN]prometheus.Gauge
	reqSize   [ingressN]prometheus.Observer
	respSize  [ingressN]prometheus.Observer
	rejected  *prometheus.CounterVec
	gqlErr    [3]prometheus.Counter // parse, validate, execution
	partial   prometheus.Counter
	retries   *prometheus.CounterVec
	timeouts  *prometheus.CounterVec
	ejections *prometheus.CounterVec

	rebuilds      *prometheus.CounterVec
	rebuildDur    prometheus.Histogram
	registrations *prometheus.CounterVec

	// sla: per-upstream TTFB, sizes and attempts (upstream_sla.go).
	sla *slaMetrics
}

func newTrafficMetrics(reg *prometheus.Registry, p string, dur, self *prometheus.HistogramVec) *trafficMetrics {
	tm := &trafficMetrics{dur: dur}
	inflight := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: p + "_requests_in_progress",
		Help: "Inbound requests being served now, by ingress. graphql_ws counts open WebSocket connections.",
	}, []string{"ingress"})
	reqSize := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    p + "_request_size_bytes",
		Help:    "Inbound request body bytes read, by ingress (the declared Content-Length when larger, so an oversize refusal shows its size). gRPC: protobuf message bytes. Not recorded for graphql_ws.",
		Buckets: sizeBuckets,
	}, []string{"ingress"})
	respSize := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    p + "_response_size_bytes",
		Help:    "Response body bytes written, counted as they are written, by ingress. gRPC: protobuf message bytes. Not recorded for graphql_ws.",
		Buckets: sizeBuckets,
	}, []string{"ingress"})
	tm.rejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: p + "_requests_rejected_total",
		Help: "Requests refused before execution, by ingress and reason.",
	}, []string{"ingress", "reason"})
	errs := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: p + "_graphql_errors_total",
		Help: "GraphQL operations whose response carried errors, by phase: parse, validate, execution. Refusals by a request limit count in requests_rejected_total instead.",
	}, []string{"kind"})
	tm.partial = prometheus.NewCounter(prometheus.CounterOpts{
		Name: p + "_graphql_partial_responses_total",
		Help: "GraphQL responses that carried both data and errors.",
	})
	tm.retries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: p + "_upstream_retries_total",
		Help: "Calls re-sent to another replica because the first attempt provably never reached an upstream. reason=unavailable (gRPC, no transport stream) | no_connection (HTTP, no connection).",
	}, []string{"namespace", "version", "reason"})
	tm.timeouts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: p + "_upstream_timeouts_total",
		Help: "Upstream calls that ran past the per-service or gateway upstream timeout. Dispatch code cannot tell these apart for OpenAPI and GraphQL sources (both report Internal).",
	}, []string{"namespace", "version"})
	tm.ejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: p + "_replica_ejections_total",
		Help: "Replica ejections: a replica reached the consecutive-failure limit (or failed again after its cooldown) and left rotation.",
	}, []string{"namespace", "version", "replica"})
	tm.rebuilds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: p + "_schema_rebuilds_total",
		Help: "Gateway-wide schema rebuilds, by result (ok | error).",
	}, []string{"result"})
	tm.rebuildDur = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    p + "_schema_rebuild_duration_seconds",
		Help:    "Duration of gateway-wide schema rebuilds.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})
	tm.registrations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: p + "_registrations_total",
		Help: "Service registrations seen by this node, by slot kind (proto | openapi | graphql | mcp | internalproto) and outcome: created, joined (another replica of an identical schema), swapped (unstable slot replaced), rejected.",
	}, []string{"kind", "outcome"})
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: p + "_build_info",
		Help: "Always 1; the labels carry the gateway module version and the Go version it was built with.",
	}, []string{"version", "go_version"})
	version := "devel"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	buildInfo.WithLabelValues(version, runtime.Version()).Set(1)

	for i := ingressID(0); i < ingressN; i++ {
		n := ingressNames[i]
		tm.self[i] = self.WithLabelValues(n)
		tm.inflight[i] = inflight.WithLabelValues(n)
		tm.reqSize[i] = reqSize.WithLabelValues(n)
		tm.respSize[i] = respSize.WithLabelValues(n)
	}
	for i, k := range [3]string{"parse", "validate", "execution"} {
		tm.gqlErr[i] = errs.WithLabelValues(k)
	}
	reg.MustRegister(inflight, reqSize, respSize, tm.rejected, errs, tm.partial, tm.retries,
		tm.timeouts, tm.ejections, tm.rebuilds, tm.rebuildDur, tm.registrations, buildInfo)
	tm.sla = newSLAMetrics(reg, p)
	return tm
}

// observeDur records request_duration_seconds. codeIdx < 0 falls back
// to an uncached label lookup.
func (tm *trafficMetrics) observeDur(ing ingressID, op opType, codeIdx int, d time.Duration) {
	if codeIdx < 0 {
		tm.dur.WithLabelValues(ingressNames[ing], "other", opNames[op]).Observe(d.Seconds())
		return
	}
	tab := tm.durTab[ing].Load()
	if tab == nil {
		tm.durTab[ing].CompareAndSwap(nil, new(durTable))
		tab = tm.durTab[ing].Load()
	}
	slot := &tab[op][codeIdx]
	o := slot.Load()
	if o == nil {
		obs := tm.dur.WithLabelValues(ingressNames[ing], codeLabels[codeIdx], opNames[op])
		slot.Store(&obs)
		o = &obs
	}
	(*o).Observe(d.Seconds())
}

func (tm *trafficMetrics) reject(ing ingressID, reason string) {
	if tm != nil {
		tm.rejected.WithLabelValues(ingressNames[ing], reason).Inc()
	}
}

func (tm *trafficMetrics) retry(ns, ver, reason string) {
	if tm != nil {
		tm.retries.WithLabelValues(ns, ver, reason).Inc()
	}
}

func (tm *trafficMetrics) timeout(ns, ver string) {
	if tm != nil {
		tm.timeouts.WithLabelValues(ns, ver).Inc()
	}
}

func (tm *trafficMetrics) ejection(ns, ver, replica string) {
	if tm != nil {
		tm.ejections.WithLabelValues(ns, ver, replica).Inc()
	}
}

func (tm *trafficMetrics) registration(kind slotKind, outcome string) {
	if tm != nil {
		tm.registrations.WithLabelValues(kind.String(), outcome).Inc()
	}
}

func (tm *trafficMetrics) rebuild(start time.Time, err error) {
	if tm == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	tm.rebuilds.WithLabelValues(result).Inc()
	tm.rebuildDur.Observe(time.Since(start).Seconds())
}

// ---------------------------------------------------------------------
// Per-request record
// ---------------------------------------------------------------------

type recKey struct{}

// reqRec is what one inbound request accumulates for metrics.
type reqRec struct {
	tm      *trafficMetrics
	ingress ingressID
	start   time.Time
	op      opType

	status    int
	declared  int64
	reqBytes  int64
	respBytes int64

	// stream: open-ended (subscription, SSE); no duration recorded.
	stream bool
	// timed: the ingress hook measured total / self (dispatch
	// accounting); otherwise the wrapper's own clock stands in.
	timed       bool
	total, self time.Duration

	// grpc: a gRPC code was seen (native gRPC ingress, or a
	// grpc-status on a Connect-served gRPC response).
	grpc     bool
	grpcCode codes.Code
}

func (rc *reqRec) setTiming(total, self time.Duration) {
	rc.timed, rc.total, rc.self = true, total, self
}

func (rc *reqRec) finish() {
	tm := rc.tm
	tm.inflight[rc.ingress].Dec()
	if rc.ingress == ingGraphQLWS {
		return
	}
	idx := httpCodeIdx(rc.status)
	if rc.grpc {
		idx = grpcCodeIdx(rc.grpcCode)
	}
	if !rc.stream {
		total := rc.total
		if !rc.timed {
			total = time.Since(rc.start)
		}
		tm.observeDur(rc.ingress, rc.op, idx, total)
		if rc.timed {
			self := rc.self
			if self < 0 {
				self = 0
			}
			tm.self[rc.ingress].Observe(self.Seconds())
		}
	}
	rq := rc.reqBytes
	if rc.declared > rq {
		rq = rc.declared
	}
	tm.reqSize[rc.ingress].Observe(float64(rq))
	tm.respSize[rc.ingress].Observe(float64(rc.respBytes))
}

func recFromCtx(ctx context.Context) *reqRec {
	rc, _ := ctx.Value(recKey{}).(*reqRec)
	return rc
}

// recordRequest records a finished request's timing: into its record
// when it has one (the wrapper then adds code and sizes), else to the
// Metrics sink.
func (g *Gateway) recordRequest(rc *reqRec, ingress string, total, self time.Duration) {
	if rc != nil {
		rc.setTiming(total, self)
		return
	}
	g.cfg.metrics.RecordRequest(ingress, total, self)
}

// ---------------------------------------------------------------------
// HTTP wrapper
// ---------------------------------------------------------------------

type countingBody struct {
	rc io.ReadCloser
	n  int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.n += int64(n)
	return n, err
}

func (b *countingBody) Close() error { return b.rc.Close() }

// httpRec is the ResponseWriter handed to the wrapped handler. It
// passes everything through and counts: status, bytes written.
type httpRec struct {
	reqRec
	w     http.ResponseWriter
	body  countingBody
	wrote bool
}

var httpRecPool = sync.Pool{New: func() any { return new(httpRec) }}

func (h *httpRec) Header() http.Header { return h.w.Header() }

func (h *httpRec) WriteHeader(code int) {
	if !h.wrote && (code >= 200 || code == http.StatusSwitchingProtocols) {
		h.wrote, h.status = true, code
	}
	h.w.WriteHeader(code)
}

func (h *httpRec) Write(b []byte) (int, error) {
	if !h.wrote {
		h.wrote, h.status = true, http.StatusOK
	}
	n, err := h.w.Write(b)
	h.respBytes += int64(n)
	return n, err
}

func (h *httpRec) Flush() {
	if f, ok := h.w.(http.Flusher); ok {
		if !h.wrote {
			h.wrote, h.status = true, http.StatusOK
		}
		f.Flush()
	}
}

func (h *httpRec) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := h.w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("gateway: response writer cannot hijack")
	}
	c, rw, err := hj.Hijack()
	if err == nil {
		h.wrote, h.status = true, http.StatusSwitchingProtocols
	}
	return c, rw, err
}

func (h *httpRec) Unwrap() http.ResponseWriter { return h.w }

// grpcStatusFromHeader reads a gRPC-protocol response's status from
// the headers (trailers-only) or trailers Connect served.
func grpcStatusFromHeader(h http.Header) (codes.Code, bool) {
	v := ""
	if vs := h["Grpc-Status"]; len(vs) > 0 {
		v = vs[0]
	} else if vs := h[http.TrailerPrefix+"Grpc-Status"]; len(vs) > 0 {
		v = vs[0]
	} else if vs := h[http.TrailerPrefix+"grpc-status"]; len(vs) > 0 {
		v = vs[0]
	}
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return codes.Code(n), true
}

func isWSUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// instrument wraps an ingress handler so its requests are counted,
// sized and timed. Returns h itself when metrics are off. withCtx puts
// the record on the request context (Connect: the gRPC hooks find it
// there), at the cost of a request copy.
func (g *Gateway) instrument(ing ingressID, withCtx bool, h http.Handler) http.Handler {
	tm := g.tm
	if tm == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ing
		if ing == ingGraphQL && isWSUpgrade(r) {
			id = ingGraphQLWS
		}
		hr := httpRecPool.Get().(*httpRec)
		hr.reqRec = reqRec{tm: tm, ingress: id, start: time.Now(), declared: r.ContentLength}
		hr.w, hr.wrote = w, false
		hr.stream = id == ingGraphQLWS || (id == ingMCP && r.Method == http.MethodGet)
		orig := r.Body
		if orig != nil && orig != http.NoBody {
			hr.body = countingBody{rc: orig}
			r.Body = &hr.body
		}
		if withCtx {
			r = r.WithContext(context.WithValue(r.Context(), recKey{}, &hr.reqRec))
		}
		tm.inflight[id].Inc()
		defer func() {
			r.Body = orig
			hr.reqBytes = hr.body.n
			if hr.status == 0 {
				hr.status = http.StatusOK
			}
			if id == ingConnect && !hr.grpc {
				if c, ok := grpcStatusFromHeader(hr.w.Header()); ok {
					hr.grpc, hr.grpcCode = true, c
				}
			}
			hr.finish()
			hr.w, hr.body = nil, countingBody{}
			httpRecPool.Put(hr)
		}()
		h.ServeHTTP(hr, r)
	})
}

// httpRecOf returns the record behind w when the instrument wrapper
// is in front of it.
func httpRecOf(w http.ResponseWriter) *reqRec {
	if hr, ok := w.(*httpRec); ok {
		return &hr.reqRec
	}
	return nil
}

// ---------------------------------------------------------------------
// Native gRPC wrapper
// ---------------------------------------------------------------------

// recCtx carries the request record on a stream's context without an
// allocation: it lives inside the pooled grpcRec.
type recCtx struct {
	context.Context
	rec *reqRec
}

func (c *recCtx) Value(k any) any {
	if _, ok := k.(recKey); ok {
		return c.rec
	}
	return c.Context.Value(k)
}

type grpcRec struct {
	reqRec
	grpc.ServerStream
	rctx recCtx
}

var grpcRecPool = sync.Pool{New: func() any { return new(grpcRec) }}

func (s *grpcRec) Context() context.Context { return &s.rctx }

func (s *grpcRec) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if err == nil {
		if pm, ok := m.(proto.Message); ok {
			s.reqBytes += int64(proto.Size(pm))
		}
	}
	return err
}

func (s *grpcRec) SendMsg(m any) error {
	if pm, ok := m.(proto.Message); ok {
		s.respBytes += int64(proto.Size(pm))
	}
	return s.ServerStream.SendMsg(m)
}

func (g *Gateway) instrumentGRPC(inner grpc.StreamHandler) grpc.StreamHandler {
	tm := g.tm
	if tm == nil {
		return inner
	}
	return func(srv any, stream grpc.ServerStream) (err error) {
		gr := grpcRecPool.Get().(*grpcRec)
		gr.reqRec = reqRec{tm: tm, ingress: ingGRPC, start: time.Now(), status: 200, grpc: true}
		gr.ServerStream = stream
		gr.rctx = recCtx{Context: stream.Context(), rec: &gr.reqRec}
		tm.inflight[ingGRPC].Inc()
		defer func() {
			gr.grpcCode = status.Code(err)
			gr.finish()
			gr.ServerStream, gr.rctx = nil, recCtx{}
			grpcRecPool.Put(gr)
		}()
		return inner(srv, gr)
	}
}

// ---------------------------------------------------------------------
// GraphQL outcomes (gat observer)
// ---------------------------------------------------------------------

func (g *Gateway) observeOutcome(ctx context.Context, o gat.Outcome) {
	tm := g.tm
	if o.Reject != "" {
		tm.reject(ingGraphQL, o.Reject)
	}
	switch o.ErrorKind {
	case "parse":
		tm.gqlErr[0].Inc()
	case "validate":
		tm.gqlErr[1].Inc()
	case "execution":
		tm.gqlErr[2].Inc()
	}
	if o.Partial {
		tm.partial.Inc()
	}
	if accum := dispatchAccumulatorFromContext(ctx); accum != nil && accum.rec != nil && accum.rec.op == opNone {
		switch {
		case o.Reject != "" || o.ErrorKind == "parse" || o.ErrorKind == "validate":
		case o.Mutation:
			accum.rec.op = opMutation
		case o.Subscription:
			accum.rec.op = opSubscription
		default:
			accum.rec.op = opQuery
		}
	}
}

// ---------------------------------------------------------------------
// Scrape-time gauges
// ---------------------------------------------------------------------

// gatewayCollector reads registry and cluster state at scrape time.
type gatewayCollector struct {
	g        *Gateway
	services *prometheus.Desc
	ejected  *prometheus.Desc
	peers    *prometheus.Desc
	level    *prometheus.Desc
}

func newGatewayCollector(g *Gateway, p string) *gatewayCollector {
	return &gatewayCollector{
		g: g,
		services: prometheus.NewDesc(p+"_registered_services",
			"Registered services (slots) on this node, by kind. Gateway-internal slots are not counted.",
			[]string{"kind"}, nil),
		ejected: prometheus.NewDesc(p+"_replicas_ejected",
			"Replicas currently ejected from rotation, per service.",
			[]string{"namespace", "version"}, nil),
		peers: prometheus.NewDesc(p+"_cluster_peers",
			"Gateway nodes currently routed in the NATS cluster: distinct remote servers with a live route, plus this node; 0 when not clustered.",
			nil, nil),
		level: prometheus.NewDesc(p+"_cluster_feature_level",
			"Cluster feature levels on this node: kind=active (the cluster's level as this node last read it), min and max (the levels this binary can run at). Cluster nodes only.",
			[]string{"kind"}, nil),
	}
}

func (c *gatewayCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.services
	ch <- c.ejected
	ch <- c.peers
	ch <- c.level
}

func (c *gatewayCollector) Collect(ch chan<- prometheus.Metric) {
	g := c.g
	kinds := map[slotKind]int{}
	type nv struct{ ns, ver string }
	ejected := map[nv]int{}
	now := time.Now().UnixNano()
	count := func(k nv, h *replicaHealth) {
		if until := h.ejectedUntil.Load(); until != 0 && now < until {
			ejected[k]++
		} else if _, ok := ejected[k]; !ok {
			ejected[k] = 0
		}
	}
	g.mu.Lock()
	for key, s := range g.slots {
		if s.kind == slotKindInternalProto {
			continue
		}
		kinds[s.kind]++
		k := nv{key.namespace, key.version}
		switch {
		case s.proto != nil:
			if rs := s.proto.replicas.Load(); rs != nil {
				for _, r := range *rs {
					count(k, &r.health)
				}
			}
		case s.openapi != nil:
			if rs := s.openapi.replicas.Load(); rs != nil {
				for _, r := range *rs {
					count(k, &r.health)
				}
			}
		case s.graphql != nil:
			if rs := s.graphql.replicas.Load(); rs != nil {
				for _, r := range *rs {
					count(k, &r.health)
				}
			}
		}
	}
	g.mu.Unlock()
	peers := g.livePeerCount()
	for k, n := range kinds {
		ch <- prometheus.MustNewConstMetric(c.services, prometheus.GaugeValue, float64(n), k.String())
	}
	for k, n := range ejected {
		ch <- prometheus.MustNewConstMetric(c.ejected, prometheus.GaugeValue, float64(n), k.ns, k.ver)
	}
	ch <- prometheus.MustNewConstMetric(c.peers, prometheus.GaugeValue, float64(peers))
	if g.cfg.cluster != nil {
		ch <- prometheus.MustNewConstMetric(c.level, prometheus.GaugeValue, float64(g.clusterLevel.Load()), "active")
		ch <- prometheus.MustNewConstMetric(c.level, prometheus.GaugeValue, float64(clusterLevelMin), "min")
		ch <- prometheus.MustNewConstMetric(c.level, prometheus.GaugeValue, float64(clusterLevelMax()), "max")
	}
}

// livePeerCount counts this node plus the distinct remote NATS servers
// it holds a live route to; 0 when not clustered. Route state follows
// the TCP/NATS-ping lifecycle, so a dead peer drops within seconds and
// a restarted node (new server ID) replaces its predecessor. Distinct
// RemoteID because route pooling opens several connections per peer.
// The peers KV bucket is not used: its entries linger for peerTTL after
// death and raise no delete event on expiry.
func (g *Gateway) livePeerCount() int {
	cl := g.cfg.cluster
	if cl == nil || cl.Server == nil {
		return 0
	}
	rz, err := cl.Server.Routez(&natsd.RoutezOptions{})
	if err != nil || rz == nil {
		return 1
	}
	remotes := make(map[string]struct{}, len(rz.Routes))
	for _, r := range rz.Routes {
		remotes[r.RemoteID] = struct{}{}
	}
	return len(remotes) + 1
}
