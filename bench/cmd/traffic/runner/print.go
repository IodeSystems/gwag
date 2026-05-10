package runner

import (
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"sync/atomic"
	"text/tabwriter"
	"time"
)

// printClientSummary renders the per-target row + per-target detail
// blocks (example bodies + error breakdown).
func printClientSummary(targets []Target, stats []*Stats, elapsed time.Duration) {
	fmt.Println()
	fmt.Printf("=== client-side (over %s) ===\n", elapsed.Round(time.Millisecond))

	singleTarget := len(targets) == 1
	if singleTarget {
		fmt.Printf("  target: %s\n", targets[0].Label)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if singleTarget {
		fmt.Fprintln(tw, "  RPS\tP50\tP95\tP99\tOK\tERRS\tCODES")
	} else {
		fmt.Fprintln(tw, "  TARGET\tRPS\tP50\tP95\tP99\tOK\tERRS\tCODES")
	}
	for ti, t := range targets {
		s := stats[ti]
		count := atomic.LoadUint64(&s.count)
		errs := s.totalErrs()
		s.mu.Lock()
		ls := append([]time.Duration(nil), s.latencies...)
		codeSnapshot := copyStrCounts(s.codes)
		s.mu.Unlock()
		var p50s, p95s, p99s string
		if len(ls) == 0 {
			p50s, p95s, p99s = "-", "-", "-"
		} else {
			sort.Slice(ls, func(i, j int) bool { return ls[i] < ls[j] })
			p50s = fmtSeconds(pct(ls, 0.5))
			p95s = fmtSeconds(pct(ls, 0.95))
			p99s = fmtSeconds(pct(ls, 0.99))
		}
		rps := float64(count+errs) / elapsed.Seconds()
		if singleTarget {
			fmt.Fprintf(tw, "  %.1f\t%s\t%s\t%s\t%d\t%d\t%s\n",
				rps, p50s, p95s, p99s, count, errs, formatCodeMap(codeSnapshot))
		} else {
			fmt.Fprintf(tw, "  %s\t%.1f\t%s\t%s\t%s\t%d\t%d\t%s\n",
				t.Label, rps, p50s, p95s, p99s, count, errs, formatCodeMap(codeSnapshot))
		}
	}
	tw.Flush()

	for ti, t := range targets {
		s := stats[ti]
		errs := s.totalErrs()
		s.mu.Lock()
		bodies := make(map[string]string, len(s.bodyByCode))
		for k, v := range s.bodyByCode {
			bodies[k] = v
		}
		s.mu.Unlock()

		if len(bodies) == 0 && errs == 0 {
			continue
		}
		if singleTarget {
			fmt.Println()
		} else {
			fmt.Printf("\n  %s\n", t.Label)
		}
		if len(bodies) > 0 {
			fmt.Println("  example responses:")
			labels := make([]string, 0, len(bodies))
			for k := range bodies {
				labels = append(labels, k)
			}
			sort.Strings(labels)
			for _, lab := range labels {
				fmt.Printf("    [%s] %s\n", lab, bodies[lab])
			}
		}
		if errs > 0 {
			fmt.Println("  error breakdown:")
			for _, cat := range []ErrCategory{ErrDrop, ErrTransport, ErrHTTP, ErrEnvelope} {
				n := s.errs[cat]
				if n == 0 {
					continue
				}
				fmt.Printf("    %-9s %d\n", cat, n)
				for _, msg := range s.samples[cat] {
					fmt.Printf("      sample: %s\n", msg)
				}
			}
		}
	}
}

// PrintCompare emits a single-row-per-pass table summarising the
// passes that just ran (`gateway` vs `direct`, typically) with a
// final delta row showing absolute + percentage change. Latencies
// are pooled across all targets within a pass so multi-gateway runs
// collapse to a single comparable row.
func PrintCompare(passes ...PassResult) {
	if len(passes) < 2 {
		return
	}
	type aggRow struct {
		label                string
		count                uint64
		errs                 uint64
		elapsed              time.Duration
		p50, p95, p99, pmean time.Duration
	}
	rows := make([]aggRow, 0, len(passes))
	for _, p := range passes {
		var ls []time.Duration
		var count, errs uint64
		for _, s := range p.Stats {
			s.mu.Lock()
			ls = append(ls, s.latencies...)
			errs += s.totalErrs()
			s.mu.Unlock()
			count += atomic.LoadUint64(&s.count)
		}
		row := aggRow{label: p.Label, count: count, errs: errs, elapsed: p.Elapsed}
		if len(ls) > 0 {
			sort.Slice(ls, func(i, j int) bool { return ls[i] < ls[j] })
			row.p50 = pct(ls, 0.5)
			row.p95 = pct(ls, 0.95)
			row.p99 = pct(ls, 0.99)
			var sum time.Duration
			for _, l := range ls {
				sum += l
			}
			row.pmean = sum / time.Duration(len(ls))
		}
		rows = append(rows, row)
	}
	fmt.Println()
	fmt.Println("=== gateway vs direct ===")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  PASS\tRPS\tMEAN\tP50\tP95\tP99\tOK\tERRS")
	for _, r := range rows {
		rps := 0.0
		if r.elapsed > 0 {
			rps = float64(r.count+r.errs) / r.elapsed.Seconds()
		}
		fmt.Fprintf(tw, "  %s\t%.1f\t%s\t%s\t%s\t%s\t%d\t%d\n",
			r.label, rps, fmtSeconds(r.pmean), fmtSeconds(r.p50),
			fmtSeconds(r.p95), fmtSeconds(r.p99), r.count, r.errs)
	}
	if len(rows) >= 2 {
		base := rows[len(rows)-1] // direct (last)
		gw := rows[0]              // gateway (first)
		fmt.Fprintf(tw, "  Δ (gateway − direct)\t-\t%s (%s)\t%s (%s)\t%s (%s)\t%s (%s)\t-\t-\n",
			fmtSeconds(gw.pmean-base.pmean), pctDelta(gw.pmean, base.pmean),
			fmtSeconds(gw.p50-base.p50), pctDelta(gw.p50, base.p50),
			fmtSeconds(gw.p95-base.p95), pctDelta(gw.p95, base.p95),
			fmtSeconds(gw.p99-base.p99), pctDelta(gw.p99, base.p99),
		)
	}
	tw.Flush()
}

// pctDelta renders the relative change of `a` vs `base` as a signed
// percentage; "-" when base is zero or unset.
func pctDelta(a, base time.Duration) string {
	if base <= 0 {
		return "-"
	}
	frac := (float64(a) - float64(base)) / float64(base) * 100
	return fmt.Sprintf("%+.1f%%", frac)
}

// printConcurrencyAdvisor warns when the configured --concurrency ×
// observed-p50 < target RPS — Little's law says the bench client
// itself is then the throughput cap, not the gateway. Suggests a
// floor with 1.5× headroom so the next run gives the gateway some
// breathing room above the bare minimum.
func printConcurrencyAdvisor(targets []Target, stats []*Stats, opts Options) {
	if opts.RPS <= 0 || opts.Concurrency <= 0 {
		return
	}
	var hdr bool
	for ti, s := range stats {
		s.mu.Lock()
		ls := append([]time.Duration(nil), s.latencies...)
		s.mu.Unlock()
		if len(ls) < 50 {
			continue
		}
		slices.Sort(ls)
		p50 := pct(ls, 0.5)
		if p50 == 0 {
			continue
		}
		capacity := float64(opts.Concurrency) / p50.Seconds()
		if capacity >= float64(opts.RPS)*0.95 {
			continue
		}
		if !hdr {
			fmt.Println()
			fmt.Println("=== concurrency advisor ===")
			hdr = true
		}
		suggested := int(math.Ceil(float64(opts.RPS) * p50.Seconds() * 1.5))
		label := ""
		if len(targets) > 1 {
			label = " [" + targets[ti].Label + "]"
		}
		fmt.Printf("  --rps=%d × p50=%s × --concurrency=%d ≈ %.0f rps capacity%s; raise --concurrency to ≥%d (or pass 0 for auto).\n",
			opts.RPS, fmtSeconds(p50), opts.Concurrency, capacity, label, suggested)
	}
}

func copyStrCounts(src map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func formatCodeMap(m map[string]uint64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return joinStr(parts, " ")
}
