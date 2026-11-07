//go:build e2e

package harness

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// LoadOpts configures a load run.
type LoadOpts struct {
	// Workers is the number of concurrent callers. Default 4.
	Workers int
	// Interval is each worker's pause between calls. 0 = back to back.
	Interval time.Duration
	// CallTimeout bounds each call's context. Default 10s.
	CallTimeout time.Duration
}

// Load is a running load generator. Stop it to get the result.
type Load struct {
	ok, failed atomic.Int64
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	start      time.Time

	mu     sync.Mutex
	errs   map[string]int
	stopAt time.Time
	once   sync.Once
}

// LoadResult counts calls by outcome.
type LoadResult struct {
	OK, Failed int64
	Duration   time.Duration
	// Errors counts failures by message (messages are cut at 200
	// characters; at most 20 distinct ones are kept).
	Errors map[string]int
}

// StartLoad runs fn from opts.Workers goroutines until Stop. fn gets
// a per-call context and the worker index; a non-nil error counts the
// call as failed. Stop is also registered as a test cleanup.
func StartLoad(t testing.TB, opts LoadOpts, fn func(ctx context.Context, worker int) error) *Load {
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = 10 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &Load{cancel: cancel, start: time.Now(), errs: map[string]int{}}
	for w := 0; w < opts.Workers; w++ {
		l.wg.Add(1)
		go func(w int) {
			defer l.wg.Done()
			for ctx.Err() == nil {
				cctx, ccancel := context.WithTimeout(ctx, opts.CallTimeout)
				err := fn(cctx, w)
				ccancel()
				if ctx.Err() != nil && err != nil {
					// Stop cancelled this call mid-flight: not a failure.
					return
				}
				if err != nil {
					l.failed.Add(1)
					l.recordErr(err)
				} else {
					l.ok.Add(1)
				}
				if opts.Interval > 0 {
					select {
					case <-ctx.Done():
					case <-time.After(opts.Interval):
					}
				}
			}
		}(w)
	}
	t.Cleanup(func() { l.Stop() })
	return l
}

func (l *Load) recordErr(err error) {
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.errs[msg]; ok || len(l.errs) < 20 {
		l.errs[msg]++
	}
}

// Counts returns the ok and failed counts so far.
func (l *Load) Counts() (ok, failed int64) { return l.ok.Load(), l.failed.Load() }

// WaitOK waits until at least n calls have succeeded since start.
func (l *Load) WaitOK(t testing.TB, n int64, timeout time.Duration) {
	t.Helper()
	Eventually(t, timeout, fmt.Sprintf("load reaches %d ok calls", n), func() error {
		if ok, failed := l.Counts(); ok < n {
			return fmt.Errorf("ok=%d failed=%d", ok, failed)
		}
		return nil
	})
}

// Stop stops every worker, waits for in-flight calls, and returns the
// result. Safe to call more than once.
func (l *Load) Stop() LoadResult {
	l.once.Do(func() {
		l.cancel()
		l.wg.Wait()
		l.mu.Lock()
		l.stopAt = time.Now()
		l.mu.Unlock()
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	errs := make(map[string]int, len(l.errs))
	for k, v := range l.errs {
		errs[k] = v
	}
	return LoadResult{OK: l.ok.Load(), Failed: l.failed.Load(), Duration: l.stopAt.Sub(l.start), Errors: errs}
}

// String summarises the result for logs and failure messages.
func (r LoadResult) String() string {
	s := fmt.Sprintf("ok=%d failed=%d in %s", r.OK, r.Failed, r.Duration.Round(time.Millisecond))
	if len(r.Errors) == 0 {
		return s
	}
	var lines []string
	for msg, n := range r.Errors {
		lines = append(lines, fmt.Sprintf("  %dx %s", n, msg))
	}
	sort.Strings(lines)
	return s + "\n" + strings.Join(lines, "\n")
}

// RequireNoFailures fails the test if any call failed, or if no call
// succeeded.
func (r LoadResult) RequireNoFailures(t testing.TB) {
	t.Helper()
	if r.Failed > 0 || r.OK == 0 {
		t.Fatalf("load: want zero failed and some ok calls, got %s", r)
	}
}
