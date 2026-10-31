// Package bustest is a conformance suite for gat.Bus implementations.
// A backend's test calls Run with a factory; every backend must pass
// the same cases (exact match, `*` / `>` wildcards, fanout to
// overlapping subscribers, unsubscribe).
//
// It lives in its own package so the testing import stays out of gat.
//
// Stability: experimental
package bustest

import (
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// Harness is one bus under test. Sync, when non-nil, blocks until
// subscriptions and publishes already issued have reached the
// transport (e.g. a NATS Flush); nil means the bus is synchronous.
//
// Stability: experimental
type Harness struct {
	Bus  gat.Bus
	Sync func()
}

// Run executes the conformance cases. newHarness is called once per
// case and must return a fresh, isolated bus.
//
// Stability: experimental
func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	t.Run("ExactMatch", func(t *testing.T) {
		h := newHarness(t)
		r := subscribe(t, h, "users.42")
		publish(t, h, "users.42", "hi")
		publish(t, h, "users.43", "no")
		r.want(t, h, "users.42=hi")
	})
	t.Run("SingleSegmentWildcard", func(t *testing.T) {
		h := newHarness(t)
		r := subscribe(t, h, "users.*")
		publish(t, h, "users.42", "a")
		publish(t, h, "users.42.profile", "b")
		publish(t, h, "users", "c")
		r.want(t, h, "users.42=a")
	})
	t.Run("TrailingWildcard", func(t *testing.T) {
		h := newHarness(t)
		r := subscribe(t, h, "users.>")
		publish(t, h, "users", "a")
		publish(t, h, "users.42", "b")
		publish(t, h, "users.42.profile", "c")
		publish(t, h, "orders.1", "d")
		r.want(t, h, "users.42=b", "users.42.profile=c")
	})
	t.Run("OverlappingSubscribersEachOnce", func(t *testing.T) {
		h := newHarness(t)
		exact := subscribe(t, h, "e.1")
		star := subscribe(t, h, "e.*")
		rest := subscribe(t, h, ">")
		publish(t, h, "e.1", "x")
		exact.want(t, h, "e.1=x")
		star.want(t, h, "e.1=x")
		rest.want(t, h, "e.1=x")
	})
	t.Run("Unsubscribe", func(t *testing.T) {
		h := newHarness(t)
		gone := subscribe(t, h, "c.1")
		stay := subscribe(t, h, "c.*")
		publish(t, h, "c.1", "before")
		gone.want(t, h, "c.1=before")
		gone.unsub()
		gone.unsub() // idempotent
		sync0(h)
		publish(t, h, "c.1", "after")
		stay.want(t, h, "c.1=before", "c.1=after")
		gone.want(t, h, "c.1=before") // nothing after unsubscribe
	})
}

// recorder collects deliveries as "channel=payload" strings.
type recorder struct {
	mu    sync.Mutex
	got   []string
	unsub func()
}

func subscribe(t *testing.T, h Harness, pattern string) *recorder {
	t.Helper()
	r := &recorder{}
	unsub, err := h.Bus.Subscribe(pattern, func(channel string, payload []byte) {
		r.mu.Lock()
		r.got = append(r.got, channel+"="+string(payload))
		r.mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Subscribe(%q): %v", pattern, err)
	}
	r.unsub = unsub
	t.Cleanup(unsub)
	sync0(h)
	return r
}

func publish(t *testing.T, h Harness, channel, payload string) {
	t.Helper()
	if err := h.Bus.Publish(channel, []byte(payload)); err != nil {
		t.Fatalf("Publish(%q): %v", channel, err)
	}
}

func sync0(h Harness) {
	if h.Sync != nil {
		h.Sync()
	}
}

// want waits until the recorder holds exactly the wanted deliveries
// since subscribe
// (order-insensitive), then waits a short settle period and checks
// nothing extra arrived.
func (r *recorder) want(t *testing.T, h Harness, want ...string) {
	t.Helper()
	sync0(h)
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		n := len(r.got)
		r.mu.Unlock()
		if n >= len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.Sync != nil {
		time.Sleep(50 * time.Millisecond) // async bus: let stragglers land
	}
	r.mu.Lock()
	got := append([]string(nil), r.got...)
	r.mu.Unlock()
	if !sameMultiset(got, want) {
		t.Errorf("deliveries = %q, want %q", got, want)
	}
}

func sameMultiset(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}
