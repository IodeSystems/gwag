package gateway

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// App-key metrics. Like lifecycleMetrics they live on the default
// Prometheus registry only; with WithMetrics(custom) or
// WithoutMetrics nothing is registered (the Metrics interface is not
// extended).
type appKeyMetrics struct {
	g        *Gateway
	auth     *prometheus.CounterVec
	expiry   *prometheus.Desc
	expiring *prometheus.Desc
}

func newAppKeyMetrics(g *Gateway, pm *prometheusMetrics) *appKeyMetrics {
	p := pm.prefix
	m := &appKeyMetrics{
		g: g,
		auth: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: p + "_app_key_auth_total",
			Help: "App-key checks by op (register, heartbeat, deregister, drain_instance, schema_registry_write, or an operator RPC) and outcome: ok, ok_admin, missing, invalid, expired, denied_delegate, namespace_forbidden, unavailable, admin_required.",
		}, []string{"op", "outcome"}),
		expiry: prometheus.NewDesc(p+"_app_key_expiry_seconds",
			"Seconds until each app key the gateway minted expires; negative once expired. Revoked keys have no series. Cluster-wide set, read on this node.",
			[]string{"key_id"}, nil),
		expiring: prometheus.NewDesc(p+"_app_keys_expiring",
			"App keys the gateway minted that expire within 7 days and have not expired yet.",
			nil, nil),
	}
	pm.registry.MustRegister(m.auth, m)
	return m
}

func (m *appKeyMetrics) Describe(chan<- *prometheus.Desc) {}

func (m *appKeyMetrics) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	keys, err := m.g.listAppKeys(ctx)
	if err != nil {
		return
	}
	now := nowFunc()
	expiring := 0
	for _, r := range keys {
		left := r.expires().Sub(now)
		if left > 0 && left <= appKeyWarnBefore {
			expiring++
		}
		ch <- prometheus.MustNewConstMetric(m.expiry, prometheus.GaugeValue, left.Seconds(), r.ID)
	}
	ch <- prometheus.MustNewConstMetric(m.expiring, prometheus.GaugeValue, float64(expiring))
}

// recordAppKeyAuth counts one check. No-op without the default sink.
func (g *Gateway) recordAppKeyAuth(op, outcome string) {
	if g.appKeyMetrics == nil {
		return
	}
	g.appKeyMetrics.auth.WithLabelValues(op, outcome).Inc()
}
