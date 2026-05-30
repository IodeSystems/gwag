package gateway

import (
	"context"
	"fmt"
	"net/http"
)

// TokenSource supplies a credential for outbound service-account auth.
// It is called once per outbound request, so an implementation can cache
// a short-lived token and only mint a new one as expiry approaches. The
// returned value is the bare credential — no "Bearer " prefix.
//
// Stability: experimental
type TokenSource func(ctx context.Context) (string, error)

// StaticToken returns a TokenSource that always yields token — for a
// long-lived service-account key. For refreshing credentials, supply
// your own TokenSource closure.
//
// Stability: experimental
func StaticToken(token string) TokenSource {
	return func(context.Context) (string, error) { return token, nil }
}

// ServiceAccountTransport is an http.RoundTripper that attaches a
// service-account credential to every outbound request: the gateway
// calls the upstream as *itself*, distinct from forwarding the caller's
// own token (ForwardHeaders). Install it via WithOpenAPIClient (gateway-
// wide) or OpenAPIClient (per source):
//
//	src := gateway.StaticToken(os.Getenv("SA_TOKEN"))
//	gw.AddOpenAPI(spec, gateway.To(url),
//		gateway.OpenAPIClient(&http.Client{Transport: gateway.ServiceAccountTransport{Token: src}}))
//
// Header defaults to "Authorization"; when Header is "Authorization" and
// Scheme is unset, Scheme defaults to "Bearer". For a custom header
// (e.g. "X-Api-Key") the raw token is written unless Scheme is set. Base
// defaults to http.DefaultTransport.
//
// Stability: experimental
type ServiceAccountTransport struct {
	Token  TokenSource
	Header string
	Scheme string
	Base   http.RoundTripper
}

// RoundTrip implements http.RoundTripper. It clones the request before
// adding the credential header (per the RoundTripper contract — the
// inbound request must not be mutated) and fails the request if the
// TokenSource errors.
func (t ServiceAccountTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.Token == nil {
		return nil, fmt.Errorf("gateway: ServiceAccountTransport: nil TokenSource")
	}
	tok, err := t.Token(req.Context())
	if err != nil {
		return nil, fmt.Errorf("gateway: service-account token: %w", err)
	}
	header, scheme := t.Header, t.Scheme
	if header == "" {
		header = "Authorization"
		if scheme == "" {
			scheme = "Bearer"
		}
	}
	value := tok
	if scheme != "" {
		value = scheme + " " + tok
	}
	clone := req.Clone(req.Context())
	clone.Header.Set(header, value)
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// ServiceAccountClient is sugar for an *http.Client whose Transport is a
// ServiceAccountTransport over src (Authorization: Bearer <token>,
// http.DefaultTransport). Pass it to WithOpenAPIClient / OpenAPIClient.
//
// Stability: experimental
func ServiceAccountClient(src TokenSource) *http.Client {
	return &http.Client{Transport: ServiceAccountTransport{Token: src}}
}
