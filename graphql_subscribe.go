package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

// upstreamSubFrame is one delivery from an upstream subscription. Set
// Result on a `next`, set Errors on an `error`, set Done=true once the
// upstream sent `complete` (or the connection closed). Local resolvers
// fan one of these to graphql-go's subscribe channel per frame.
type upstreamSubFrame struct {
	// Result is the upstream `next` payload — already
	// JSON-roundtripped into map[string]any so the local subscription
	// resolver can transform / pluck the field-of-interest.
	Result map[string]any
	// Errors is the upstream `error` payload (graphql errors array)
	// or a transport-level failure surfaced as a single error.
	Errors []json.RawMessage
	// Done signals the stream has ended (upstream `complete`, ctx
	// cancel, or transport close). The channel is closed after Done
	// is sent; callers can use either signal.
	Done bool
}

// subscribeUpstreamGraphQL opens a graphql-transport-ws connection to
// `endpoint`, subscribes with `query` + `variables`, and returns a
// channel that delivers `next` / `error` frames until ctx cancels or
// the upstream sends `complete`.
//
// `forwardHeaders` is the same allowlist OpenAPI / query forwarding
// uses; it's applied to the WS upgrade request so connection_init can
// be authenticated identically to a query.
//
// v1: one WS connection per call. Multiplexing many local subscribers
// onto one upstream WS is a tier-3 follow-up.
func subscribeUpstreamGraphQL(
	ctx context.Context,
	endpoint string,
	query string,
	variables map[string]any,
	forwardHeaders []string,
) (<-chan *upstreamSubFrame, error) {
	wsURL, err := graphqlEndpointToWSURL(endpoint)
	if err != nil {
		return nil, err
	}

	headers := http.Header{}
	if in := HTTPRequestFromContext(ctx); in != nil {
		allow := forwardHeaders
		if allow == nil {
			allow = defaultForwardedHeaders
		}
		for _, h := range allow {
			if v := in.Header.Get(h); v != "" {
				headers.Set(h, v)
			}
		}
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{wsSubprotocol},
		HTTPHeader:   headers,
	})
	dialCancel()
	if err != nil {
		return nil, fmt.Errorf("graphql ws dial %s: %w", wsURL, err)
	}
	if conn.Subprotocol() != wsSubprotocol {
		_ = conn.Close(websocket.StatusPolicyViolation, "subprotocol mismatch")
		return nil, fmt.Errorf("graphql ws %s: server selected %q, want %q", wsURL, conn.Subprotocol(), wsSubprotocol)
	}

	// connection_init → connection_ack handshake.
	if err := wsWriteJSON(ctx, conn, wsMessage{Type: msgConnInit}); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "connection_init failed")
		return nil, fmt.Errorf("graphql ws %s: write connection_init: %w", wsURL, err)
	}
	ackCtx, ackCancel := context.WithTimeout(ctx, 5*time.Second)
	for {
		var m wsMessage
		if err := wsReadJSON(ackCtx, conn, &m); err != nil {
			ackCancel()
			_ = conn.Close(websocket.StatusInternalError, "connection_ack timeout")
			return nil, fmt.Errorf("graphql ws %s: await connection_ack: %w", wsURL, err)
		}
		if m.Type == msgConnAck {
			break
		}
		if m.Type == msgPing {
			_ = wsWriteJSON(ctx, conn, wsMessage{Type: msgPong})
			continue
		}
		// Anything else before ack is a protocol violation.
		ackCancel()
		_ = conn.Close(websocket.StatusPolicyViolation, "unexpected pre-ack frame")
		return nil, fmt.Errorf("graphql ws %s: pre-ack frame %q", wsURL, m.Type)
	}
	ackCancel()

	// Subscribe.
	subID := newWSSubID()
	subPayload, err := json.Marshal(subscribePayload{Query: query, Variables: variables})
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "marshal subscribe")
		return nil, fmt.Errorf("graphql ws %s: marshal subscribe: %w", wsURL, err)
	}
	if err := wsWriteJSON(ctx, conn, wsMessage{ID: subID, Type: msgSubscribe, Payload: subPayload}); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "send subscribe failed")
		return nil, fmt.Errorf("graphql ws %s: write subscribe: %w", wsURL, err)
	}

	out := make(chan *upstreamSubFrame, 8)

	// Reader pump: read frames, decode `next` / `error`, emit to out.
	// Cancellation: ctx cancel triggers a `complete` send + ws close;
	// the reader exits when the conn closes.
	go func() {
		defer close(out)
		defer func() {
			_ = conn.Close(websocket.StatusNormalClosure, "done")
		}()
		// Watcher: when ctx cancels, send complete + close.
		go func() {
			<-ctx.Done()
			cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = wsWriteJSON(cctx, conn, wsMessage{ID: subID, Type: msgComplete})
			ccancel()
			_ = conn.Close(websocket.StatusNormalClosure, "client done")
		}()

		for {
			var m wsMessage
			if err := wsReadJSON(ctx, conn, &m); err != nil {
				if ctx.Err() == nil {
					select {
					case out <- &upstreamSubFrame{
						Errors: []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"message":"upstream ws read: %s"}`, jsonEscape(err.Error())))},
						Done:   true,
					}:
					default:
					}
				}
				return
			}
			switch m.Type {
			case msgPing:
				_ = wsWriteJSON(ctx, conn, wsMessage{Type: msgPong})
			case msgPong:
				// no-op
			case msgNext:
				if m.ID != subID {
					continue // shouldn't happen; we only have one sub
				}
				var payload struct {
					Data   map[string]any    `json:"data,omitempty"`
					Errors []json.RawMessage `json:"errors,omitempty"`
				}
				if err := json.Unmarshal(m.Payload, &payload); err != nil {
					select {
					case out <- &upstreamSubFrame{Errors: []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"message":"decode next: %s"}`, jsonEscape(err.Error())))}}:
					case <-ctx.Done():
						return
					}
					continue
				}
				select {
				case out <- &upstreamSubFrame{Result: payload.Data, Errors: payload.Errors}:
				case <-ctx.Done():
					return
				}
			case msgError:
				if m.ID != subID {
					continue
				}
				var errs []json.RawMessage
				if err := json.Unmarshal(m.Payload, &errs); err != nil {
					errs = []json.RawMessage{m.Payload}
				}
				select {
				case out <- &upstreamSubFrame{Errors: errs, Done: true}:
				case <-ctx.Done():
				}
				return
			case msgComplete:
				if m.ID != subID {
					continue
				}
				select {
				case out <- &upstreamSubFrame{Done: true}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()

	return out, nil
}

// graphqlEndpointToWSURL turns an http(s) GraphQL endpoint into a
// ws(s) URL. Most servers serve both transports at the same path
// (graphql-yoga, hasura, apollo-server), so a swap of the scheme
// suffices. Operators with different paths can override at the source
// level (future option).
func graphqlEndpointToWSURL(endpoint string) (string, error) {
	switch {
	case strings.HasPrefix(endpoint, "http://"):
		return "ws://" + strings.TrimPrefix(endpoint, "http://"), nil
	case strings.HasPrefix(endpoint, "https://"):
		return "wss://" + strings.TrimPrefix(endpoint, "https://"), nil
	case strings.HasPrefix(endpoint, "ws://"), strings.HasPrefix(endpoint, "wss://"):
		return endpoint, nil
	default:
		return "", fmt.Errorf("graphql subscribe: unsupported endpoint scheme: %s", endpoint)
	}
}

// wsWriteJSON serialises and writes a graphql-transport-ws message.
// Caller-owned ctx bounds the write deadline.
func wsWriteJSON(ctx context.Context, conn *websocket.Conn, m wsMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

// wsReadJSON reads one text frame and decodes into m.
func wsReadJSON(ctx context.Context, conn *websocket.Conn, m *wsMessage) error {
	_, b, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, m)
}

// newWSSubID returns a fresh unique-per-connection subscription id.
func newWSSubID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// jsonEscape escapes `s` for embedding in a JSON string literal.
// Cheap; only used for crafting the synthetic-error payload.
func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}
