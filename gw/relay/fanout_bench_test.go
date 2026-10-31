package relay

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// BenchmarkFanoutCompressed pushes one event to N compressed sockets through a
// real server. Clients read raw frames without inflating them, so the numbers
// are the server's. "sameID" is what the generated runtime sends; "distinctID"
// is the worst case, where no two sockets can share a frame.
func BenchmarkFanoutCompressed(b *testing.B) {
	var row strings.Builder
	for i := 0; row.Len() < 20<<10; i++ {
		fmt.Fprintf(&row, `{"team":"team-%d","score":%d,"rank":%d},`, i, i*37%1000, i)
	}
	const sockets = 100
	for _, tc := range []struct {
		name string
		id   func(int) string
	}{
		{"sameID", func(int) string { return "ch#0" }},
		{"distinctID", func(i int) string { return fmt.Sprint(i) }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			secret := []byte("k")
			src := newFakeSource()
			srv := httptest.NewServer(New(secret, src, WithCompression()).Handler())
			defer srv.Close()
			url := "ws" + strings.TrimPrefix(srv.URL, "http")
			tok, _ := Sign(secret, Cap{Channel: "ch"})
			dialer := websocket.Dialer{EnableCompression: true}
			readers := make([]*bufio.Reader, sockets)
			for i := range readers {
				c, resp, err := dialer.Dial(url, http.Header{})
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				if !strings.Contains(resp.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate") {
					b.Fatal("compression not negotiated")
				}
				if err := c.WriteJSON(inFrame{Op: "sub", ID: tc.id(i), Token: tok}); err != nil {
					b.Fatal(err)
				}
				if _, _, err := c.ReadMessage(); err != nil { // ack
					b.Fatal(err)
				}
				readers[i] = bufio.NewReader(c.UnderlyingConn())
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// A new payload each time, as a real event is.
				payload := []byte(fmt.Sprintf(`{"seq":%d,"rows":[%s{}]}`, i, row.String()))
				var wg sync.WaitGroup
				for _, r := range readers {
					wg.Add(1)
					go func(r *bufio.Reader) {
						defer wg.Done()
						if err := discardMessage(r); err != nil {
							b.Error(err)
						}
					}(r)
				}
				src.publish("ch", payload)
				wg.Wait()
			}
		})
	}
}

// discardMessage skips one message of unmasked server frames.
func discardMessage(r *bufio.Reader) error {
	for {
		var h [2]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return err
		}
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var x [2]byte
			if _, err := io.ReadFull(r, x[:]); err != nil {
				return err
			}
			n = uint64(binary.BigEndian.Uint16(x[:]))
		case 127:
			var x [8]byte
			if _, err := io.ReadFull(r, x[:]); err != nil {
				return err
			}
			n = binary.BigEndian.Uint64(x[:])
		}
		if _, err := r.Discard(int(n)); err != nil {
			return err
		}
		if h[0]&0x80 != 0 {
			return nil
		}
	}
}
