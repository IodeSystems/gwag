//go:build e2e

package harness

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"testing"
	"time"
)

// PollInterval is how often Eventually and Consistently re-check.
var PollInterval = 100 * time.Millisecond

type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

// Permanent marks an error that no amount of waiting fixes (a process
// exited, a request was refused): Eventually stops at once.
func Permanent(err error) error { return permanentError{err} }

// Poll calls fn until it returns nil, returns a Permanent error, or
// timeout passes. It returns nil or the last error, annotated.
func Poll(timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	start := time.Now()
	n := 0
	for {
		n++
		err := fn()
		if err == nil {
			return nil
		}
		var perm permanentError
		if errors.As(err, &perm) {
			return fmt.Errorf("gave up after %s (%d tries): %w", time.Since(start).Round(time.Millisecond), n, perm.err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not true after %s (%d tries); last error: %w", timeout, n, err)
		}
		time.Sleep(PollInterval)
	}
}

// Eventually polls fn until it returns nil. On timeout, or on a
// Permanent error, the test fails with desc and fn's last error.
func Eventually(t testing.TB, timeout time.Duration, desc string, fn func() error) {
	t.Helper()
	if err := Poll(timeout, fn); err != nil {
		t.Fatalf("eventually %s: %v", desc, err)
	}
}

// Consistently calls fn for the whole duration and fails the test the
// first time it returns an error.
func Consistently(t testing.TB, d time.Duration, desc string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for n := 1; ; n++ {
		if err := fn(); err != nil {
			t.Fatalf("consistently %s: failed on check %d: %v", desc, n, err)
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(PollInterval)
	}
}

// Ctx returns a context that ends after d or when the test ends.
func Ctx(t testing.TB, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

var (
	portsMu sync.Mutex
	ports   = map[int]bool{}
)

// FreePort returns a TCP port on 127.0.0.1 that was free a moment ago
// and has not been handed out before in this test binary. Another
// process can still take it before the caller binds it; a process
// that fails to bind shows that in its log.
//
// The port is drawn from freePortMin..freePortMax, below the ephemeral
// range Linux (32768-60999) and macOS (49152-65535) give outgoing
// connections. A port from 127.0.0.1:0 is in that range: a node
// started later than its port was picked (cluster.NewNode) lost it to
// an outgoing connection in an S13 run ("bind: address already in
// use"); a cluster of six opens many.
func FreePort(t testing.TB) int {
	t.Helper()
	for i := 0; i < 1000; i++ {
		port := freePortMin + rand.IntN(freePortMax-freePortMin+1)
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = l.Close()
		portsMu.Lock()
		used := ports[port]
		ports[port] = true
		portsMu.Unlock()
		if !used {
			return port
		}
	}
	t.Fatalf("harness: no unused free port in %d-%d after 1000 tries", freePortMin, freePortMax)
	return 0
}

const (
	freePortMin = 20000
	freePortMax = 32767
)
