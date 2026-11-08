package gateway

import "encoding/json"

// subscriptionBroker returns the ps.sub broker, lazy-initialized on
// first use over the gateway's pub/sub Bus (g.psBus: NATS with local
// fallback in cluster mode, local otherwise). Never nil.
func (g *Gateway) subscriptionBroker() *subBroker {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.broker == nil {
		g.broker = newSubBroker(g.psBus)
		g.broker.typed = g.decodeTypedEvent
	}
	return g.broker
}

// adminEventsBroker returns the broker admin_events_watchServices
// subscribes through: the shared broker in cluster mode, nil without
// a cluster (as before the pub/sub Bus). Admin events are published
// on raw NATS (publishServiceChange), never on the local bus, so the
// failover bus only ever delivers them from NATS.
func (g *Gateway) adminEventsBroker() *subBroker {
	if g.cfg.cluster == nil {
		return nil
	}
	return g.subscriptionBroker()
}

// lookupPool returns the proto pool for (ns, ver) under g.mu.
func (g *Gateway) lookupPool(ns, ver string) (*pool, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.protoSlot(poolKey{namespace: ns, version: ver})
	return p, p != nil
}

// ---------------------------------------------------------------------
// graphql-transport-ws protocol
// https://github.com/enisdenjo/graphql-ws/blob/master/PROTOCOL.md
// ---------------------------------------------------------------------

const wsSubprotocol = "graphql-transport-ws"

const (
	msgConnInit  = "connection_init"
	msgConnAck   = "connection_ack"
	msgPing      = "ping"
	msgPong      = "pong"
	msgSubscribe = "subscribe"
	msgNext      = "next"
	msgError     = "error"
	msgComplete  = "complete"
)

type wsMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type subscribePayload struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables,omitempty"`
	OperationName string         `json:"operationName,omitempty"`
}
