// Package runner is the format-agnostic core of `bench traffic`. It
// drives a rate loop per target, owns request stats and result
// printing, and snapshots the gateway's /api/metrics before+after
// the run for the per-backend table.
//
// Adapters (graphql / grpc / openapi) plug in via Target.Fire — a
// per-request closure that does the actual wire work and records
// outcomes onto *Stats. The runner is responsible for everything
// else: ticker, concurrency cap, drop accounting, summary tables.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrCategory groups errors so the summary explains *what* failed
// rather than just how many. Drops are concurrency saturation;
// transport are TCP/timeout failures (or gRPC UNAVAILABLE);
// httpStatus is 4xx/5xx; envelope is a wire-200 with a non-empty
// error envelope (GraphQL `errors[]` or similar).
type ErrCategory string

const (
	ErrDrop      ErrCategory = "drop"
	ErrTransport ErrCategory = "transport"
	ErrHTTP      ErrCategory = "http"
	ErrEnvelope  ErrCategory = "envelope"
)

const sampleErrMessages = 3

// Stats accumulates per-target outcomes. Adapters call RecordCode +
// RecordOK on success, RecordCode + RecordErr on failure. RecordCode
// is called regardless because the wire outcome (HTTP status, gRPC
// status code) is interesting on both paths — a 200 with a non-empty
// envelope is an error but the 200 still happened.
type Stats struct {
	count uint64

	mu         sync.Mutex
	errs       map[ErrCategory]uint64
	codes      map[string]uint64 // wire outcome label → count
	latencies  []time.Duration
	samples    map[ErrCategory][]string
	bodyByCode map[string]string // first body per code label, truncated
}

func NewStats() *Stats {
	return &Stats{
		errs:       map[ErrCategory]uint64{},
		codes:      map[string]uint64{},
		samples:    map[ErrCategory][]string{},
		bodyByCode: map[string]string{},
	}
}

// RecordCode bumps the wire-outcome code counter. Adapters call this
// for every completed request before deciding ok/err.
func (s *Stats) RecordCode(label string) {
	s.mu.Lock()
	s.codes[label]++
	s.mu.Unlock()
}

// RecordOK marks a successful request and adds its latency.
func (s *Stats) RecordOK(latency time.Duration) {
	atomic.AddUint64(&s.count, 1)
	s.mu.Lock()
	s.latencies = append(s.latencies, latency)
	s.mu.Unlock()
}

// RecordErr increments the error category and saves up to
// sampleErrMessages of the message text.
func (s *Stats) RecordErr(cat ErrCategory, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[cat]++
	if len(s.samples[cat]) < sampleErrMessages {
		s.samples[cat] = append(s.samples[cat], msg)
	}
}

// RecordBody saves the first non-empty body seen for a given outcome
// label. Callers truncate before passing in; this is just a "first
// wins" cache so the summary has one example per label.
func (s *Stats) RecordBody(label, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.bodyByCode[label]; exists {
		return
	}
	s.bodyByCode[label] = body
}

func (s *Stats) totalErrs() uint64 {
	var n uint64
	for _, v := range s.errs {
		n += v
	}
	return n
}

// Options are the cross-format flags. Adapters parse their own flags
// for query/service/method/args; runner just gets the shared knobs.
//
// Concurrency = 0 means "auto" — runner picks max(64, RPS/20). Pin a
// higher floor explicitly when running at multi-ms p50 or fan-out
// dispatch where 5% of RPS isn't enough headroom.
//
// Shards = 0 means "auto" — runner picks ceil(RPS/1500) so each
// driver goroutine ticks well below the empirical single-goroutine
// ceiling (~3.2k Hz on Linux). Without sharding, a single ticker +
// goroutine-spawn loop caps at ~3k RPS regardless of how much
// headroom the gateway has — every measurement above ~3k turned
// into client-bound numbers. Pin Shards explicitly to fix the
// number for repro / CI runs.
type Options struct {
	RPS           int
	Duration      time.Duration
	Concurrency   int
	Shards        int
	ServerMetrics bool
}

// Target is one (label, fire, metrics-url) triple the runner drives.
// Label shows up in the summary header for multi-target runs (URL,
// or "ns.method" for grpc/openapi). MetricsURL is the gateway
// /api/metrics endpoint; empty disables server-side capture for that
// target. Multiple targets sharing a MetricsURL are deduped.
type Target struct {
	Label      string
	MetricsURL string
	Fire       func(ctx context.Context, s *Stats)
}

// PassResult is the captured outcome of one runner pass: per-target
// stats, wall-clock duration, and the optional gateway-metrics
// snapshots. Callers print it via PrintPass; --direct adapters
// capture two and pass them to PrintCompare.
type PassResult struct {
	Label    string // "gateway", "direct", etc.; used in compare output
	Targets  []Target
	Stats    []*Stats
	Elapsed  time.Duration
	// EffectiveConcurrency is the in-flight cap actually applied —
	// equal to opts.Concurrency on the way in, or autoConcurrency(rps)
	// when the caller passed 0. Reported so the JSON sidecar carries
	// the value the run used rather than the symbolic 0.
	EffectiveConcurrency int
	// EffectiveShards is the driver-goroutine count Run actually used
	// (auto-resolved when opts.Shards was 0). Reported so the JSON
	// sidecar + report writer can show the value rather than the
	// symbolic 0; useful when chasing client-bound vs gateway-bound
	// regressions.
	EffectiveShards int
	PreSnap         map[string]*metricFamily
	PostSnap        map[string]*metricFamily
}

// Run blocks for opts.Duration (or until SIGINT/SIGTERM), drives each
// target at opts.RPS with opts.Concurrency in-flight, and returns a
// PassResult. The caller prints via PrintPass / PrintCompare.
func Run(opts Options, label string, targets []Target) (PassResult, error) {
	if len(targets) == 0 {
		return PassResult{}, errors.New("at least one target is required")
	}
	if opts.RPS <= 0 {
		return PassResult{}, errors.New("--rps must be > 0")
	}
	if opts.Concurrency < 0 {
		return PassResult{}, errors.New("--concurrency must be ≥ 0 (0 = auto)")
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = autoConcurrency(opts.RPS)
	}

	stats := make([]*Stats, len(targets))
	for i := range stats {
		stats[i] = NewStats()
	}

	var preSnap map[string]*metricFamily
	if opts.ServerMetrics {
		preSnap = collectMetrics(targets)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	tctx, tcancel := context.WithTimeout(ctx, opts.Duration)
	defer tcancel()

	shardCount := opts.Shards
	if shardCount <= 0 {
		shardCount = autoShardCount(opts.RPS)
	}

	runStart := time.Now()
	wg := sync.WaitGroup{}
	for ti, target := range targets {
		ti := ti
		fire := target.Fire
		sem := make(chan struct{}, opts.Concurrency)
		// Period for each shard: at total RPS R split across N shards,
		// each shard fires every N/R seconds → period * N = 1s / R.
		// Using time.Duration math (ns) avoids the rounding cliff a
		// naive `time.Second/R/N` would hit at high RPS.
		period := time.Duration(int64(time.Second) * int64(shardCount) / int64(opts.RPS))
		if period < time.Microsecond {
			period = time.Microsecond
		}
		for s := 0; s < shardCount; s++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ticker := time.NewTicker(period)
				defer ticker.Stop()
				for {
					select {
					case <-tctx.Done():
						return
					case <-ticker.C:
						select {
						case sem <- struct{}{}:
							go func() {
								defer func() { <-sem }()
								fire(tctx, stats[ti])
							}()
						default:
							stats[ti].RecordErr(ErrDrop, "concurrency saturated; --concurrency too low or server too slow")
						}
					}
				}
			}()
		}
	}
	wg.Wait()
	elapsed := time.Since(runStart)

	var postSnap map[string]*metricFamily
	if opts.ServerMetrics {
		postSnap = collectMetrics(targets)
	}

	return PassResult{
		Label:                label,
		Targets:              targets,
		Stats:                stats,
		Elapsed:              elapsed,
		EffectiveConcurrency: opts.Concurrency,
		EffectiveShards:      shardCount,
		PreSnap:              preSnap,
		PostSnap:             postSnap,
	}, nil
}

// PrintPass renders the client-side, server-side (when ServerMetrics
// is on), and concurrency-advisor blocks for one PassResult.
func PrintPass(opts Options, r PassResult) {
	if r.Label != "" {
		fmt.Println()
		fmt.Printf("### pass: %s\n", r.Label)
	}
	printClientSummary(r.Targets, r.Stats, r.Elapsed)
	if opts.ServerMetrics {
		printServerSummary(r.PreSnap, r.PostSnap, r.Elapsed)
	}
	printConcurrencyAdvisor(r.Targets, r.Stats, opts)
}
