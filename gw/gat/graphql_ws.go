package gat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/IodeSystems/graphql-go/v2"
	"nhooyr.io/websocket"
)

// graphql-transport-ws, server side. Same protocol and behaviour as
// gw/subscriptions.go; gat's copy runs Subscription fields registered
// with Subscribe instead of NATS-backed ones.
// https://github.com/enisdenjo/graphql-ws/blob/master/PROTOCOL.md

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

type wsSubscribePayload struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables,omitempty"`
	OperationName string         `json:"operationName,omitempty"`
}

type connInitKey struct{}

// ConnectionInitPayload returns the payload the client sent in
// graphql-transport-ws `connection_init`, or nil. Browsers cannot set
// headers on a WebSocket, so clients put credentials here; read it in
// a Subscribe source or a huma middleware (via ctx.Context()).
//
// Stability: experimental
func ConnectionInitPayload(ctx context.Context) json.RawMessage {
	p, _ := ctx.Value(connInitKey{}).(json.RawMessage)
	return p
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// serveGraphQLWS handles one graphql-transport-ws connection. Each
// connection multiplexes any number of subscriptions, each with its
// own context so complete / cancel is per id. parent is the connection's
// parent context (r's, or the WSUpgrade hook's).
func (g *Gateway) serveGraphQLWS(parent context.Context, w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{wsSubprotocol},
	})
	if err != nil {
		return
	}
	if conn.Subprotocol() != wsSubprotocol {
		_ = conn.Close(websocket.StatusPolicyViolation, "expected "+wsSubprotocol+" subprotocol")
		return
	}
	defer conn.Close(websocket.StatusInternalError, "internal")

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// The upgrade request is what huma middlewares see for every
	// subscription on this connection, and one attribute bag spans
	// them: same connection, same client.
	ctx, _ = newScope(ctx, r, false)

	// websocket.Conn isn't safe for concurrent writes.
	var writeMu sync.Mutex
	writeMsg := func(m wsMessage) error {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
		defer wcancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}

	var subsMu sync.Mutex
	subs := map[string]context.CancelFunc{}
	defer func() {
		subsMu.Lock()
		for _, c := range subs {
			c()
		}
		subsMu.Unlock()
	}()

	// connection_init must arrive within 3s (graphql-ws reference
	// default).
	initCtx, initCancel := context.WithTimeout(ctx, 3*time.Second)
	defer initCancel()
	mt, raw, err := conn.Read(initCtx)
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "init timeout")
		return
	}
	if mt != websocket.MessageText {
		_ = conn.Close(websocket.StatusInvalidFramePayloadData, "expected text frame")
		return
	}
	var init wsMessage
	if err := json.Unmarshal(raw, &init); err != nil || init.Type != msgConnInit {
		_ = conn.Close(websocket.StatusPolicyViolation, "expected connection_init")
		return
	}
	if len(init.Payload) > 0 {
		ctx = context.WithValue(ctx, connInitKey{}, init.Payload)
	}
	if err := writeMsg(wsMessage{Type: msgConnAck}); err != nil {
		return
	}

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m wsMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		switch m.Type {
		case msgPing:
			_ = writeMsg(wsMessage{Type: msgPong})
		case msgPong:
		case msgSubscribe:
			if m.ID == "" {
				continue
			}
			var p wsSubscribePayload
			if err := json.Unmarshal(m.Payload, &p); err != nil {
				_ = writeMsg(wsErrorMsg(m.ID, "bad payload: "+err.Error()))
				continue
			}
			subCtx, subCancel := context.WithCancel(ctx)
			subsMu.Lock()
			if existing, ok := subs[m.ID]; ok {
				existing()
			}
			subs[m.ID] = subCancel
			subsMu.Unlock()
			go g.runGraphQLSubscription(subCtx, m.ID, p, writeMsg, func() {
				subsMu.Lock()
				delete(subs, m.ID)
				subsMu.Unlock()
			})
		case msgComplete:
			subsMu.Lock()
			if c, ok := subs[m.ID]; ok {
				c()
				delete(subs, m.ID)
			}
			subsMu.Unlock()
		}
	}
}

// runGraphQLSubscription executes one graphql.Subscribe and writes each
// result as a `next` frame, then `complete` when the stream ends. A
// failure before any data is an `error` frame and ends the
// subscription.
func (g *Gateway) runGraphQLSubscription(ctx context.Context, id string, p wsSubscribePayload, writeMsg func(wsMessage) error, cleanup func()) {
	defer cleanup()
	// Loaded per subscription, not per connection: a long-lived
	// connection's new subscriptions see a replaced schema.
	results := graphql.Subscribe(graphql.Params{
		Schema:         *g.snap.Load().schema,
		RequestString:  p.Query,
		VariableValues: p.Variables,
		OperationName:  p.OperationName,
		Context:        ctx,
	})
	for res := range results {
		if ctx.Err() != nil {
			return
		}
		// error payload = Array<GraphQLError>; next payload =
		// ExecutionResult.
		if len(res.Errors) > 0 && res.Data == nil {
			b, err := json.Marshal(res.Errors)
			if err != nil {
				continue
			}
			_ = writeMsg(wsMessage{ID: id, Type: msgError, Payload: b})
			return
		}
		b, err := json.Marshal(res)
		if err != nil {
			continue
		}
		_ = writeMsg(wsMessage{ID: id, Type: msgNext, Payload: b})
	}
	// A subscription the client completed ends without a `complete`
	// frame, as the protocol has it, unless EchoComplete is set.
	if ctx.Err() == nil || g.echoComplete {
		_ = writeMsg(wsMessage{ID: id, Type: msgComplete})
	}
}

func wsErrorMsg(id, message string) wsMessage {
	b, _ := json.Marshal([]map[string]string{{"message": message}})
	return wsMessage{ID: id, Type: msgError, Payload: b}
}
