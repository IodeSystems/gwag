package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
)

// HMAC caller-id mode tests. Mirrors the auth_subscribe_test.go layout
// for the parallel sub-auth primitive — same verification scheme,
// scoped to caller identity instead of subscription channel.

func TestCallerIDHMAC_LegacyTokenVerifies(t *testing.T) {
	secret := []byte("legacy-key")
	sig, ts := SignCallerIDToken(secret, "billing", 60)

	r := callerHMACRequest("billing", "", sig, ts)
	ctx := WithHTTPRequest(context.Background(), r)

	o := CallerIDHMACOptions{Secret: secret}
	got, err := o.resolve(ctx, time.Unix(ts, 0))
	if err != nil {
		t.Fatalf("legacy token: %v", err)
	}
	if got != "billing" {
		t.Errorf("got %q, want billing", got)
	}
}

func TestCallerIDHMAC_RotatedKidVerifies(t *testing.T) {
	secret := []byte("rotated-key")
	kid := "v2"
	sig, _, ts := SignCallerIDTokenWithKid(secret, kid, "users", 60)

	r := callerHMACRequest("users", kid, sig, ts)
	ctx := WithHTTPRequest(context.Background(), r)

	o := CallerIDHMACOptions{Secrets: map[string][]byte{kid: secret}}
	got, err := o.resolve(ctx, time.Unix(ts, 0))
	if err != nil {
		t.Fatalf("rotated kid: %v", err)
	}
	if got != "users" {
		t.Errorf("got %q, want users", got)
	}
}

func TestCallerIDHMAC_TokenBoundToKid(t *testing.T) {
	// A token signed for kid="v1" must not verify when sent under
	// kid="v2" even if both secrets are identical — payload includes
	// the kid so swapping kid changes the MAC.
	secret := []byte("shared")
	sig, _, ts := SignCallerIDTokenWithKid(secret, "v1", "alice", 60)

	r := callerHMACRequest("alice", "v2", sig, ts) // wrong kid
	ctx := WithHTTPRequest(context.Background(), r)

	o := CallerIDHMACOptions{Secrets: map[string][]byte{"v1": secret, "v2": secret}}
	_, err := o.resolve(ctx, time.Unix(ts, 0))
	if !errors.Is(err, errCallerIDHMACSignatureMismatch) {
		t.Errorf("cross-kid replay should fail, got %v", err)
	}
}

func TestCallerIDHMAC_UnknownKidErrors(t *testing.T) {
	secret := []byte("k")
	sig, _, ts := SignCallerIDTokenWithKid(secret, "v1", "alice", 60)

	r := callerHMACRequest("alice", "v1", sig, ts)
	ctx := WithHTTPRequest(context.Background(), r)

	o := CallerIDHMACOptions{Secrets: map[string][]byte{"v9": []byte("other")}}
	_, err := o.resolve(ctx, time.Unix(ts, 0))
	if !errors.Is(err, errCallerIDHMACUnknownKid) {
		t.Errorf("unknown kid: got %v, want errCallerIDHMACUnknownKid", err)
	}
}

func TestCallerIDHMAC_TooOld(t *testing.T) {
	secret := []byte("k")
	sig, ts := SignCallerIDToken(secret, "alice", 60)

	r := callerHMACRequest("alice", "", sig, ts)
	ctx := WithHTTPRequest(context.Background(), r)

	o := CallerIDHMACOptions{Secret: secret, SkewWindow: 30 * time.Second}
	// Advance now by 5 minutes — outside the 30s skew window.
	_, err := o.resolve(ctx, time.Unix(ts+300, 0))
	if !errors.Is(err, errCallerIDHMACTimestampTooOld) {
		t.Errorf("too old: got %v, want errCallerIDHMACTimestampTooOld", err)
	}
}

func TestCallerIDHMAC_TooNew(t *testing.T) {
	secret := []byte("k")
	sig, ts := SignCallerIDToken(secret, "alice", 60)

	r := callerHMACRequest("alice", "", sig, ts)
	ctx := WithHTTPRequest(context.Background(), r)

	o := CallerIDHMACOptions{Secret: secret, SkewWindow: 30 * time.Second}
	// Rewind now by 5 minutes — token's ts is in the future from
	// the gateway's POV.
	_, err := o.resolve(ctx, time.Unix(ts-300, 0))
	if !errors.Is(err, errCallerIDHMACTimestampTooNew) {
		t.Errorf("too new: got %v, want errCallerIDHMACTimestampTooNew", err)
	}
}

func TestCallerIDHMAC_BadSignature(t *testing.T) {
	secret := []byte("k")
	_, ts := SignCallerIDToken(secret, "alice", 60)
	forged := base64.StdEncoding.EncodeToString([]byte("forged-sig-bytes"))

	r := callerHMACRequest("alice", "", forged, ts)
	ctx := WithHTTPRequest(context.Background(), r)

	o := CallerIDHMACOptions{Secret: secret}
	_, err := o.resolve(ctx, time.Unix(ts, 0))
	if !errors.Is(err, errCallerIDHMACSignatureMismatch) {
		t.Errorf("forged sig: got %v, want errCallerIDHMACSignatureMismatch", err)
	}
}

func TestCallerIDHMAC_MalformedMissingCallerID(t *testing.T) {
	o := CallerIDHMACOptions{Secret: []byte("k")}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(HMACCallerIDTimestampHeader, "1")
	r.Header.Set(HMACCallerIDSignatureHeader, "anysig")
	ctx := WithHTTPRequest(context.Background(), r)
	if _, err := o.resolve(ctx, time.Now()); !errors.Is(err, errCallerIDHMACMalformed) {
		t.Errorf("missing caller-id: got %v, want errCallerIDHMACMalformed", err)
	}
}

func TestCallerIDHMAC_MalformedMissingTimestamp(t *testing.T) {
	o := CallerIDHMACOptions{Secret: []byte("k")}
	r, _ := newRequestWithHeader(PublicCallerIDHeader, "alice")
	r.Header.Set(HMACCallerIDSignatureHeader, "anysig")
	ctx := WithHTTPRequest(context.Background(), r)
	if _, err := o.resolve(ctx, time.Now()); !errors.Is(err, errCallerIDHMACMalformed) {
		t.Errorf("missing ts: got %v, want errCallerIDHMACMalformed", err)
	}
}

func TestCallerIDHMAC_MalformedNonNumericTimestamp(t *testing.T) {
	o := CallerIDHMACOptions{Secret: []byte("k")}
	r, _ := newRequestWithHeader(PublicCallerIDHeader, "alice")
	r.Header.Set(HMACCallerIDTimestampHeader, "not-a-number")
	r.Header.Set(HMACCallerIDSignatureHeader, "anysig")
	ctx := WithHTTPRequest(context.Background(), r)
	if _, err := o.resolve(ctx, time.Now()); !errors.Is(err, errCallerIDHMACMalformed) {
		t.Errorf("non-numeric ts: got %v, want errCallerIDHMACMalformed", err)
	}
}

func TestCallerIDHMAC_AnonymousNoHeaders(t *testing.T) {
	// No signature → treat as anonymous (not an error). resolveCallerID
	// surfaces "unknown" on the seam; enforce-mode is the layer that
	// turns this into a 401 later.
	r := httptest.NewRequest("GET", "/", nil)
	ctx := WithHTTPRequest(context.Background(), r)

	o := CallerIDHMACOptions{Secret: []byte("k")}
	got, err := o.resolve(ctx, time.Now())
	if err != nil {
		t.Fatalf("anonymous: %v", err)
	}
	if got != "" {
		t.Errorf("anonymous: got %q, want empty", got)
	}
}

func TestCallerIDHMAC_NotConfiguredError(t *testing.T) {
	// Caller signed something, but the operator forgot to configure a
	// secret. Surface as a real error so the misconfiguration is
	// visible (vs the silent "anonymous" path when the caller didn't
	// sign at all).
	r := callerHMACRequest("alice", "", "sig", 1)
	ctx := WithHTTPRequest(context.Background(), r)

	o := CallerIDHMACOptions{}
	_, err := o.resolve(ctx, time.Now())
	if !errors.Is(err, errCallerIDHMACNotConfigured) {
		t.Errorf("not-configured: got %v, want errCallerIDHMACNotConfigured", err)
	}
}

func TestCallerIDHMAC_GRPCMetadataPath(t *testing.T) {
	secret := []byte("grpc-key")
	sig, ts := SignCallerIDToken(secret, "payments", 60)

	md := metadata.Pairs(
		PublicCallerIDMetadata, "payments",
		HMACCallerIDTimestampMetadata, strconv.FormatInt(ts, 10),
		HMACCallerIDSignatureMetadata, sig,
	)
	ctx := metadata.NewIncomingContext(context.Background(), md)

	o := CallerIDHMACOptions{Secret: secret}
	got, err := o.resolve(ctx, time.Unix(ts, 0))
	if err != nil {
		t.Fatalf("grpc verify: %v", err)
	}
	if got != "payments" {
		t.Errorf("got %q, want payments", got)
	}
}

func TestCallerIDHMAC_HTTPWinsOverGRPC(t *testing.T) {
	// Mixed context — HTTP signature drives the verify path (mirrors
	// the Public extractor's resolution order, observable contract).
	secret := []byte("k")
	sigHTTP, ts := SignCallerIDToken(secret, "http-alice", 60)

	r := callerHMACRequest("http-alice", "", sigHTTP, ts)
	ctx := WithHTTPRequest(context.Background(), r)

	sigGRPC, _ := SignCallerIDToken(secret, "grpc-bob", 60)
	md := metadata.Pairs(
		PublicCallerIDMetadata, "grpc-bob",
		HMACCallerIDTimestampMetadata, strconv.FormatInt(ts, 10),
		HMACCallerIDSignatureMetadata, sigGRPC,
	)
	ctx = metadata.NewIncomingContext(ctx, md)

	o := CallerIDHMACOptions{Secret: secret}
	got, err := o.resolve(ctx, time.Unix(ts, 0))
	if err != nil {
		t.Fatalf("mixed: %v", err)
	}
	if got != "http-alice" {
		t.Errorf("got %q, want http-alice", got)
	}
}

// End-to-end through the option seam: WithCallerIDHMAC plumbs into the
// dispatch recording site and a valid token shows up on Snapshot rows.
// The verify path uses real time.Now(), so the option's SkewWindow is
// sized to comfortably cover wall-clock skew between the signer call
// and the resolve call.
func TestSnapshot_CallerDimension_HMACExtractor(t *testing.T) {
	old := nowFunc
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	secret := []byte("seam-key")
	g := New(
		WithoutMetrics(),
		WithoutBackpressure(),
		WithAdminToken([]byte("t")),
		WithCallerIDHMAC(CallerIDHMACOptions{Secret: secret, SkewWindow: time.Hour}),
	)
	t.Cleanup(g.Close)

	sig, ts := SignCallerIDToken(secret, "billing", 60)
	r := callerHMACRequest("billing", "", sig, ts)
	ctx := WithHTTPRequest(context.Background(), r)
	g.cfg.metrics.RecordDispatch(ctx, "greeter", "v1", "Hello", 5*time.Millisecond, nil)

	rows := g.Snapshot(time.Minute, now)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d: %+v", len(rows), rows)
	}
	if rows[0].Caller != "billing" {
		t.Errorf("caller: got %q, want billing", rows[0].Caller)
	}
}

// callerHMACRequest builds an *http.Request carrying the four HMAC
// fields a signer would send. kid is set only when non-empty.
func callerHMACRequest(callerID, kid, sigB64 string, ts int64) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(PublicCallerIDHeader, callerID)
	r.Header.Set(HMACCallerIDTimestampHeader, strconv.FormatInt(ts, 10))
	if kid != "" {
		r.Header.Set(HMACCallerIDKidHeader, kid)
	}
	r.Header.Set(HMACCallerIDSignatureHeader, sigB64)
	return r
}
