//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// TestS15GatedDeploy is the integration pattern's deploy gate
// (docs/integrate.md): two replicas register with a health URL and a
// spec URL; the gateway verifies each and `gwag service wait --green`
// passes. One replica's health check fails: the gateway reports it
// red with the reason, stops routing to it while the other keeps
// serving, and `service wait` for it exits 3. It recovers to green.
func TestS15GatedDeploy(t *testing.T) {
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{Args: []string{"--verify-interval", "300ms", "--verify-retry", "100ms"}})
	a := env.Fixture(gw, harness.FixtureOpts{Namespace: "billing", Instance: "billing-a", Verify: true})
	b := env.Fixture(gw, harness.FixtureOpts{Namespace: "billing", Instance: "billing-b", Verify: true})
	admin := gw.Admin()

	verification := func(id string) (string, []string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ss, err := admin.Services(ctx)
		if err != nil {
			return "", nil
		}
		if svc := ss.Service("billing", "v1"); svc != nil {
			if in := svc.Instance(id); in != nil {
				return in.Verification + "/" + in.State, in.VerifyReasons
			}
		}
		return "", nil
	}
	wait := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(harness.Binary(t, "gwag"), append([]string{"service", "wait", "--gateway", gw.CPAddr(), "--interval", "100ms"}, args...)...)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return string(out), 0
		case errors.As(err, &ee):
			return string(out), ee.ExitCode()
		}
		t.Fatalf("gwag service wait: %v", err)
		return "", -1
	}

	t.Run("both green", func(t *testing.T) {
		for _, id := range []string{"billing-a", "billing-b"} {
			if v, r := verification(id); v != "green/ready" {
				t.Fatalf("%s = %s %v, want green/ready", id, v, r)
			}
		}
		if out, code := wait("--service", "billing:v1", "--green", "--timeout", "10s"); code != 0 {
			t.Fatalf("service wait: exit %d\n%s", code, out)
		}
	})

	t.Run("a red: not routed, b serves, wait exits 3", func(t *testing.T) {
		a.SetFaults(harness.Faults{Unhealthy: true})
		harness.Eventually(t, 10*time.Second, "billing-a red", func() error {
			if v, r := verification("billing-a"); v != "red/waiting" {
				return fmt.Errorf("billing-a = %s %v", v, r)
			}
			return nil
		})
		if _, r := verification("billing-a"); len(r) != 1 || !strings.Contains(r[0], "status 500") {
			t.Fatalf("reasons = %v, want the health status", r)
		}
		before := a.Stats().Calls
		for i := 0; i < 20; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := b.Echo(ctx, "v1", "x")
			cancel()
			if err != nil {
				t.Fatalf("call %d with one red replica: %v", i, err)
			}
		}
		if got := a.Stats().Calls - before; got != 0 {
			t.Fatalf("red replica got %d calls", got)
		}
		out, code := wait("--service", "billing:v1", "--instance", "billing-a", "--green", "--timeout", "1s")
		if code != 3 || !strings.Contains(out, "status 500") {
			t.Fatalf("service wait for a red instance: exit %d, want 3 with the reason\n%s", code, out)
		}
		s := gw.MustScrape(t)
		if got := s.Counter("service_verification", harness.Labels{"namespace": "billing", "version": "v1", "state": "red"}); got != 1 {
			t.Fatalf("service_verification{red} = %v, want 1\n%s", got, s.Describe("service_verification", nil))
		}
	})

	t.Run("a recovers", func(t *testing.T) {
		a.SetFaults(harness.Faults{})
		if out, code := wait("--service", "billing:v1", "--instance", "billing-a", "--green", "--timeout", "10s"); code != 0 {
			t.Fatalf("service wait after recovery: exit %d\n%s", code, out)
		}
	})
}
