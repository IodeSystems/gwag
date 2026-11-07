//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// knownBug skips a test that reproduces an open gateway bug, so the
// suite stays green until the fix lands. E2E_KNOWN_BUGS=1 runs them;
// they fail until the bug is fixed, then the skip should be removed.
func knownBug(t *testing.T, summary string) {
	t.Helper()
	if os.Getenv("E2E_KNOWN_BUGS") != "1" {
		t.Skipf("known gateway bug, run with E2E_KNOWN_BUGS=1: %s", summary)
	}
}

// TestKnownBugSelectionNotApplied: for OpenAPI and downstream-GraphQL
// services the gateway returns the upstream object whole, not the
// fields the query selected. A proto service answers only the
// selection. GraphQL requires the response to hold exactly the
// selected fields.
func TestKnownBugSelectionNotApplied(t *testing.T) {
	knownBug(t, "OpenAPI and downstream-GraphQL results carry unselected fields")
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{})
	env.Fixture(gw, harness.FixtureOpts{Namespace: "orders"})
	env.Fixture(gw, harness.FixtureOpts{Kind: harness.KindOpenAPI, Namespace: "billing"})
	env.Fixture(gw, harness.FixtureOpts{Kind: harness.KindGraphQL, Namespace: "catalog"})

	for _, c := range []struct{ kind, query, path string }{
		{"proto", `{ orders { echo(msg: "x") { msg } } }`, "orders.echo"},
		{"openapi", `mutation { billing { echo(body: {msg: "x"}) { msg } } }`, "billing.echo"},
		{"graphql", `{ catalog { echo(msg: "x") { msg } } }`, "catalog.echo"},
	} {
		r := gw.MustGraphQL(t, c.query, nil)
		obj, _ := r.Path(c.path).(map[string]any)
		var keys []string
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "msg" {
			b, _ := json.Marshal(obj)
			t.Errorf("%s: selected { msg }, got fields %v: %s", c.kind, keys, b)
		}
	}
}

// TestDispatchCodeLabelCanonical: the same upstream failure (an
// internal error) carries one code label, "Internal" (the grpc
// codes.Code.String form), on proto, OpenAPI and downstream-GraphQL
// dispatches, so one PromQL selector counts errors across kinds.
func TestDispatchCodeLabelCanonical(t *testing.T) {
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{})
	fixtures := []*harness.Fixture{
		env.Fixture(gw, harness.FixtureOpts{Namespace: "orders", ErrorRate: 1}),
		env.Fixture(gw, harness.FixtureOpts{Kind: harness.KindOpenAPI, Namespace: "billing", ErrorRate: 1}),
		env.Fixture(gw, harness.FixtureOpts{Kind: harness.KindGraphQL, Namespace: "catalog", ErrorRate: 1}),
	}
	before := gw.MustScrape(t)
	for _, f := range fixtures {
		if _, err := f.Echo(harness.Ctx(t, 10*time.Second), "v1", "x"); err == nil {
			t.Fatalf("%s: want the injected error", f.Opts.Namespace)
		}
	}
	var after *harness.Scrape
	harness.Eventually(t, 10*time.Second, "three failed dispatches recorded", func() error {
		var err error
		if after, err = gw.Scrape(harness.Ctx(t, 5*time.Second)); err != nil {
			return err
		}
		if d := harness.Delta(before, after, dispatchCount, nil); d != 3 {
			return fmt.Errorf("delta %v", d)
		}
		return nil
	})
	codes := map[string]bool{}
	for _, s := range after.Samples(dispatchCount, nil) {
		if harness.Delta(before, after, dispatchCount, s.Labels) > 0 {
			codes[s.Labels["code"]] = true
		}
	}
	if len(codes) != 1 || !codes["Internal"] {
		t.Errorf("one failure class, want only code=Internal, got %v\n%s", codes, after.Describe(dispatchCount, nil))
	}
}
