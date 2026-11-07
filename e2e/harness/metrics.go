//go:build e2e

package harness

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// MetricPrefix is the gateway's default metric name prefix.
const MetricPrefix = "go_api_gateway_"

// Labels selects series: a series matches when it has every listed
// label with the listed value. Unlisted labels match anything.
type Labels map[string]string

// Scrape is one parsed /api/metrics answer.
type Scrape struct {
	At       time.Time
	Families map[string]*dto.MetricFamily
}

// Sample is one matched series.
type Sample struct {
	Labels Labels
	Value  float64
}

// Scrape fetches and parses the gateway's /api/metrics.
func (g *Gateway) Scrape(ctx context.Context) (*Scrape, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL()+"/api/metrics", nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/api/metrics: HTTP %d", resp.StatusCode)
	}
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse /api/metrics: %w", err)
	}
	return &Scrape{At: time.Now(), Families: fams}, nil
}

// MustScrape is Scrape that fails the test on error.
func (g *Gateway) MustScrape(t testing.TB) *Scrape {
	t.Helper()
	s, err := g.Scrape(Ctx(t, 10*time.Second))
	if err != nil {
		t.Fatalf("scrape %s: %v", g.Name, err)
	}
	return s
}

// Samples returns every series of name that matches labels. name may
// omit MetricPrefix. For a histogram or summary, name it with a
// _count or _sum suffix (e.g. "dispatch_duration_seconds_count").
func (s *Scrape) Samples(name string, labels Labels) []Sample {
	fam, field := s.family(name)
	if fam == nil {
		return nil
	}
	var out []Sample
	for _, m := range fam.GetMetric() {
		ls := Labels{}
		for _, lp := range m.GetLabel() {
			ls[lp.GetName()] = lp.GetValue()
		}
		if !ls.matches(labels) {
			continue
		}
		out = append(out, Sample{Labels: ls, Value: value(fam.GetType(), m, field)})
	}
	return out
}

// Counter returns the sum over every series of name that matches
// labels (0 when none). It reads counters, gauges and untyped metrics,
// and histogram/summary _count and _sum.
func (s *Scrape) Counter(name string, labels Labels) float64 {
	var sum float64
	for _, smp := range s.Samples(name, labels) {
		sum += smp.Value
	}
	return sum
}

// Has reports whether the scrape has a metric family for name.
func (s *Scrape) Has(name string) bool {
	fam, _ := s.family(name)
	return fam != nil
}

// Delta is after.Counter - before.Counter for name and labels.
func Delta(before, after *Scrape, name string, labels Labels) float64 {
	return after.Counter(name, labels) - before.Counter(name, labels)
}

// Describe lists the series of name matching labels, one per line;
// for failure messages.
func (s *Scrape) Describe(name string, labels Labels) string {
	var lines []string
	for _, smp := range s.Samples(name, labels) {
		lines = append(lines, fmt.Sprintf("  %s %v", smp.Labels, smp.Value))
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "  (no series)"
	}
	return strings.Join(lines, "\n")
}

func (s *Scrape) family(name string) (*dto.MetricFamily, string) {
	for _, n := range []string{name, MetricPrefix + name} {
		if f, ok := s.Families[n]; ok {
			return f, ""
		}
		for _, suf := range []string{"_count", "_sum"} {
			if base, ok := strings.CutSuffix(n, suf); ok {
				if f, ok := s.Families[base]; ok {
					return f, suf
				}
			}
		}
	}
	return nil, ""
}

func value(t dto.MetricType, m *dto.Metric, field string) float64 {
	switch t {
	case dto.MetricType_COUNTER:
		return m.GetCounter().GetValue()
	case dto.MetricType_GAUGE:
		return m.GetGauge().GetValue()
	case dto.MetricType_HISTOGRAM:
		if field == "_sum" {
			return m.GetHistogram().GetSampleSum()
		}
		return float64(m.GetHistogram().GetSampleCount())
	case dto.MetricType_SUMMARY:
		if field == "_sum" {
			return m.GetSummary().GetSampleSum()
		}
		return float64(m.GetSummary().GetSampleCount())
	default:
		return m.GetUntyped().GetValue()
	}
}

// Want is one expected metric movement: the series of a name matching
// Labels moved by exactly Delta (summed over matches).
type Want struct {
	Labels Labels
	Delta  float64
}

// WaitDeltas scrapes until every want holds against before, and
// returns that scrape. Metrics can land just after a response is
// written, so a single scrape is not enough. On timeout the test fails
// with each mismatch and the series it saw.
func (g *Gateway) WaitDeltas(t testing.TB, before *Scrape, name string, wants []Want, timeout time.Duration) *Scrape {
	t.Helper()
	var last *Scrape
	Eventually(t, timeout, fmt.Sprintf("%s: %s deltas", g.Name, name), func() error {
		s, err := g.Scrape(context.Background())
		if err != nil {
			return err
		}
		last = s
		var bad []string
		for _, w := range wants {
			if got := Delta(before, s, name, w.Labels); got != w.Delta {
				bad = append(bad, fmt.Sprintf("%s: delta %v, want %v", w.Labels, got, w.Delta))
			}
		}
		if len(bad) > 0 {
			return fmt.Errorf("%s\nseries now:\n%s", strings.Join(bad, "\n"), s.Describe(name, nil))
		}
		return nil
	})
	return last
}

func (l Labels) matches(want Labels) bool {
	for k, v := range want {
		if l[k] != v {
			return false
		}
	}
	return true
}

func (l Labels) String() string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, l[k])
	}
	return "{" + strings.Join(parts, ",") + "}"
}
