package gateway

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrGenerateAdminToken_InMemory(t *testing.T) {
	a, err := loadOrGenerateAdminToken("")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 {
		t.Fatalf("token len = %d, want 32", len(a))
	}
	b, err := loadOrGenerateAdminToken("")
	if err != nil {
		t.Fatal(err)
	}
	// Two in-memory tokens must differ — no persistence.
	if string(a) == string(b) {
		t.Fatalf("expected fresh tokens to differ across calls")
	}
}

func TestLoadOrGenerateAdminToken_Persists(t *testing.T) {
	dir := t.TempDir()
	a, err := loadOrGenerateAdminToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadOrGenerateAdminToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("token didn't persist across reload")
	}
	raw, err := os.ReadFile(filepath.Join(dir, adminTokenFilename))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("file content not hex: %v", err)
	}
	if string(decoded) != string(a) {
		t.Fatalf("on-disk token != returned token")
	}
}

func TestLoadOrGenerateAdminToken_RejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, adminTokenFilename), []byte("not-hex!!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrGenerateAdminToken(dir); err == nil {
		t.Fatalf("expected error on corrupt token file")
	}
}

func TestAdminMiddleware_PublicReads(t *testing.T) {
	gw := New(WithAdminToken([]byte("supersecret")))
	saw := false
	h := gw.AdminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = true
		if IsAdminAuth(r.Context()) {
			t.Errorf("public GET should not mark ctx authenticated")
		}
		w.WriteHeader(http.StatusOK)
	}))

	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		saw = false
		req := httptest.NewRequest(m, "/admin/peers", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("%s want 200, got %d", m, rr.Code)
		}
		if !saw {
			t.Errorf("%s never reached inner handler", m)
		}
	}
}

func TestAdminMiddleware_GatesWrites(t *testing.T) {
	tok := []byte("supersecret")
	tokHex := hex.EncodeToString(tok)
	gw := New(WithAdminToken(tok))
	authed := false
	h := gw.AdminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authed = IsAdminAuth(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name       string
		header     string
		wantStatus int
		wantAuthed bool
	}{
		{"no header", "", 401, false},
		{"empty bearer", "Bearer ", 401, false},
		{"wrong token", "Bearer " + hex.EncodeToString([]byte("nope")), 401, false},
		{"raw (non-hex) match", "Bearer supersecret", 200, true},
		{"hex match", "Bearer " + tokHex, 200, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authed = false
			req := httptest.NewRequest(http.MethodPost, "/admin/peers/x/forget", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if authed != tc.wantAuthed {
				t.Fatalf("IsAdminAuth(ctx) = %v, want %v", authed, tc.wantAuthed)
			}
		})
	}
}

func TestAdminMiddleware_NoTokenConfigured(t *testing.T) {
	// New() always populates a token; assemble the config manually
	// for the misconfigured-explicitly-empty path.
	gw := &Gateway{cfg: &config{adminToken: nil}}
	h := gw.AdminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Reads still pass.
	req := httptest.NewRequest(http.MethodGet, "/admin/peers", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("read got %d, want 200", rr.Code)
	}

	// Writes fail with 500 (misconfiguration, not 401).
	req = httptest.NewRequest(http.MethodPost, "/admin/peers/x/forget", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("write got %d, want 500", rr.Code)
	}
}

func TestForwardOpenAPIHeaders_DefaultsToAuth(t *testing.T) {
	in := httptest.NewRequest(http.MethodPost, "/graphql", nil)
	in.Header.Set("Authorization", "Bearer abc")
	in.Header.Set("X-Other", "ignored")

	out, _ := http.NewRequest(http.MethodPost, "http://upstream/op", nil)
	ctx := WithHTTPRequest(context.Background(), in)
	forwardOpenAPIHeaders(ctx, out, nil)

	if got := out.Header.Get("Authorization"); got != "Bearer abc" {
		t.Errorf("default allowlist should forward Authorization, got %q", got)
	}
	if got := out.Header.Get("X-Other"); got != "" {
		t.Errorf("default allowlist must not forward X-Other, got %q", got)
	}
}

func TestForwardOpenAPIHeaders_EmptyAllowlistForwardsNothing(t *testing.T) {
	in := httptest.NewRequest(http.MethodPost, "/graphql", nil)
	in.Header.Set("Authorization", "Bearer abc")

	out, _ := http.NewRequest(http.MethodPost, "http://upstream/op", nil)
	ctx := WithHTTPRequest(context.Background(), in)
	forwardOpenAPIHeaders(ctx, out, []string{})

	if got := out.Header.Get("Authorization"); got != "" {
		t.Errorf("empty allowlist must drop Authorization, got %q", got)
	}
}

func TestForwardOpenAPIHeaders_CustomAllowlist(t *testing.T) {
	in := httptest.NewRequest(http.MethodPost, "/graphql", nil)
	in.Header.Set("Authorization", "Bearer abc")
	in.Header.Set("X-Api-Key", "k1")
	in.Header.Set("X-Other", "ignored")

	out, _ := http.NewRequest(http.MethodPost, "http://upstream/op", nil)
	ctx := WithHTTPRequest(context.Background(), in)
	forwardOpenAPIHeaders(ctx, out, []string{"X-Api-Key"})

	if got := out.Header.Get("X-Api-Key"); got != "k1" {
		t.Errorf("X-Api-Key not forwarded, got %q", got)
	}
	if got := out.Header.Get("Authorization"); got != "" {
		t.Errorf("custom allowlist must drop Authorization, got %q", got)
	}
	if got := out.Header.Get("X-Other"); got != "" {
		t.Errorf("custom allowlist must drop unlisted header, got %q", got)
	}
}

func TestForwardOpenAPIHeaders_NoInboundCtx(t *testing.T) {
	out, _ := http.NewRequest(http.MethodPost, "http://upstream/op", nil)
	// Should be a no-op, not a panic.
	forwardOpenAPIHeaders(context.Background(), out, nil)
	if got := out.Header.Get("Authorization"); got != "" {
		t.Errorf("expected nothing forwarded, got %q", got)
	}
}

