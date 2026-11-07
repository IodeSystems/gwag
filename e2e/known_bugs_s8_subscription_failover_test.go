//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// TestKnownBugPubSubStreamGauges: docs/operations.md defines
// streams_inflight_total as "Active subscription streams, gateway-wide"
// and pool_streams_inflight as "Active subscription streams in a pool",
// and Drain waits for streams_inflight_total to reach 0. A ps_sub
// subscription is a subscription stream, but the broker path (gw/pubsub.go,
// "Stream-cap accounting ... does not apply here") never touches the
// gauges, the stream caps, or /api/health's active_streams. A node
// holding only ps_sub subscriptions reports zero streams.
func TestKnownBugPubSubStreamGauges(t *testing.T) {
	knownBug(t, "ps_sub subscriptions are not counted in streams_inflight_total / active_streams")
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{NATS: true, Args: []string{"--subscribe-secret", s8Secret}})
	ctx := harness.Ctx(t, 2*time.Minute)

	const channel = "events.gauges.one"
	tok, err := gw.Sign(ctx, channel, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := gw.Subscribe(t, fmt.Sprintf(`subscription { ps_sub(channel: %q, %s) { payload } }`, channel, tok.Args()))
	if err != nil {
		t.Fatal(err)
	}
	harness.Eventually(t, 10*time.Second, "streams_inflight_total is 1 and health counts the stream", func() error {
		s, err := gw.Scrape(ctx)
		if err != nil {
			return err
		}
		if v := s.Counter("streams_inflight_total", nil); v != 1 {
			return fmt.Errorf("streams_inflight_total = %v with one ps_sub open, want 1", v)
		}
		body, status, err := gw.Health(ctx)
		if err != nil || status != http.StatusOK {
			return fmt.Errorf("health %d %v", status, err)
		}
		if n, _ := body["active_streams"].(float64); n != 1 {
			return fmt.Errorf("health active_streams = %v, want 1", body["active_streams"])
		}
		return nil
	})
	sub.Close()
	harness.Eventually(t, 10*time.Second, "streams_inflight_total returns to 0", func() error {
		s, err := gw.Scrape(ctx)
		if err != nil {
			return err
		}
		if v := s.Counter("streams_inflight_total", nil); v != 0 {
			return fmt.Errorf("streams_inflight_total = %v", v)
		}
		return nil
	})
}
