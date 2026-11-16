package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebBasePrecedence(t *testing.T) {
	for _, c := range []struct{ flag, ctx, want string }{
		{"http://f:1", "http://c:2", "http://f:1"},
		{"", "http://c:2", "http://c:2"},
		{"", "", ""}, // webLogin falls back to the endpoint
		{"http://f:1", "", "http://f:1"},
	} {
		if got := webBase(c.flag, c.ctx); got != c.want {
			t.Errorf("webBase(%q,%q)=%q want %q", c.flag, c.ctx, got, c.want)
		}
	}
}

func TestWebURLOldFileLoads(t *testing.T) {
	t.Chdir(t.TempDir())
	old := `{"logins":[{"name":"a","primary":true,"gateway":"g:1","endpoint":"http://127.0.0.1:8080","bearer":"ab"}]}`
	if err := os.MkdirAll(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsFile, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadCredentials()
	if err != nil || len(c.Logins) != 1 || c.Logins[0].WebURL != "" || c.Logins[0].Bearer != "ab" {
		t.Fatalf("%+v %v", c, err)
	}
	// An empty web_url is not written back.
	if err := saveCredentials(c); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(credentialsFile)
	if strings.Contains(string(b), "web_url") {
		t.Errorf("web_url written when empty: %s", b)
	}
}

func TestLoginAndUseWebURL(t *testing.T) {
	t.Chdir(t.TempDir())
	if rc := loginCmd([]string{"--name", "a", "--gateway", "g:1", "--web-url", "http://192.168.1.76:8080/x"}); rc != 0 {
		t.Fatalf("login rc=%d", rc)
	}
	web := func() string { c, _ := loadCredentials(); return c.Logins[0].WebURL }
	if web() != "http://192.168.1.76:8080/x" {
		t.Fatalf("web_url %q", web())
	}
	// Relogin without --web-url keeps it.
	if rc := loginCmd([]string{"--name", "a", "--gateway", "g:1", "--token", "ff", "--primary"}); rc != 0 || web() == "" {
		t.Errorf("relogin dropped web_url: rc=%d %q", rc, web())
	}
	// use --web-url sets, "" clears, bad value is refused, primary unchanged.
	loginCmd([]string{"--name", "b", "--gateway", "h:1"})
	if rc := useCmd([]string{"b", "--web-url", "https://gw.example"}); rc != 0 {
		t.Fatalf("use rc=%d", rc)
	}
	c, _ := loadCredentials()
	if c.Logins[1].WebURL != "https://gw.example" || !c.Logins[0].Primary || c.Logins[1].Primary {
		t.Errorf("after use: %+v", c.Logins)
	}
	if rc := useCmd([]string{"b", "--web-url", ""}); rc != 0 {
		t.Fatal(rc)
	}
	if c, _ = loadCredentials(); c.Logins[1].WebURL != "" {
		t.Errorf("not cleared: %+v", c.Logins[1])
	}
	if rc := useCmd([]string{"b", "--web-url", "gw.example"}); rc != 2 {
		t.Errorf("bad url rc=%d", rc)
	}
	if rc := useCmd([]string{"nope", "--web-url", "http://x:1"}); rc != 1 {
		t.Errorf("unknown login rc=%d", rc)
	}
	if rc := loginCmd([]string{"--name", "c", "--gateway", "h:1", "--web-url", "bad"}); rc != 2 {
		t.Errorf("bad login url rc=%d", rc)
	}
}

func TestServicePublicURL(t *testing.T) {
	env, _, out := newTestEnv(t, "linux")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(env.home, "cfg"))
	if err := env.install([]string{"--name", "pu", "--public-url", "ftp://x"}); err == nil {
		t.Fatal("bad --public-url accepted")
	}
	if err := env.install([]string{"--name", "pu", "--public-url", "http://192.168.1.76:18790", "--", "--addr", "127.0.0.1:18790"}); err != nil {
		t.Fatal(err)
	}
	dd := env.installedDataDir("pu")
	if readPublicURL(dd) != "http://192.168.1.76:18790" {
		t.Fatalf("stored %q", readPublicURL(dd))
	}
	unit, _ := os.ReadFile(env.defPath("pu"))
	if strings.Contains(string(unit), "192.168.1.76") {
		t.Errorf("public url in the unit: %s", unit)
	}
	// Reinstall without the flag keeps it.
	if err := env.install([]string{"--name", "pu", "--", "--addr", "127.0.0.1:18790"}); err != nil {
		t.Fatal(err)
	}
	if readPublicURL(dd) == "" {
		t.Fatal("reinstall dropped public-url")
	}
	os.WriteFile(filepath.Join(dd, "admin-token"), []byte("deadbeef\n"), 0o600)

	// status shows it.
	out.Reset()
	if err := env.control("status", []string{"--name", "pu"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "public-url: http://192.168.1.76:18790") {
		t.Errorf("status: %q", out.String())
	}

	// login copies it into the context; --web uses it unless --web-url.
	if err := env.login([]string{"--name", "pu"}); err != nil {
		t.Fatal(err)
	}
	c, _ := loadCredentialsAt(filepath.Join(env.home, "cfg", "gwag", "credentials.json"))
	if len(c.Logins) != 1 || c.Logins[0].WebURL != "http://192.168.1.76:18790" {
		t.Errorf("context %+v", c.Logins)
	}
	var base string
	env.webLogin = func(_ context.Context, _, _, b string, _ bool, _ io.Writer) error { base = b; return nil }
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--name", "pu", "--web"}, "http://192.168.1.76:18790"},
		{[]string{"--name", "pu", "--web", "--web-url", "http://o:1"}, "http://o:1"},
	} {
		if err := env.login(tc.args); err != nil || base != tc.want {
			t.Errorf("%v: base %q err %v", tc.args, base, err)
		}
	}

	// --public-url "" clears.
	if err := env.install([]string{"--name", "pu", "--public-url", "", "--", "--addr", "127.0.0.1:18790"}); err != nil {
		t.Fatal(err)
	}
	if readPublicURL(dd) != "" {
		t.Error("not cleared")
	}
	if err := env.login([]string{"--name", "pu", "--web"}); err != nil || base != "" {
		t.Errorf("after clear: base %q err %v", base, err)
	}
}
