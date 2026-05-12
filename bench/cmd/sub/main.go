// sub — minimal graphql-transport-ws subscription client for bench.
//
// Connects to the gateway's WebSocket endpoint, subscribes to
// ps_sub(channel), and prints each event as a JSON line to stdout.
//
//	bench sub --channel events.test.>
//	bench sub --channel events.test.> --timeout 30
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nhooyr.io/websocket"
)

type wsMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type subscribePayload struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

func main() {
	fs := flag.NewFlagSet("sub", flag.ExitOnError)
	target := fs.String("target", "", "Gateway HTTP URL (e.g. http://localhost:18080)")
	channel := fs.String("channel", "", "ps.sub channel subject")
	timeout := fs.Duration("timeout", 0, "Exit after duration (e.g. 30s); 0 = run until interrupted")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sub [flags]")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Subscribe to a ps.sub channel and print events as JSON lines.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return
	}
	if *target == "" || *channel == "" {
		fmt.Fprintln(os.Stderr, "error: --target and --channel are required")
		os.Exit(1)
	}

	// Convert http(s) → ws(s).
	wsURL := *target
	if len(wsURL) >= 5 && wsURL[:5] == "http:" {
		wsURL = "ws:" + wsURL[5:]
	} else if len(wsURL) >= 6 && wsURL[:6] == "https:" {
		wsURL = "wss:" + wsURL[6:]
	}
	wsURL += "/api/graphql"

	ctx, cancel := context.WithCancel(context.Background())
	if *timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, *timeout)
	}

	// Handle SIGINT/SIGTERM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()

	// Connect.
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"graphql-transport-ws"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", wsURL, err)
		os.Exit(1)
	}
	defer conn.CloseNow()

	// connection_init.
	if err := writeJSON(ctx, conn, wsMessage{Type: "connection_init"}); err != nil {
		fmt.Fprintf(os.Stderr, "connection_init: %v\n", err)
		os.Exit(1)
	}

	// Wait for connection_ack.
	msg, err := readJSON(ctx, conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read connection_ack: %v\n", err)
		os.Exit(1)
	}
	if msg.Type != "connection_ack" {
		fmt.Fprintf(os.Stderr, "expected connection_ack, got %s\n", msg.Type)
		os.Exit(1)
	}

	// Subscribe.
	query := fmt.Sprintf(`subscription { ps_sub(channel: %s) { channel payload payloadType ts } }`, jsonQuote(*channel))
	subID := "1"
	if err := writeJSON(ctx, conn, wsMessage{
		ID:    subID,
		Type:  "subscribe",
		Payload: mustMarshal(subscribePayload{Query: query}),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "subscribe: %v\n", err)
		os.Exit(1)
	}

	// Read events.
	for {
		msg, err := readJSON(ctx, conn)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				fmt.Fprintf(os.Stderr, "read: %v\n", err)
				os.Exit(1)
			}
		}
		switch msg.Type {
		case "next":
			// The payload is {"data": {"ps_sub": {...}}}.
			var envelope struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(msg.Payload, &envelope); err == nil && envelope.Data != nil {
				var data map[string]json.RawMessage
				if err := json.Unmarshal(envelope.Data, &data); err == nil {
					if ev, ok := data["ps_sub"]; ok {
						fmt.Println(string(ev))
						continue
					}
				}
			}
			// Fallback: print raw payload.
			fmt.Println(string(msg.Payload))
		case "complete":
			return
		case "error":
			fmt.Fprintf(os.Stderr, "subscription error: %s\n", msg.Payload)
			return
		case "ping":
			_ = writeJSON(ctx, conn, wsMessage{Type: "pong"})
		case "pong":
			// ignore
		default:
			fmt.Fprintf(os.Stderr, "unknown message type: %s\n", msg.Type)
		}
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

func readJSON(ctx context.Context, conn *websocket.Conn) (wsMessage, error) {
	_, b, err := conn.Read(ctx)
	if err != nil {
		return wsMessage{}, err
	}
	var msg wsMessage
	if err := json.Unmarshal(b, &msg); err != nil {
		return wsMessage{}, err
	}
	return msg, nil
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
