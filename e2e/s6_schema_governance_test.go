//go:build e2e

package e2e

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// Two schemas for the same line. v2 drops the `instance` field of the
// reply: a client that selects it breaks.
const (
	govProtoV1 = `syntax = "proto3";
package PACKAGE;

// FixtureService is the e2e fixture's echo service.
service FixtureService {
  // Echo returns the message and the instance that served it.
  rpc Echo(EchoRequest) returns (EchoResponse);
}

message EchoRequest {
  string msg = 1;
}

message EchoResponse {
  string msg = 1;
  string instance = 2;
}
`
	govProtoBreaking = `syntax = "proto3";
package PACKAGE;

// FixtureService is the e2e fixture's echo service.
service FixtureService {
  // Echo returns the message.
  rpc Echo(EchoRequest) returns (EchoResponse);
}

message EchoRequest {
  string msg = 1;
}

message EchoResponse {
  string msg = 1;
}
`
)

// TestS6SchemaGovernance covers what the gateway lets a team change:
// a locked vN line refuses a different schema and keeps serving the
// old one, the mutable unstable line swaps, a line can be deprecated
// with a reason and un-deprecated, and the schema export plus
// `gwag ctl schema diff` show the change.
func TestS6SchemaGovernance(t *testing.T) {
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{Environment: "e2e"})
	admin := gw.Admin()
	ctx := harness.Ctx(t, 3*time.Minute)
	const wait = 20 * time.Second

	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	v1Proto := write("v1.proto", govProtoV1)
	breakingProto := write("breaking.proto", govProtoBreaking)

	registrations := func(kind, outcome string) harness.Labels {
		return harness.Labels{"kind": kind, "outcome": outcome}
	}
	selects := func(ns, line string) string {
		return `{ ` + ns + ` { ` + line + ` { echo(msg: "x") { msg instance } } } }`
	}

	env.Fixture(gw, harness.FixtureOpts{Namespace: "orders", Instance: "orders-1", Owner: "commerce", Proto: map[string]string{"v1": v1Proto}})
	billing := env.Fixture(gw, harness.FixtureOpts{Namespace: "billing", Instance: "billing-1", Owner: "finance", Versions: []string{"unstable"}, Proto: map[string]string{"unstable": v1Proto}})

	// The reply of orders v1 and billing unstable, selecting `instance`.
	call := func(ns, line string) (string, error) {
		r, err := gw.GraphQL(ctx, selects(ns, line), nil)
		if err != nil {
			return "", err
		}
		if err := r.Err(); err != nil {
			return "", err
		}
		inst, _ := r.Path(ns + "." + line + ".echo.instance").(string)
		return inst, nil
	}

	t.Run("baseline: both lines serve the full reply", func(t *testing.T) {
		if inst, err := call("orders", "v1"); err != nil || inst != "orders-1" {
			t.Fatalf("orders v1: instance %q, %v", inst, err)
		}
		if inst, err := call("billing", "unstable"); err != nil || inst != "billing-1" {
			t.Fatalf("billing unstable: instance %q, %v", inst, err)
		}
	})

	var ordersSDL string
	t.Run("a breaking change on locked v1 is refused, old schema keeps serving", func(t *testing.T) {
		var err error
		if ordersSDL, err = gw.SchemaSDL(ctx, "orders"); err != nil {
			t.Fatal(err)
		}
		before := gw.MustScrape(t)
		rebuilds := before.Counter("schema_rebuilds_total", nil)

		bad := env.Fixture(gw, harness.FixtureOpts{Namespace: "orders", Instance: "orders-2", Proto: map[string]string{"v1": breakingProto}, NoWait: true})
		bad.Proc.WaitLog("vN is locked once registered", wait)
		if code := bad.Proc.WaitExit(wait); code == 0 {
			t.Errorf("orders-2 exit code 0, want a failure")
		}
		gw.WaitDeltas(t, before, "registrations_total", []harness.Want{
			{Labels: registrations("proto", "rejected"), Delta: 1},
			{Labels: registrations("proto", "swapped"), Delta: 0},
			{Labels: registrations("proto", "created"), Delta: 0},
			{Labels: registrations("proto", "joined"), Delta: 0},
		}, wait)

		// The admin list records the refusal and has no second replica.
		ss, err := admin.Services(ctx)
		if err != nil {
			t.Fatal(err)
		}
		svc := ss.Service("orders", "v1")
		if svc == nil || svc.ReplicaCount != 1 || len(svc.Instances) != 1 {
			t.Fatalf("orders/v1 after the refusal: %+v", svc)
		}
		if svc.RejectedJoins == nil || svc.RejectedJoins.Count != 1 || !strings.Contains(svc.RejectedJoins.LastReason, "locked") {
			t.Errorf("orders/v1 rejectedJoins: %+v, want count 1 naming the lock", svc.RejectedJoins)
		}

		// Same schema, same SDL, same reply; no rebuild happened.
		now, err := gw.SchemaSDL(ctx, "orders")
		if err != nil {
			t.Fatal(err)
		}
		if now != ordersSDL {
			t.Errorf("orders SDL changed after a refused registration")
		}
		if inst, err := call("orders", "v1"); err != nil || inst != "orders-1" {
			t.Fatalf("orders v1 after the refusal: instance %q, %v", inst, err)
		}
		if got := gw.MustScrape(t).Counter("schema_rebuilds_total", nil); got != rebuilds {
			t.Errorf("schema_rebuilds_total moved %v -> %v on a refused registration", rebuilds, got)
		}
	})

	t.Run("an identical schema joins the locked line as a second replica", func(t *testing.T) {
		before := gw.MustScrape(t)
		env.Fixture(gw, harness.FixtureOpts{Namespace: "orders", Instance: "orders-3", Proto: map[string]string{"v1": v1Proto}})
		gw.WaitDeltas(t, before, "registrations_total", []harness.Want{
			{Labels: registrations("proto", "joined"), Delta: 1},
			{Labels: registrations("proto", "rejected"), Delta: 0},
		}, wait)
		ss, err := admin.Services(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if svc := ss.Service("orders", "v1"); svc.ReplicaCount != 2 {
			t.Errorf("orders/v1 replicaCount %d, want 2", svc.ReplicaCount)
		}
	})

	t.Run("the same change on unstable swaps the schema", func(t *testing.T) {
		oldSDL, err := gw.SchemaSDL(ctx, "billing")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(oldSDL, "instance") {
			t.Fatalf("billing SDL before the swap has no instance field:\n%s", oldSDL)
		}
		oldFile := write("billing-before.graphql", oldSDL)
		before := gw.MustScrape(t)

		env.Fixture(gw, harness.FixtureOpts{Namespace: "billing", Instance: "billing-2", Versions: []string{"unstable"}, Proto: map[string]string{"unstable": breakingProto}})
		gw.WaitDeltas(t, before, "registrations_total", []harness.Want{
			{Labels: registrations("proto", "swapped"), Delta: 1},
			{Labels: registrations("proto", "rejected"), Delta: 0},
		}, wait)
		gw.WaitCounterAtLeast(t, before, "schema_rebuilds_total", harness.Labels{"result": "ok"}, 1, wait)

		harness.Eventually(t, wait, "billing SDL lost the instance field", func() error {
			sdl, err := gw.SchemaSDL(ctx, "billing")
			if err != nil {
				return err
			}
			if sdl == oldSDL || strings.Contains(sdl, "instance") {
				return errors.New("SDL unchanged")
			}
			return nil
		})
		// The old query breaks; the new one runs against the new instance.
		if _, err := call("billing", "unstable"); err == nil {
			t.Error("query selecting `instance` still validates after the swap")
		}
		r := gw.MustGraphQL(t, `{ billing { unstable { echo(msg: "x") { msg } } } }`, nil)
		if r.Path("billing.unstable.echo.msg") != "x" {
			t.Errorf("billing unstable after the swap: %s", r.Data)
		}
		ss, err := admin.Services(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if svc := ss.Service("billing", "unstable"); svc == nil || svc.ReplicaCount != 1 || svc.Instance("billing-2") == nil {
			t.Errorf("billing/unstable after the swap: %+v", svc)
		}

		// The previous trunk build goes away; the new one keeps serving.
		if code := billing.Proc.Stop(30 * time.Second); code != 0 {
			t.Errorf("billing-1 exit code %d, want 0", code)
		}
		admin.WaitInstance("billing", "unstable", "billing-1", "", wait)
		r = gw.MustGraphQL(t, `{ billing { unstable { echo(msg: "y") { msg } } } }`, nil)
		if r.Path("billing.unstable.echo.msg") != "y" {
			t.Errorf("billing unstable after the old build left: %s", r.Data)
		}

		t.Run("schema diff shows it", func(t *testing.T) {
			// The CI gate (`gwag ctl schema diff --strict`) sees the removal as breaking.
			out, code := schemaDiff(t, oldFile, gw.SchemaURL("billing"))
			t.Logf("gwag ctl schema diff --strict (unstable, before -> after):\n%s", out)
			if code == 0 || !strings.Contains(out, "[BREAKING]") || !strings.Contains(out, "instance") {
				t.Errorf("exit %d; want a failure naming the removed instance field", code)
			}
			// The locked line shows no difference to the file taken before
			// the refused registration.
			out, code = schemaDiff(t, write("orders-before.graphql", ordersSDL), gw.SchemaURL("orders"))
			if code != 0 || !strings.Contains(out, "(no differences)") {
				t.Errorf("orders v1 diff: exit %d: %s", code, out)
			}
		})
	})

	t.Run("deprecate v1 with a reason, then undeprecate", func(t *testing.T) {
		const reason = "moving to v2 in Q3; migrate before then"
		before := ordersSDL
		if strings.Contains(before, "@deprecated") {
			t.Fatalf("orders SDL is deprecated before anything was deprecated:\n%s", before)
		}
		if err := admin.Deprecate(ctx, "orders", "v1", reason); err != nil {
			t.Fatal(err)
		}
		ss, err := admin.Services(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := ss.Service("orders", "v1").ManualDeprecationReason; got != reason {
			t.Errorf("admin manualDeprecationReason %q, want %q", got, reason)
		}
		harness.Eventually(t, wait, "SDL carries @deprecated with the reason", func() error {
			sdl, err := gw.SchemaSDL(ctx, "orders")
			if err != nil {
				return err
			}
			if !strings.Contains(sdl, "@deprecated(reason: \""+reason+"\")") {
				return errors.New("no @deprecated in the SDL:\n" + sdl)
			}
			return nil
		})
		// Deprecated is advice: the line still serves.
		if inst, err := call("orders", "v1"); err != nil || inst == "" {
			t.Errorf("orders v1 while deprecated: %q, %v", inst, err)
		}
		// Admin graph/GraphQL read the same state.
		r := gw.MustGraphQL(t, `{ admin { listServices { services { namespace version manualDeprecationReason } } } }`, nil, harness.Bearer(gw.Token()))
		if !strings.Contains(string(r.Data), reason) {
			t.Errorf("admin.listServices does not show the reason: %s", r.Data)
		}

		if err := admin.Undeprecate(ctx, "orders", "v1"); err != nil {
			t.Fatal(err)
		}
		ss, err = admin.Services(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := ss.Service("orders", "v1").ManualDeprecationReason; got != "" {
			t.Errorf("admin manualDeprecationReason %q after undeprecate, want empty", got)
		}
		harness.Eventually(t, wait, "SDL back to the original", func() error {
			sdl, err := gw.SchemaSDL(ctx, "orders")
			if err != nil {
				return err
			}
			if strings.Contains(sdl, "@deprecated") {
				return errors.New("still deprecated")
			}
			return nil
		})
	})

	t.Run("an empty reason is a 422 on body.reason", func(t *testing.T) {
		err := admin.Deprecate(ctx, "orders", "v1", "")
		var he *harness.HTTPError
		if !errors.As(err, &he) {
			t.Fatalf("empty reason: %v, want an HTTP error", err)
		}
		if he.Status != 422 || !strings.Contains(he.Body, `"location":"body.reason"`) {
			t.Errorf("empty reason: HTTP %d %s, want 422 with body.reason", he.Status, he.Body)
		}
		ss, _ := admin.Services(ctx)
		if got := ss.Service("orders", "v1").ManualDeprecationReason; got != "" {
			t.Errorf("a refused deprecate left reason %q", got)
		}
		// An unknown line is refused too.
		if err := admin.Deprecate(ctx, "orders", "v9", "nope"); err == nil {
			t.Error("deprecating a line that does not exist succeeded")
		}
	})

}

// schemaDiff runs `gwag ctl schema diff --strict` and returns its output
// and exit code.
func schemaDiff(t *testing.T, from, to string) (string, int) {
	t.Helper()
	cmd := exec.Command(harness.Binary(t, "gwag"), "ctl", "schema", "diff", "--from", from, "--to", to, "--strict")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	default:
		t.Fatalf("gwag ctl schema diff: %v", err)
		return "", -1
	}
}
