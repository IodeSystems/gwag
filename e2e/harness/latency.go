//go:build e2e

package harness

import (
	"sort"
	"sync"
	"time"
)

// Latencies collects call durations from concurrent workers and
// reports percentiles. Use Time inside a StartLoad function.
type Latencies struct {
	mu sync.Mutex
	d  []time.Duration
}

// Time runs fn, records its duration when fn succeeds, and returns
// fn's error.
func (l *Latencies) Time(fn func() error) error {
	start := time.Now()
	err := fn()
	if err == nil {
		l.mu.Lock()
		l.d = append(l.d, time.Since(start))
		l.mu.Unlock()
	}
	return err
}

// Len is the number of recorded calls.
func (l *Latencies) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.d)
}

// Reset drops every recorded duration.
func (l *Latencies) Reset() {
	l.mu.Lock()
	l.d = nil
	l.mu.Unlock()
}

// Percentile returns the p-th percentile (0..100, nearest rank) of the
// recorded durations, or 0 when none were recorded.
func (l *Latencies) Percentile(p float64) time.Duration {
	l.mu.Lock()
	s := append([]time.Duration(nil), l.d...)
	l.mu.Unlock()
	if len(s) == 0 {
		return 0
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s))*p/100+0.5) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}
