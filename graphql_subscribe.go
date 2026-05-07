package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// Caller-owned ctx bounds the write deadline. Used by the test fixture;
// the multiplexer in graphql_sub_broker.go has its own writeWSJSON
// that takes an explicit serialisation mutex.
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
