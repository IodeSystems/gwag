package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// HealthHandler returns an http.Handler for /health-style probes.
//   - 200 + JSON, status "serving", when serving normally.
//   - 503 + JSON, status "starting", on a cluster node that has not
//     yet replayed the service registry from JetStream KV (see
//     registry sync below).
//   - 503 + JSON, status "draining", when Drain has been called.
//   - 503 + JSON, status "incompatible", on a cluster node whose
//     binary cannot run at the cluster's active feature level
//     (cluster_level.go); "error" says why.
//
// Registry sync: a cluster node learns its services from the registry
// KV bucket. Until the reconciler's watch has delivered every existing
// value and the schema has been rebuilt from them, the node would
// answer "Cannot query field" for services the rest of the cluster
// routes, so health stays 503. The gate arms when cluster tracking
// starts (ControlPlane on a WithCluster gateway). A gateway without a
// cluster, or one whose control plane is never mounted, has nothing to
// replay and is ready at once. If JetStream is unreachable the node
// stays "starting" (and logs the retry) rather than serve a partial
// schema.
//
// The body's "pubsub" field is the ps.pub / ps.sub transport: "nats",
// "local" (no cluster), or "local_degraded" (cluster NATS connection
// down; this node delivers locally only). Degraded pub/sub adds
// "degraded": true but keeps the status code — the node still
// serves.
//
// Mount alongside the GraphQL handler; load balancers poll this to
// take the gateway out of rotation when it goes 503.
//
// Stability: stable
func (g *Gateway) HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := "serving"
		code := http.StatusOK
		switch {
		case g.draining.Load():
			status = "draining"
			code = http.StatusServiceUnavailable
		case g.clusterLevelErr.Load() != nil:
			status = "incompatible"
			code = http.StatusServiceUnavailable
		case g.registrySync.Load() == registrySyncPending:
			status = "starting"
			code = http.StatusServiceUnavailable
		}
		body := map[string]any{
			"status":         status,
			"active_streams": g.streamGlobal.Load(),
		}
		if g.cfg.cluster != nil {
			body["node_id"] = g.cfg.cluster.NodeID
			body["cluster_level"] = g.clusterLevel.Load()
		}
		if e := g.clusterLevelErr.Load(); e != nil {
			body["error"] = *e
		}
		mode := g.pubSubBusMode()
		body["pubsub"] = mode
		if mode == pubSubModeDegraded {
			body["degraded"] = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
}

// Registry sync states for Gateway.registrySync.
const (
	registrySyncNone    int32 = iota // no cluster tracking: nothing to replay
	registrySyncPending              // tracking started, initial replay not done
	registrySyncDone                 // initial replay applied
)

// armRegistrySync moves None to Pending. Pending and Done stay as
// they are, so a second arm after the replay cannot un-ready the node.
func (g *Gateway) armRegistrySync() {
	g.registrySync.CompareAndSwap(registrySyncNone, registrySyncPending)
}

// markRegistrySynced is called by the reconciler at the end of the
// registry KV's initial values.
func (g *Gateway) markRegistrySynced() {
	g.registrySync.Store(registrySyncDone)
}

// IsDraining reports whether Drain has been called.
//
// Stability: stable
func (g *Gateway) IsDraining() bool {
	return g.draining.Load()
}

// Drain initiates a graceful shutdown of subscription traffic:
//
//  1. /health flips to 503 so a load balancer pulls this node out.
//  2. New WebSocket upgrades are rejected with 503.
//  3. Existing WebSockets have their context cancelled — graphql-go
//     emits `complete` frames per active subscription, then the
//     connection closes.
//  4. Drain waits until streams_inflight_total reaches 0 OR ctx
//     expires.
//
// Idempotent. After return, the gateway is still alive — Close() does
// the actual teardown. Drain is the LB-friendly preamble; cluster
// shutdown follows.
//
// HTTP unary queries (GraphQL queries/mutations) are NOT actively
// drained — they're sub-second and finish on their own once the LB
// stops sending new traffic.
//
// Stability: stable
func (g *Gateway) Drain(ctx context.Context) error {
	if !g.draining.CompareAndSwap(false, true) {
		return nil // already draining
	}

	// Snapshot connection cancels and fire them all. The serveWebSocket
	// goroutines clean up themselves; their ctx-bound subscription
	// goroutines cancel, broker releases fire, stream slots return.
	g.wsMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(g.wsConns))
	for _, c := range g.wsConns {
		cancels = append(cancels, c)
	}
	g.wsMu.Unlock()
	for _, c := range cancels {
		c()
	}

	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if g.streamGlobal.Load() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
