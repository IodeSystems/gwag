package memory_test

import (
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/eventstest"
	"github.com/iodesystems/gwag/v2/events/memory"
)

func TestBackend(t *testing.T) {
	eventstest.Run(t, eventstest.Harness{
		New: func(t *testing.T) events.Backend {
			b := memory.New()
			t.Cleanup(func() { _ = b.Close() })
			return b
		},
		DeadLetters: func(t *testing.T, b events.Backend, stream string, n int) []*events.Message {
			deadline := time.Now().Add(10 * time.Second)
			for {
				var out []*events.Message
				for _, m := range b.(*memory.Backend).DeadLetters() {
					if m.Subject == events.DLQSubject(stream) {
						out = append(out, m)
					}
				}
				if len(out) >= n {
					return out
				}
				if time.Now().After(deadline) {
					t.Fatalf("got %d dead letters, want %d", len(out), n)
				}
				time.Sleep(5 * time.Millisecond)
			}
		},
	})
}
