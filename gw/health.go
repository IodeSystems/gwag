package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// HealthHandler returns an http.Handler for /health-style probes.
//   - 200 + JSON when serving normally.
//   - 503 + JSON when Drain has been called.
//
// Mount alongside the GraphQL handler; load balancers poll this to
// take the gateway out of rotation when it goes 503.
//
// Stability: stable
func (g *Gateway) HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := "serving"
		code := http.StatusOK
		if g.draining.Load() {
			status = "draining"
			code = http.StatusServiceUnavailable
		}
		body := map[string]any{
			"status":         status,
			"active_streams": g.streamGlobal.Load(),
		}
		if g.cfg.cluster != nil {
			body["node_id"] = g.cfg.cluster.NodeID
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
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
