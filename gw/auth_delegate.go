package gateway

import (
	"context"
	"fmt"
	"time"

	cpv1 "github.com/iodesystems/go-api-gateway/gw/proto/controlplane/v1"
	eav1 "github.com/iodesystems/go-api-gateway/gw/proto/eventsauth/v1"
	"google.golang.org/grpc"
)

// warnSubscribeDelegateDeprecated emits a one-time-per-(ns,ver)
// deprecation log when a service registers under the
// SubscriptionAuthorizer delegate namespace (`_events_auth`). The
// wire path stays functional for one release; plan §2 removes it
// outright after. Returns true the first time it fires for a given
// tuple — false on subsequent re-registers (heartbeat-driven joins,
// replica adds). Tests use the bool; production callers ignore it.
//
// Routed through the embedded NATS warn channel when a cluster is
// configured (mirrors warnUnsupportedStreaming); fmt.Println
// otherwise.
func (g *Gateway) warnSubscribeDelegateDeprecated(ns, ver string) bool {
	if ns != authorizerNamespace {
		return false
	}
	if _, loaded := g.warnedEventsAuth.LoadOrStore(ns+":"+ver, struct{}{}); loaded {
		return false
	}
	msg := fmt.Sprintf("gateway: deprecation: service registered under %s/%s — the SubscriptionAuthorizer delegate is going away. Migrate to gateway.WithSignerSecret(...) and have the calling service do its own authz before invoking SignSubscriptionToken (plan §2).", ns, ver)
	if g.cfg.cluster != nil {
		g.cfg.cluster.Server.Warnf("%s", msg)
	} else {
		fmt.Println(msg)
	}
	return true
}

// consultSubscribeDelegate calls AuthorizeSign on the registered
// SubscriptionAuthorizer if one is present. Returns:
//   - (UNSPECIFIED, "", nil) when no delegate is registered (caller
//     interprets as "no delegate; proceed with signing").
//   - (code, reason, nil) when the delegate replied.
//   - (UNSPECIFIED, "", err) when the dispatch itself failed
//     (network, no live replicas, etc.) — caller maps to UNAVAILABLE.
func (g *Gateway) consultSubscribeDelegate(
	ctx context.Context,
	channel string,
	timestampUnix, ttlSeconds int64,
) (cpv1.SubscribeAuthCode, string, error) {
	pool, ok := g.lookupPool(authorizerNamespace, "v1")
	if !ok {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_UNSPECIFIED, "", nil
	}
	r := pool.pickReplica()
	if r == nil {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_UNSPECIFIED, "", fmt.Errorf("no live %s/v1 replicas", authorizerNamespace)
	}

	conn, ok := r.conn.(grpc.ClientConnInterface)
	if !ok {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_UNSPECIFIED, "", fmt.Errorf("delegate replica conn not usable")
	}
	client := eav1.NewSubscriptionAuthorizerClient(conn)

	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := client.AuthorizeSign(dctx, &eav1.AuthorizeSignRequest{
		Channel:       channel,
		TimestampUnix: timestampUnix,
		TtlSeconds:    ttlSeconds,
	})
	if err != nil {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_UNSPECIFIED, "", err
	}
	return cpv1.SubscribeAuthCode(resp.GetCode()), resp.GetReason(), nil
}
