package gateway

import "encoding/json"

// subscriptionBroker returns the gateway's shared subBroker, lazy-
// initialized on first use. nil when no cluster is configured.
func (g *Gateway) subscriptionBroker() *subBroker {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.broker == nil && g.cfg.cluster != nil {
		g.broker = newSubBroker(g.cfg.cluster.Conn)
	}
	return g.broker
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
