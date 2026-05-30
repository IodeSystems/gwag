package gat

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// TestGatewaySignSubscribeToken covers the *Gateway method form of the
// signer: it errors before EnableSubscribeAuth, and after it mints a
// token the gateway's own verifier accepts — equivalent to the package
// function under the configured secret.
func TestGatewaySignSubscribeToken(t *testing.T) {
	secret := []byte("sub-secret")
	g, _ := New()
	defer g.Close()

	if _, _, err := g.SignSubscribeToken("c"); err == nil {
		t.Fatal("expected error when subscribe auth not enabled")
	}

	g.EnableSubscribeAuth(secret)
	tok, ts, err := g.SignSubscribeToken("room.>")
	if err != nil {
		t.Fatalf("SignSubscribeToken: %v", err)
	}

	v := url.Values{}
	v.Set("channel", "room.>")
	v.Set("token", tok)
	v.Set("ts", strconv.FormatInt(ts, 10))
	req := httptest.NewRequest("GET", "/?"+v.Encode(), nil)
	if err := g.verifySubscribeRequest(req); err != nil {
		t.Errorf("verifySubscribeRequest rejected method-signed token: %v", err)
	}

	if want := computeSubscribeToken(secret, "room.>", ts); want != tok {
		t.Errorf("method token %q != package token %q", tok, want)
	}
}
