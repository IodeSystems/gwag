//go:build e2e

package e2e

import (
	"fmt"
	"os"
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

// TestKnownBugCodeLabelCase: the same upstream failure (an internal
// error) gets code="Internal" on a proto dispatch but code="INTERNAL"
// on OpenAPI and downstream-GraphQL dispatches (classifyError uses
// grpc codes.Code.String for one and the gateway's own Code.String
// for the other), so one PromQL selector cannot count errors across
// kinds.
func TestKnownBugCodeLabelCase(t *testing.T) {
	knownBug(t, "dispatch code label is Internal for proto, INTERNAL for OpenAPI/GraphQL")
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
	if len(codes) != 1 {
		t.Errorf("one failure class, %d code labels: %v\n%s", len(codes), codes, after.Describe(dispatchCount, nil))
	}
}
