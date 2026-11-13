//go:build e2e

package harness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// WaitGauge scrapes until the sum of the series of name matching
// labels equals want. A gauge is computed at scrape time from the
// admin views, so it follows the registry without lag; the poll covers
// the moment between a request returning and the registry settling.
func (g *Gateway) WaitGauge(t testing.TB, name string, labels Labels, want float64, timeout time.Duration) *Scrape {
	t.Helper()
	var last *Scrape
	Eventually(t, timeout, fmt.Sprintf("%s: %s%s = %v", g.Name, name, labels, want), func() error {
		s, err := g.Scrape(context.Background())
		if err != nil {
			return err
		}
		last = s
		if got := s.Counter(name, labels); got != want {
			return fmt.Errorf("got %v\nseries now:\n%s", got, s.Describe(name, nil))
		}
		return nil
	})
	return last
}

// WaitNoSeries scrapes until no series of name matches labels. The
// lifecycle gauges exist only while their line or instance does.
func (g *Gateway) WaitNoSeries(t testing.TB, name string, labels Labels, timeout time.Duration) {
	t.Helper()
	Eventually(t, timeout, fmt.Sprintf("%s: no series %s%s", g.Name, name, labels), func() error {
		s, err := g.Scrape(context.Background())
		if err != nil {
			return err
		}
		if n := len(s.Samples(name, labels)); n > 0 {
			return fmt.Errorf("%d series still exported\n%s", n, s.Describe(name, labels))
		}
		return nil
	})
}

// WaitCounterAtLeast scrapes until the counter's delta against before
// is at least min. For counters fed by a 2s sampler, where the exact
// number of flips depends on timing.
func (g *Gateway) WaitCounterAtLeast(t testing.TB, before *Scrape, name string, labels Labels, min float64, timeout time.Duration) *Scrape {
	t.Helper()
	var last *Scrape
	Eventually(t, timeout, fmt.Sprintf("%s: %s%s delta >= %v", g.Name, name, labels, min), func() error {
		s, err := g.Scrape(context.Background())
		if err != nil {
			return err
		}
		last = s
		if got := Delta(before, s, name, labels); got < min {
			return fmt.Errorf("delta %v\nseries now:\n%s", got, s.Describe(name, nil))
		}
		return nil
	})
	return last
}

// WaitInstanceAny waits until the gateway lists instance id on
// (namespace, version) in any state, and returns that state.
func (a *Admin) WaitInstanceAny(namespace, version, id string, timeout time.Duration) string {
	t := a.g.env.t
	t.Helper()
	var state string
	Eventually(t, timeout, fmt.Sprintf("%s lists %s on %s/%s", a.g.Name, id, namespace, version), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ss, err := a.Services(ctx)
		if err != nil {
			return err
		}
		if svc := ss.Service(namespace, version); svc != nil {
			if in := svc.Instance(id); in != nil {
				state = in.State
				return nil
			}
		}
		return fmt.Errorf("not listed")
	})
	return state
}

// SchemaSDL fetches the public GraphQL SDL (GET /api/schema/graphql).
// service, when set, is a selector such as "orders" or "orders:v1".
func (g *Gateway) SchemaSDL(ctx context.Context, service string) (string, error) {
	u := g.SchemaURL(service)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", &HTTPError{Method: http.MethodGet, Path: u, Status: resp.StatusCode, Body: string(b)}
	}
	return string(b), nil
}

// SchemaURL is the URL of the SDL export, for `gwag ctl schema diff`.
func (g *Gateway) SchemaURL(service string) string {
	u := g.BaseURL() + "/api/schema/graphql"
	if service != "" {
		u += "?service=" + url.QueryEscape(service)
	}
	return u
}
