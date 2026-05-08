package gateway

import (
	"fmt"
)

// warnSubscribeDelegateDeprecated emits a one-time-per-(ns,ver)
// deprecation log when a service registers under the legacy
// SubscriptionAuthorizer delegate namespace (`_events_auth`). The
// runtime delegate dispatch was removed in plan §2.3 — registrations
// under this namespace are now dead code from the gateway's
// perspective. The warning surfaces that fact at registration time
// so operators don't silently rely on a path that no longer fires.
//
// Returns true the first time it fires for a given tuple — false on
// subsequent re-registers (heartbeat-driven joins, replica adds).
// Tests use the bool; production callers ignore it.
//
// Routed through the embedded NATS warn channel when a cluster is
// configured (mirrors warnUnsupportedStreaming); fmt.Println
// otherwise.
//
// The generated `gw/proto/eventsauth/v1` package is intentionally
// parked here for one release after this delete — anything that
// imports it for its own purposes still builds, but the gateway
// ignores it.
func (g *Gateway) warnSubscribeDelegateDeprecated(ns, ver string) bool {
	if ns != authorizerNamespace {
		return false
	}
	if _, loaded := g.warnedEventsAuth.LoadOrStore(ns+":"+ver, struct{}{}); loaded {
		return false
	}
	msg := fmt.Sprintf("gateway: deprecation: service registered under %s/%s — the SubscriptionAuthorizer delegate has been removed (plan §2.3). Migrate to gateway.WithSignerSecret(...) and have the calling service do its own authz before invoking SignSubscriptionToken.", ns, ver)
	if g.cfg.cluster != nil {
		g.cfg.cluster.Server.Warnf("%s", msg)
	} else {
		fmt.Println(msg)
	}
	return true
}
