// Traffic generator for the gateway bench stack.
//
// Hammers one or more GraphQL endpoints at a target rate and reports
// per-endpoint latency quantiles + error counts at the end. Use it
// alongside Grafana — this binary gives you the client view, the
// gateway's metrics give you the server view.
//
// Default query is the greeter saying hello, which the bench's
// up.sh registers automatically. Override with --query for other
// shapes. Multi-target: pass --target several times (or comma-
// separated) to spread load across a cluster.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type targetStats struct {
	count uint64
	errs  uint64

	mu        sync.Mutex
	latencies []time.Duration
}

func main() {
	var (
		rps         = flag.Int("rps", 100, "requests per second per target")
		duration    = flag.Duration("duration", 30*time.Second, "test duration")
		concurrency = flag.Int("concurrency", 16, "max concurrent in-flight per target (extras are dropped)")
		query       = flag.String("query", `{ greeter { hello(name: "world") { greeting } } }`, "GraphQL query string")
		timeout     = flag.Duration("timeout", 5*time.Second, "per-request HTTP timeout")
	)
	var targetsRaw stringFlag
	flag.Var(&targetsRaw, "target", "GraphQL endpoint URL (repeat or comma-separate for multiple)")
	flag.Parse()

	targets := splitTargets(targetsRaw)
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "at least one --target is required")
		os.Exit(2)
	}

	body, err := json.Marshal(map[string]any{"query": *query})
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal query: %v\n", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	tctx, tcancel := context.WithTimeout(ctx, *duration)
	defer tcancel()

	stats := make([]*targetStats, len(targets))
	for i := range stats {
		stats[i] = &targetStats{}
	}

	fmt.Printf("running %d req/s for %s against %d target(s)\n", *rps, duration.String(), len(targets))

	wg := sync.WaitGroup{}
	for ti, target := range targets {
		ti := ti
		target := target
		client := &http.Client{Timeout: *timeout}
		sem := make(chan struct{}, *concurrency)
		ticker := time.NewTicker(time.Second / time.Duration(*rps))
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer ticker.Stop()
			for {
				select {
				case <-tctx.Done():
					return
				case <-ticker.C:
					select {
					case sem <- struct{}{}:
						// got a slot; fire one off async.
						go func() {
							defer func() { <-sem }()
							fireOnce(tctx, client, target, body, stats[ti])
						}()
					default:
						// concurrency saturated — count as a backpressure
						// drop so the user notices when --concurrency is
						// the bottleneck rather than the server.
						atomic.AddUint64(&stats[ti].errs, 1)
					}
				}
			}
		}()
	}
	wg.Wait()

	fmt.Println()
	for ti, t := range targets {
		s := stats[ti]
		count := atomic.LoadUint64(&s.count)
		errs := atomic.LoadUint64(&s.errs)
		fmt.Printf("=== %s ===\n", t)
		fmt.Printf("  count=%d errs=%d errRate=%.2f%%\n", count, errs, errPct(count+errs, errs))
		s.mu.Lock()
		ls := append([]time.Duration(nil), s.latencies...)
		s.mu.Unlock()
		if len(ls) == 0 {
			fmt.Println("  no successful samples")
			continue
		}
		sort.Slice(ls, func(i, j int) bool { return ls[i] < ls[j] })
		fmt.Printf("  p50=%s p95=%s p99=%s max=%s\n",
			pct(ls, 0.5), pct(ls, 0.95), pct(ls, 0.99), ls[len(ls)-1])
	}
}

func fireOnce(ctx context.Context, client *http.Client, target string, body []byte, s *targetStats) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(body))
	if err != nil {
		atomic.AddUint64(&s.errs, 1)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		atomic.AddUint64(&s.errs, 1)
		return
	}
	defer resp.Body.Close()
	// Drain so the keep-alive lives. Cheap; bodies are tiny.
	_, _ = io.Copy(io.Discard, resp.Body)
	atomic.AddUint64(&s.count, 1)
	if resp.StatusCode != 200 {
		atomic.AddUint64(&s.errs, 1)
		return
	}
	s.mu.Lock()
	s.latencies = append(s.latencies, elapsed)
	s.mu.Unlock()
}

func pct(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)) * q)
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func errPct(total, errs uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(errs) / float64(total) * 100
}

// stringFlag is a flag.Value collecting repeated --target entries.
type stringFlag []string

func (s *stringFlag) String() string { return strings.Join(*s, ",") }
func (s *stringFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// splitTargets accepts a list of --target args, each of which may
// itself contain a comma-separated list. The flat result is the
// union; empties are dropped.
func splitTargets(raw stringFlag) []string {
	var out []string
	for _, r := range raw {
		for _, p := range strings.Split(r, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
