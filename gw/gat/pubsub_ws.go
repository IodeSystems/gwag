package gat

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"nhooyr.io/websocket"
)

// SubscribePath is the URL path (under the RegisterHTTP /
// RegisterHuma prefix) of the WebSocket stream that delivers pubsub
// events to clients. Always mounted — the in-process pubsub
// primitive is always available.
//
//	GET {prefix}/_gat/subscribe?channel=PATTERN   (WebSocket upgrade)
//
// Each matching event arrives as a text frame carrying the JSON
// {channel, payload} shape of Event. The stream ends when the client
// disconnects or the gateway is closed. When EnableSubscribeAuth is
// set, the request must also carry valid `token` and `ts` query
// parameters minted by SignSubscribeToken.
//
// Stability: experimental
const SubscribePath = "/_gat/subscribe"

const wsWriteTimeout = 5 * time.Second

// subscribeWSHandler upgrades the request to a WebSocket and streams
// pubsub events matching the `channel` query parameter. It is a
// server-to-client stream: the client sends nothing after the
// handshake. The subscription is cancelled when the client
// disconnects.
func (g *Gateway) subscribeWSHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pattern := r.URL.Query().Get("channel")
		if pattern == "" {
			http.Error(w, "missing channel query parameter", http.StatusBadRequest)
			return
		}
		// Verify the subscribe token before upgrading — reject with a
		// plain 401 rather than completing the handshake then closing.
		if err := g.verifySubscribeRequest(r); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return // Accept has already written the failure
		}
		defer conn.Close(websocket.StatusInternalError, "internal")

		// CloseRead drains + discards any client frames (handling
		// pings) and returns a context cancelled when the connection
		// drops — the disconnect signal for this push-only stream.
		ctx := conn.CloseRead(r.Context())

		events, cancel := g.pubsub.Subscribe(pattern)
		defer cancel()

		for {
			select {
			case <-ctx.Done():
				conn.Close(websocket.StatusNormalClosure, "")
				return
			case ev, open := <-events:
				if !open {
					conn.Close(websocket.StatusGoingAway, "gateway closed")
					return
				}
				data, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				wctx, wcancel := context.WithTimeout(ctx, wsWriteTimeout)
				err = conn.Write(wctx, websocket.MessageText, data)
				wcancel()
				if err != nil {
					return // client gone or write timed out
				}
			}
		}
	})
}
