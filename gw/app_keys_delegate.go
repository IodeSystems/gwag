package gateway

import (
	"context"
	"time"

	"google.golang.org/grpc"

	akv1 "github.com/iodesystems/gwag/v2/gw/proto/appkeyauth/v1"
)

// appKeyAuthorizerNamespace is the reserved registration namespace
// for the AppKeyAuthorizer delegate (proto appkeyauth/v1). Mirrors
// the AdminAuthorizer delegate at "_admin_auth/v1".
const appKeyAuthorizerNamespace = "_app_key_auth"

type appKeyDelegateOutcome int

const (
	// No usable answer: check the gateway's own keys. No delegate,
	// UNSPECIFIED / UNAVAILABLE / NOT_CONFIGURED, or a transport error.
	appKeyDelegateFallthrough appKeyDelegateOutcome = iota
	appKeyDelegateAccept
	appKeyDelegateReject
)

// consultAppKeyDelegate asks the registered AppKeyAuthorizer about
// tok. With accept, grant holds the namespaces it named.
func (g *Gateway) consultAppKeyDelegate(ctx context.Context, tok, op string, namespaces []string, remoteAddr string) (appKeyDelegateOutcome, appKeyGrant) {
	pool, ok := g.lookupPool(appKeyAuthorizerNamespace, "v1")
	if !ok {
		return appKeyDelegateFallthrough, appKeyGrant{}
	}
	rep := pool.pickReplica()
	if rep == nil {
		return appKeyDelegateFallthrough, appKeyGrant{}
	}
	conn, ok := rep.conn.(grpc.ClientConnInterface)
	if !ok {
		return appKeyDelegateFallthrough, appKeyGrant{}
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := akv1.NewAppKeyAuthorizerClient(conn).Authorize(dctx, &akv1.AuthorizeRequest{
		Token:      tok,
		Operation:  op,
		Namespaces: namespaces,
		RemoteAddr: remoteAddr,
	})
	if err != nil {
		return appKeyDelegateFallthrough, appKeyGrant{}
	}
	switch resp.GetCode() {
	case akv1.AppKeyAuthCode_APP_KEY_AUTH_CODE_OK:
		grant := appKeyGrant{id: sanitizeSubject(resp.GetKeyId()), namespaces: resp.GetNamespaces(), delegate: true}
		if ms := resp.GetExpiresUnixMs(); ms > 0 {
			grant.expires = time.UnixMilli(ms)
		}
		return appKeyDelegateAccept, grant
	case akv1.AppKeyAuthCode_APP_KEY_AUTH_CODE_DENIED:
		return appKeyDelegateReject, appKeyGrant{}
	}
	return appKeyDelegateFallthrough, appKeyGrant{}
}
