package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Service-lifecycle metrics: start gate, per-instance drain, declared
// dependencies and sunset-ability.
//
// Gauges are computed at scrape time from the same views the admin
// API serves (ListServices + buildDependencyGraph), so a series exists
// only while its instance or line exists and the sunset-able rule is
// the admin graph's, not a copy. The cost of a scrape is one admin
// graph build (one pass over slots, instances and the 1h usage rings).
//
// Counters are direct. Transitions are detected by diffing the gate
// state of every known instance; the gate loop does that on every kick
// and every gateTick, and each scrape does it too, so a READY ->
// DEGRADED flip shorter than gateTick can go uncounted.
//
// These collectors live on the default Prometheus registry only. With
// WithMetrics(custom) or WithoutMetrics the gateway registers nothing
// (MetricsHandler answers 404 there too), the same rule as the plan
// cache collector. The Metrics interface is Stability: stable and is
// not extended for this.
//
// Every series is a per-node view, like usage: in a cluster each node
// reports its own registry, drain bookkeeping and traffic.

// sunsetMetricWindow is the usage window behind line_sunsetable. It is
// the admin dependency graph's default ("1h").
const sunsetMetricWindow = time.Hour

// Drain request outcomes for drain_requests_total.
const (
	drainOutcomeRequested = "requested"
	drainOutcomeCancelled = "cancelled"
	drainOutcomeRepeated  = "already_requested"
	drainOutcomeUnknown   = "unknown_instance"
	drainOutcomeError     = "error"
)

type seenGate struct {
	state string
	key   poolKey
}

type lifecycleMetrics struct {
	g *Gateway

	transitions *prometheus.CounterVec
	drainReqs   *prometheus.CounterVec

	instances *prometheus.Desc
	drains    *prometheus.Desc
	unmet     *prometheus.Desc
	sunset    *prometheus.Desc

	mu   sync.Mutex
	seen map[string]seenGate // gate instance id -> last counted state
}

func newLifecycleMetrics(g *Gateway, pm *prometheusMetrics) *lifecycleMetrics {
	p := pm.prefix
	m := &lifecycleMetrics{
		g:    g,
		seen: map[string]seenGate{},
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: p + "_start_gate_transitions_total",
			Help: "Start-gate state changes of control-plane instances, as seen by this node. from/to: none (new), waiting, ready, degraded, removed. READY/DEGRADED flips are sampled every 2s and at scrape.",
		}, []string{"namespace", "version", "from", "to"}),
		drainReqs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: p + "_drain_requests_total",
			Help: "Instance drain requests handled by this node, by outcome: requested (new), already_requested (repeat or poll), cancelled, unknown_instance, error.",
		}, []string{"outcome"}),
		instances: prometheus.NewDesc(p+"_service_instances",
			"Control-plane instances per (namespace, version) by start-gate state (ready, waiting, degraded). Per-node view; series exist only while the line is registered.",
			[]string{"namespace", "version", "state"}, nil),
		drains: prometheus.NewDesc(p+"_service_drain_instances",
			"Control-plane instances per (namespace, version) with a drain requested, by drain state (draining, blocked, safe_to_stop). Per-node view; series exist only while the line is registered.",
			[]string{"namespace", "version", "drain_state"}, nil),
		unmet: prometheus.NewDesc(p+"_dependency_unmet",
			"1 when a declared dependency has no accepted line with a ready instance, else 0. One series per (consumer, provider namespace). consumer_kind: service (a registered line) or external (a declared external dependent; consumer_namespace is its id, consumer_version is empty). Per-node view.",
			[]string{"consumer_kind", "consumer_namespace", "consumer_version", "provider"}, nil),
		sunset: prometheus.NewDesc(p+"_line_sunsetable",
			"1 when the admin dependency graph marks the line sunset-able, else 0: every dependent has another ready line and the line had no traffic on this node in the last 1h (the graph's default window); the only line of a namespace is never sunset-able. Per-node view.",
			[]string{"namespace", "version"}, nil),
	}
	pm.registry.MustRegister(m.transitions, m.drainReqs, m)
	return m
}

// Describe sends nothing: an unchecked collector, because the gauge
// series set changes with the registry.
func (m *lifecycleMetrics) Describe(chan<- *prometheus.Desc) {}

func (m *lifecycleMetrics) Collect(ch chan<- prometheus.Metric) {
	g := m.g
	m.observeGate()
	if g.cp == nil {
		return
	}
	reg, err := g.cp.ListServices(context.Background(), &cpv1.ListServicesRequest{})
	if err != nil {
		return
	}
	out, err := g.buildDependencyGraph(reg, "1h", sunsetMetricWindow, adminView{full: true})
	if err != nil {
		return
	}
	const gauge = prometheus.GaugeValue
	for _, n := range out.Body.Nodes {
		var ready, waiting, degraded, draining, blocked, safe float64
		for _, in := range n.Instances {
			switch in.State {
			case "waiting":
				waiting++
			case "degraded":
				degraded++
			default:
				ready++
			}
			switch in.DrainState {
			case drainStateDraining:
				draining++
			case drainStateBlocked:
				blocked++
			case drainStateSafeToStop:
				safe++
			}
		}
		ch <- prometheus.MustNewConstMetric(m.instances, gauge, ready, n.Namespace, n.Version, "ready")
		ch <- prometheus.MustNewConstMetric(m.instances, gauge, waiting, n.Namespace, n.Version, "waiting")
		ch <- prometheus.MustNewConstMetric(m.instances, gauge, degraded, n.Namespace, n.Version, "degraded")
		ch <- prometheus.MustNewConstMetric(m.drains, gauge, draining, n.Namespace, n.Version, "draining")
		ch <- prometheus.MustNewConstMetric(m.drains, gauge, blocked, n.Namespace, n.Version, "blocked")
		ch <- prometheus.MustNewConstMetric(m.drains, gauge, safe, n.Namespace, n.Version, "safe_to_stop")
		sun := 0.0
		if n.Sunsetable {
			sun = 1
		}
		ch <- prometheus.MustNewConstMetric(m.sunset, gauge, sun, n.Namespace, n.Version)
	}
	// One series per (consumer line, provider); edges with different
	// accepted-line sets fold to the worst case.
	type edgeKey struct{ kind, ns, ver, provider string }
	unmet := map[edgeKey]float64{}
	for _, e := range out.Body.DesiredEdges {
		k := edgeKey{e.ConsumerKind, e.ConsumerNamespace, e.ConsumerVersion, e.Provider}
		v := 0.0
		if !e.Met {
			v = 1
		}
		if cur, ok := unmet[k]; !ok || v > cur {
			unmet[k] = v
		}
	}
	for k, v := range unmet {
		ch <- prometheus.MustNewConstMetric(m.unmet, gauge, v, k.kind, k.ns, k.ver, k.provider)
	}
}

// observeGate counts the gate-state changes of every known instance
// since the last call. nil-safe.
func (m *lifecycleMetrics) observeGate() {
	if m == nil {
		return
	}
	g := m.g
	g.mu.Lock()
	cur := make(map[string]seenGate, len(g.gate.instances))
	for id, inst := range g.gate.instances {
		cur[id] = seenGate{
			state: adminInstanceState(g.gateStateLocked(inst.waiting, inst.manifest.Depends)),
			key:   inst.key,
		}
	}
	g.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	for id, to := range cur {
		from := "none"
		if prev, ok := m.seen[id]; ok {
			from = prev.state
		}
		if from != to.state {
			m.transitions.WithLabelValues(to.key.namespace, to.key.version, from, to.state).Inc()
			m.seen[id] = to
		}
	}
	for id, prev := range m.seen {
		if _, ok := cur[id]; !ok {
			m.transitions.WithLabelValues(prev.key.namespace, prev.key.version, prev.state, "removed").Inc()
			delete(m.seen, id)
		}
	}
}

func (m *lifecycleMetrics) recordDrainRequest(cancel, existed bool, err error) {
	if m == nil {
		return
	}
	outcome := drainOutcomeRequested
	switch {
	case errors.Is(err, errDrainUnknownInstance):
		outcome = drainOutcomeUnknown
	case err != nil:
		outcome = drainOutcomeError
	case cancel:
		outcome = drainOutcomeCancelled
	case existed:
		outcome = drainOutcomeRepeated
	}
	m.drainReqs.WithLabelValues(outcome).Inc()
}
