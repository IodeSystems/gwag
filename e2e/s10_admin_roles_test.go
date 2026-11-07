//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// TestS10AdminRoles: three admin readers on one gateway with a public
// and a private service.
//
//   - storefront v1 registers public (owner web); ledger v1 does not
//     (owner finance). storefront depends on ledger; ledger calls
//     storefront; Acme is declared pinning storefront [v1].
//
//     1. no token: the services list, graph and status reads show
//     storefront only, owner team without contact; the declared edge
//     to ledger and Acme are gone; ledger's traffic into storefront is
//     one "hidden" caller; dev-only reads are 401, also through
//     GraphQL. Calls to ledger through the gateway still work.
//     2. dev token: everything is listed, contacts included; external
//     dependents can be added; deprecate and drain are 403.
//     3. admin token: deprecate works; rotating the dev token retires the
//     old one at once.
func TestS10AdminRoles(t *testing.T) {
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{Environment: "e2e"})
	ctx := harness.Ctx(t, 3*time.Minute)
	admin := gw.Admin()
	anon := admin.As("")
	dev := admin.As(gw.DevToken())
	root := admin.As(gw.Token())

	ledger := env.Fixture(gw, harness.FixtureOpts{Namespace: "ledger", Owner: "finance"})
	store := env.Fixture(gw, harness.FixtureOpts{Namespace: "storefront", Owner: "web", Depends: []string{"ledger:v1"}, Args: []string{"--public"}})
	for i := 0; i < 3; i++ {
		if _, err := store.Echo(ctx, "v1", "x", harness.Caller("ledger/v1")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Echo(ctx, "v1", "x", harness.Caller("storefront/v1")); err != nil {
		t.Fatal(err)
	}
	if code, err := dev.Do(ctx, http.MethodPut, "/admin/external-dependents/acme", map[string]any{
		"name": "Acme Corp", "kind": "customer",
		"depends": []map[string]any{{"service": "storefront", "lines": []string{"v1"}}},
	}, nil); err != nil || code != http.StatusOK {
		t.Fatalf("dev PUT acme: %d %v", code, err)
	}

	t.Run("1 no token: public services only", func(t *testing.T) {
		if role, err := anon.Whoami(ctx); err != nil || role != "anonymous" {
			t.Fatalf("whoami=%q %v", role, err)
		}
		svcs, code, err := anon.Services(ctx)
		if err != nil || code != 200 {
			t.Fatalf("services: %d %v", code, err)
		}
		if len(svcs.Services) != 1 || svcs.Services[0].Namespace != "storefront" {
			t.Fatalf("services=%+v, want storefront only", svcs.Services)
		}
		s := svcs.Services[0]
		if s.Owner == nil || s.Owner.Team != "web" || s.Owner.Contact != "" || len(s.Depends) != 0 {
			t.Errorf("storefront owner=%+v depends=%+v, want team only, no depends", s.Owner, s.Depends)
		}
		if len(s.Instances) != 1 || s.Instances[0].State != "ready" || s.Instances[0].Addr != "" {
			t.Errorf("instances=%+v, want one ready, no addr", s.Instances)
		}

		harness.Eventually(t, 20*time.Second, "anonymous graph", func() error {
			g, code, err := anon.Graph(ctx, "1m")
			if err != nil || code != 200 {
				return harness.Permanent(err)
			}
			if len(g.Nodes) != 1 || g.Nodes[0].ID != "storefront/v1" {
				return harness.Permanent(fmt.Errorf("nodes=%+v, want storefront/v1", g.Nodes))
			}
			if len(g.DesiredEdges) != 0 || len(g.ExternalDependents) != 0 {
				return harness.Permanent(fmt.Errorf("desired=%+v externals=%+v, want none", g.DesiredEdges, g.ExternalDependents))
			}
			byCaller := map[string]uint64{}
			for _, e := range g.ActiveEdges {
				byCaller[e.Caller] += e.Count
				if e.Caller == "hidden" && e.ConsumerNamespace != "" {
					return harness.Permanent(fmt.Errorf("hidden edge names %q", e.ConsumerNamespace))
				}
			}
			if byCaller["hidden"] != 3 || byCaller["storefront/v1"] != 1 || byCaller["ledger/v1"] != 0 {
				return fmt.Errorf("active by caller=%v, want hidden=3 storefront/v1=1", byCaller)
			}
			return nil
		})

		for _, path := range []string{"/admin/services/stats?window=1m", "/admin/services/history?window=1m", "/admin/services/usage?window=1m"} {
			var raw map[string]any
			code, err := anon.Do(ctx, http.MethodGet, path, nil, &raw)
			if err != nil || code != 200 {
				t.Fatalf("%s: %d %v", path, code, err)
			}
			if b, _ := json.Marshal(raw); strings.Contains(string(b), "ledger") || !strings.Contains(string(b), "storefront") {
				t.Errorf("%s: %s", path, b)
			}
		}
		if code, _ := anon.Do(ctx, http.MethodGet, "/admin/services/ledger/v1/stats", nil, nil); code != http.StatusNotFound {
			t.Errorf("ledger stats: %d, want 404", code)
		}
		for _, path := range []string{"/admin/peers", "/admin/external-dependents", "/admin/instances", "/admin/mcp/schema/list"} {
			if code, _ := anon.Do(ctx, http.MethodGet, path, nil, nil); code != http.StatusUnauthorized {
				t.Errorf("%s: %d, want 401", path, code)
			}
		}

		// The self-ingested GraphQL admin surface follows the same rules.
		r, err := anon.GraphQL(ctx, `{ admin { listServices { services { namespace owner { contact } } } } }`)
		if err != nil || r.Err() != nil || strings.Contains(string(r.Data), "ledger") || strings.Contains(string(r.Data), "#web") {
			t.Errorf("GraphQL listServices: %s %v %v", r.Data, err, r.Err())
		}
		if r, err := anon.GraphQL(ctx, `{ admin { listExternalDependents { externalDependents { id } } } }`); err != nil || r.Err() == nil {
			t.Errorf("GraphQL listExternalDependents without a token: %v %v, want an error", err, r)
		}

		// Data plane unchanged: ledger answers without a token.
		if _, err := ledger.Echo(ctx, "v1", "still served"); err != nil {
			t.Errorf("ledger call: %v", err)
		}
	})

	t.Run("2 dev token: every read, external dependents", func(t *testing.T) {
		if role, _ := dev.Whoami(ctx); role != "dev" {
			t.Fatalf("whoami=%q", role)
		}
		svcs, _, err := dev.Services(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if l := svcs.Service("ledger", "v1"); l == nil || l.Owner == nil || l.Owner.Contact != "#finance" {
			t.Errorf("ledger as dev=%+v, want listed with contact", l)
		}
		g, _, err := dev.Graph(ctx, "1m")
		if err != nil || len(g.ExternalDependents) != 1 || g.Node("ledger/v1") == nil {
			t.Fatalf("dev graph: %v externals=%d", err, len(g.ExternalDependents))
		}
		if code, _ := dev.Do(ctx, http.MethodGet, "/admin/peers", nil, nil); code != 200 {
			t.Errorf("peers: %d", code)
		}
		if code, _ := dev.Do(ctx, http.MethodDelete, "/admin/external-dependents/acme", nil, nil); code != 200 {
			t.Errorf("DELETE acme: %d", code)
		}
		if code, _ := dev.Do(ctx, http.MethodPost, "/admin/services/ledger/v1/deprecate", map[string]string{"reason": "x"}, nil); code != http.StatusForbidden {
			t.Errorf("deprecate as dev: %d, want 403", code)
		}
		if code, _ := dev.Do(ctx, http.MethodPost, "/admin/instances/"+ledger.Opts.Instance+"/drain", map[string]any{}, nil); code != http.StatusForbidden {
			t.Errorf("drain as dev: %d, want 403", code)
		}
	})

	t.Run("3 admin token: writes, dev token rotation", func(t *testing.T) {
		if role, _ := root.Whoami(ctx); role != "admin" {
			t.Fatalf("whoami=%q", role)
		}
		if code, _ := root.Do(ctx, http.MethodPost, "/admin/services/ledger/v1/deprecate", map[string]string{"reason": "x"}, nil); code != 200 {
			t.Errorf("deprecate as admin: %d", code)
		}
		var out struct {
			DevToken string `json:"devToken"`
		}
		if code, err := root.Do(ctx, http.MethodPost, "/admin/dev-token/rotate", nil, &out); err != nil || code != 200 || out.DevToken == "" {
			t.Fatalf("rotate: %d %v", code, err)
		}
		if code, _ := dev.Do(ctx, http.MethodGet, "/admin/peers", nil, nil); code != http.StatusUnauthorized {
			t.Errorf("old dev token: %d, want 401", code)
		}
		if gw.DevToken() != out.DevToken {
			t.Error("rotated token not persisted to <data-dir>/dev-token")
		}
		if role, _ := admin.As(out.DevToken).Whoami(ctx); role != "dev" {
			t.Errorf("new dev token whoami=%q", role)
		}
	})
}
