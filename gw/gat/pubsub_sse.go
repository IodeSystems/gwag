package gat

import (
	"encoding/json"
	"net/http"
)

// SubscribePath is the URL path (under the RegisterHTTP /
// RegisterHuma prefix) of the Server-Sent Events stream that
// delivers pubsub events to browser / HTTP clients. Always mounted —
// the in-process pubsub primitive is always available.
//
//	GET {prefix}/_gat/subscribe?channel=PATTERN
//
// Each event arrives as an SSE `data:` line carrying the JSON
// {channel, payload} shape of Event. The stream ends when the client
// disconnects or the gateway is closed.
//
// Stability: experimental
const SubscribePath = "/_gat/subscribe"

// subscribeSSEHandler streams pubsub events matching the `channel`
// query parameter to the client as Server-Sent Events. The
// subscription is cancelled when the client disconnects.
func (g *Gateway) subscribeSSEHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pattern := r.URL.Query().Get("channel")
		if pattern == "" {
			http.Error(w, "missing channel query parameter", http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		ch, cancel := g.pubsub.Subscribe(pattern)
		defer cancel()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case ev, open := <-ch:
				if !open {
					return // gateway closed
				}
				data, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				if _, err := w.Write([]byte("data: " + string(data) + "\n\n")); err != nil {
					return // client gone
				}
				flusher.Flush()
			}
		}
	})
}
