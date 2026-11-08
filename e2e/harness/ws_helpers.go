//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// Signed is a subscription / publish token for one literal channel.
type Signed struct {
	Hmac string
	TS   int64
}

// Args renders the token as GraphQL field arguments:
// `hmac: "...", ts: "..."`.
func (s Signed) Args() string {
	return fmt.Sprintf("hmac: %q, ts: %q", s.Hmac, strconv.FormatInt(s.TS, 10))
}

// Sign mints a token for channel through the gateway's admin sign
// endpoint with the boot token. Nodes that share a --subscribe-secret
// accept each other's tokens.
func (g *Gateway) Sign(ctx context.Context, channel string, ttl time.Duration) (Signed, error) {
	var out struct {
		Code          string `json:"code"`
		Hmac          string `json:"hmac"`
		TimestampUnix int64  `json:"timestampUnix"`
		Reason        string `json:"reason"`
	}
	body := map[string]any{"channel": channel, "ttlSeconds": int64(ttl / time.Second)}
	if err := g.Admin().Do(ctx, http.MethodPost, "/admin/sign", body, &out); err != nil {
		return Signed{}, err
	}
	if out.Hmac == "" {
		return Signed{}, fmt.Errorf("sign %s: code %s: %s", channel, out.Code, out.Reason)
	}
	return Signed{Hmac: out.Hmac, TS: out.TimestampUnix}, nil
}

// Publish calls ps.pub on this node with a token for channel.
func (g *Gateway) Publish(ctx context.Context, channel, payload string, tok Signed) error {
	q := fmt.Sprintf(`{ ps { pub(channel: %q, payload: %q, %s) { _void } } }`, channel, payload, tok.Args())
	resp, err := g.GraphQL(ctx, q, nil)
	if err != nil {
		return err
	}
	return resp.Err()
}

// PublishTyped calls ps.pub with a payload_type: on a named channel
// the gateway parses payload as the protojson of that message and
// publishes the binary proto, which generated listeners decode.
func (g *Gateway) PublishTyped(ctx context.Context, channel, payload, payloadType string, tok Signed) error {
	q := fmt.Sprintf(`{ ps { pub(channel: %q, payload: %q, payload_type: %q, %s) { _void } } }`, channel, payload, payloadType, tok.Args())
	resp, err := g.GraphQL(ctx, q, nil)
	if err != nil {
		return err
	}
	return resp.Err()
}

// WSFrame is one graphql-transport-ws message.
type WSFrame struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Subscription is one graphql-transport-ws subscription on one node.
// The data payload of each `next` frame arrives on Events; Done is
// closed when the connection ends (a `complete`, an error frame, or a
// closed socket), with Err holding why.
type Subscription struct {
	Node   *Gateway
	Events chan json.RawMessage
	Done   chan struct{}

	conn *websocket.Conn
	mu   sync.Mutex
	err  error
	n    atomic.Int64
}

// Subscribe opens /api/graphql as a graphql-transport-ws socket on g,
// completes the connection_init handshake and starts query. It is
// closed at test end.
func (g *Gateway) Subscribe(t testing.TB, query string) (*Subscription, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+strings.TrimPrefix(g.BaseURL(), "http://")+"/api/graphql", &websocket.DialOptions{
		Subprotocols: []string{"graphql-transport-ws"},
		HTTPHeader:   http.Header{"X-Caller-Id": []string{DefaultCaller}},
	})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", g.Name, err)
	}
	conn.SetReadLimit(1 << 20)
	s := &Subscription{Node: g, Events: make(chan json.RawMessage, 4096), Done: make(chan struct{}), conn: conn}
	fail := func(err error) (*Subscription, error) {
		_ = conn.Close(websocket.StatusInternalError, "setup failed")
		return nil, err
	}
	if err := s.write(ctx, WSFrame{Type: "connection_init", Payload: json.RawMessage(`{}`)}); err != nil {
		return fail(err)
	}
	for {
		f, err := s.read(ctx)
		if err != nil {
			return fail(fmt.Errorf("%s: await connection_ack: %w", g.Name, err))
		}
		if f.Type == "connection_ack" {
			break
		}
		if f.Type != "ping" {
			return fail(fmt.Errorf("%s: want connection_ack, got %s %s", g.Name, f.Type, f.Payload))
		}
		_ = s.write(ctx, WSFrame{Type: "pong"})
	}
	payload, _ := json.Marshal(map[string]any{"query": query})
	if err := s.write(ctx, WSFrame{ID: "1", Type: "subscribe", Payload: payload}); err != nil {
		return fail(err)
	}
	go s.pump()
	t.Cleanup(s.Close)
	return s, nil
}

func (s *Subscription) write(ctx context.Context, f WSFrame) error {
	b, _ := json.Marshal(f)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, b)
}

func (s *Subscription) read(ctx context.Context) (WSFrame, error) {
	var f WSFrame
	_, b, err := s.conn.Read(ctx)
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(b, &f)
}

func (s *Subscription) pump() {
	defer close(s.Done)
	ctx := context.Background()
	for {
		f, err := s.read(ctx)
		if err != nil {
			s.setErr(err)
			return
		}
		switch f.Type {
		case "next":
			var p struct {
				Data   json.RawMessage `json:"data"`
				Errors json.RawMessage `json:"errors"`
			}
			_ = json.Unmarshal(f.Payload, &p)
			if len(p.Errors) > 0 && string(p.Errors) != "null" {
				s.setErr(fmt.Errorf("next with errors: %s", p.Errors))
				return
			}
			s.n.Add(1)
			select {
			case s.Events <- p.Data:
			default:
				s.setErr(fmt.Errorf("event buffer full"))
				return
			}
		case "ping":
			_ = s.write(ctx, WSFrame{Type: "pong"})
		case "complete":
			s.setErr(fmt.Errorf("complete"))
			return
		case "error":
			s.setErr(fmt.Errorf("error frame: %s", f.Payload))
			return
		}
	}
}

func (s *Subscription) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// Err is why the subscription ended; nil while it runs.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Received is the number of `next` frames so far.
func (s *Subscription) Received() int64 { return s.n.Load() }

// Close ends the socket. Safe to call more than once.
func (s *Subscription) Close() { _ = s.conn.Close(websocket.StatusNormalClosure, "bye") }
