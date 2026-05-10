package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoginLogoutRoundTrip exercises the full ./.gw lifecycle:
// login writes the file, loadContext reads it back, logout removes
// it, and a subsequent loadContext returns the zero value. Uses
// t.Chdir to keep the file under the test's tempdir so concurrent
// runs / cleanup-on-fail can't bleed into each other.
func TestLoginLogoutRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	rc := loginCmd([]string{
		"--gateway", "gw.example:50090",
		"--endpoint", "http://gw.example:8080",
		"--token", "deadbeefcafe",
	})
	if rc != 0 {
		t.Fatalf("login rc=%d", rc)
	}

	got, err := loadContext()
	if err != nil {
		t.Fatalf("loadContext: %v", err)
	}
	if got.Gateway != "gw.example:50090" {
		t.Errorf("Gateway=%q, want gw.example:50090", got.Gateway)
	}
	if got.Endpoint != "http://gw.example:8080" {
		t.Errorf("Endpoint=%q, want http://gw.example:8080", got.Endpoint)
	}
	if got.Bearer != "deadbeefcafe" {
		t.Errorf("Bearer=%q, want deadbeefcafe", got.Bearer)
	}

	info, err := os.Stat(filepath.Join(dir, contextFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The bearer is a secret — file mode must not be world-readable.
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("file perm=%v, want 0600", mode)
	}

	if rc := logoutCmd(nil); rc != 0 {
		t.Fatalf("logout rc=%d", rc)
	}
	got, err = loadContext()
	if err != nil {
		t.Fatalf("loadContext post-logout: %v", err)
	}
	if (got != gwContext{}) {
		t.Errorf("post-logout context=%+v, want zero", got)
	}
}

func TestLoginEndpointDerivedFromGateway(t *testing.T) {
	t.Chdir(t.TempDir())
	if rc := loginCmd([]string{"--gateway", "myhost:50090"}); rc != 0 {
		t.Fatalf("login rc=%d", rc)
	}
	got, _ := loadContext()
	if got.Endpoint != "http://myhost:8080" {
		t.Errorf("Endpoint=%q, want http://myhost:8080 (derived from --gateway host)", got.Endpoint)
	}
}

func TestLoginWithoutGatewayRejected(t *testing.T) {
	t.Chdir(t.TempDir())
	if rc := loginCmd(nil); rc != 2 {
		t.Errorf("login with no args rc=%d, want 2", rc)
	}
	if _, err := os.Stat(contextFile); !os.IsNotExist(err) {
		t.Errorf(".gw should not exist after rejected login")
	}
}

func TestResolveCtxPrecedence(t *testing.T) {
	if got := resolveCtx("explicit", "ctx", "fallback"); got != "explicit" {
		t.Errorf("explicit > ctx > fallback: got %q", got)
	}
	if got := resolveCtx("", "ctx", "fallback"); got != "ctx" {
		t.Errorf("ctx > fallback: got %q", got)
	}
	if got := resolveCtx("", "", "fallback"); got != "fallback" {
		t.Errorf("fallback: got %q", got)
	}
	if got := resolveCtx("", "", ""); got != "" {
		t.Errorf("all empty: got %q", got)
	}
}

func TestLogoutNoFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if rc := logoutCmd(nil); rc != 0 {
		t.Errorf("logout with no .gw rc=%d, want 0 (idempotent)", rc)
	}
}
