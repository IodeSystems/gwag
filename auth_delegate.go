package gateway

import (
	"context"
	"fmt"
	"time"

	cpv1 "github.com/iodesystems/go-api-gateway/controlplane/v1"
	eav1 "github.com/iodesystems/go-api-gateway/eventsauth/v1"
	"google.golang.org/grpc"
)

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
