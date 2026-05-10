package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoginAddsPrimaryByDefault: first login is auto-primary.
func TestLoginAddsPrimaryByDefault(t *testing.T) {
	t.Chdir(t.TempDir())
	rc := loginCmd([]string{
		"--name", "local",
		"--gateway", "gw.example:50090",
		"--endpoint", "http://gw.example:8080",
		"--token", "deadbeef",
	})
	if rc != 0 {
		t.Fatalf("login rc=%d", rc)
	}
	c, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	if len(c.Logins) != 1 {
		t.Fatalf("len(Logins)=%d, want 1", len(c.Logins))
	}
	got := c.Logins[0]
	if got.Name != "local" || !got.Primary {
		t.Errorf("entry=%+v, want name=local primary=true", got)
	}
	if got.Gateway != "gw.example:50090" || got.Endpoint != "http://gw.example:8080" || got.Bearer != "deadbeef" {
		t.Errorf("fields not persisted: %+v", got)
	}
	info, err := os.Stat(credentialsFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("file perm=%v, want 0600 (bearer is a secret)", mode)
	}
}

// TestLoginSecondNotPrimary: second login is not auto-primary unless asked.
func TestLoginSecondNotPrimary(t *testing.T) {
	t.Chdir(t.TempDir())
	loginCmd([]string{"--name", "local", "--gateway", "a:50090"})
	loginCmd([]string{"--name", "staging", "--gateway", "b:50090"})
	c, _ := loadCredentials()
	if len(c.Logins) != 2 {
		t.Fatalf("len=%d, want 2", len(c.Logins))
	}
	if !c.Logins[0].Primary || c.Logins[1].Primary {
		t.Errorf("primary=%v,%v want true,false", c.Logins[0].Primary, c.Logins[1].Primary)
	}
}

// TestLoginPrimaryFlagPromotesAndDemotes: --primary on a non-first
// login flips primary; previous primary gets demoted.
func TestLoginPrimaryFlagPromotesAndDemotes(t *testing.T) {
	t.Chdir(t.TempDir())
	loginCmd([]string{"--name", "local", "--gateway", "a:50090"})
	loginCmd([]string{"--name", "staging", "--gateway", "b:50090", "--primary"})
	c, _ := loadCredentials()
	if c.Logins[0].Primary {
		t.Errorf("local should be demoted")
	}
	if !c.Logins[1].Primary {
		t.Errorf("staging should be primary")
	}
}

// TestLoginUpsert: re-login under same name overwrites.
func TestLoginUpsert(t *testing.T) {
	t.Chdir(t.TempDir())
	loginCmd([]string{"--name", "local", "--gateway", "a:50090", "--token", "old"})
	loginCmd([]string{"--name", "local", "--gateway", "a:50090", "--token", "new"})
	c, _ := loadCredentials()
	if len(c.Logins) != 1 {
		t.Errorf("len=%d, want 1 after upsert", len(c.Logins))
	}
	if c.Logins[0].Bearer != "new" {
		t.Errorf("bearer=%q, want 'new'", c.Logins[0].Bearer)
	}
}

func TestResolvePicksPrimary(t *testing.T) {
	c := credentials{Logins: []loginEntry{
		{Name: "a"},
		{Name: "b", Primary: true},
		{Name: "c"},
	}}
	got, _ := c.resolve("")
	if got.Name != "b" {
		t.Errorf("got %q, want b (primary)", got.Name)
	}
}

func TestResolveFallsBackToFirst(t *testing.T) {
	c := credentials{Logins: []loginEntry{
		{Name: "a"},
		{Name: "b"},
	}}
	got, _ := c.resolve("")
	if got.Name != "a" {
		t.Errorf("got %q, want a (first; no primary)", got.Name)
	}
}

func TestResolveByName(t *testing.T) {
	c := credentials{Logins: []loginEntry{
		{Name: "a", Primary: true},
		{Name: "b"},
	}}
	got, ok := c.resolve("b")
	if !ok || got.Name != "b" {
		t.Errorf("explicit name should win over primary; got %+v ok=%v", got, ok)
	}
	if _, ok := c.resolve("missing"); ok {
		t.Errorf("missing should return false, not fall back")
	}
}

// TestUseSwitchesPrimary: gwag use NAME flips primary deterministically.
func TestUseSwitchesPrimary(t *testing.T) {
	t.Chdir(t.TempDir())
	loginCmd([]string{"--name", "local", "--gateway", "a:50090"})
	loginCmd([]string{"--name", "staging", "--gateway", "b:50090"})
	if rc := useCmd([]string{"staging"}); rc != 0 {
		t.Fatalf("use rc=%d", rc)
	}
	c, _ := loadCredentials()
	if c.Logins[0].Primary || !c.Logins[1].Primary {
		t.Errorf("after use staging, primary=%v,%v want false,true", c.Logins[0].Primary, c.Logins[1].Primary)
	}
}

// TestLogoutPromotesNextPrimary: removing primary auto-promotes the
// next entry so subsequent resolve() doesn't fall through to flag-only.
func TestLogoutPromotesNextPrimary(t *testing.T) {
	t.Chdir(t.TempDir())
	loginCmd([]string{"--name", "local", "--gateway", "a:50090"})
	loginCmd([]string{"--name", "staging", "--gateway", "b:50090"})
	if rc := logoutCmd([]string{"local"}); rc != 0 {
		t.Fatalf("logout rc=%d", rc)
	}
	c, _ := loadCredentials()
	if len(c.Logins) != 1 {
		t.Fatalf("len=%d, want 1", len(c.Logins))
	}
	if !c.Logins[0].Primary {
		t.Errorf("staging should auto-promote to primary after local removal")
	}
}

// TestLogoutNoArgRemovesPrimary: bare 'gwag logout' removes primary.
func TestLogoutNoArgRemovesPrimary(t *testing.T) {
	t.Chdir(t.TempDir())
	loginCmd([]string{"--name", "local", "--gateway", "a:50090"})
	loginCmd([]string{"--name", "staging", "--gateway", "b:50090", "--primary"})
	if rc := logoutCmd(nil); rc != 0 {
		t.Fatalf("logout rc=%d", rc)
	}
	c, _ := loadCredentials()
	names := []string{}
	for _, e := range c.Logins {
		names = append(names, e.Name)
	}
	if len(names) != 1 || names[0] != "local" {
		t.Errorf("after bare logout, remaining=%v want [local]", names)
	}
}

// TestLogoutAll wipes the entire .gw/ directory.
func TestLogoutAll(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	loginCmd([]string{"--name", "local", "--gateway", "a:50090"})
	if _, err := os.Stat(filepath.Join(dir, contextDir)); err != nil {
		t.Fatalf(".gw/ should exist before --all: %v", err)
	}
	if rc := logoutCmd([]string{"--all"}); rc != 0 {
		t.Fatalf("logout --all rc=%d", rc)
	}
	if _, err := os.Stat(filepath.Join(dir, contextDir)); !os.IsNotExist(err) {
		t.Errorf(".gw/ should not exist after --all (err=%v)", err)
	}
}

func TestLoginEndpointDerivedFromGateway(t *testing.T) {
	t.Chdir(t.TempDir())
	loginCmd([]string{"--gateway", "myhost:50090"})
	c, _ := loadCredentials()
	if c.Logins[0].Endpoint != "http://myhost:8080" {
		t.Errorf("Endpoint=%q, want http://myhost:8080 (derived from --gateway host)", c.Logins[0].Endpoint)
	}
}

func TestLoginWithoutGatewayRejected(t *testing.T) {
	t.Chdir(t.TempDir())
	if rc := loginCmd(nil); rc != 2 {
		t.Errorf("login with no args rc=%d, want 2", rc)
	}
	if _, err := os.Stat(credentialsFile); !os.IsNotExist(err) {
		t.Errorf("credentials.json should not exist after rejected login")
	}
}

func TestResolveCtxPrecedence(t *testing.T) {
	if got := resolveCtx("explicit", "ctx", "fallback"); got != "explicit" {
		t.Errorf("explicit: got %q", got)
	}
	if got := resolveCtx("", "ctx", "fallback"); got != "ctx" {
		t.Errorf("ctx: got %q", got)
	}
	if got := resolveCtx("", "", "fallback"); got != "fallback" {
		t.Errorf("fallback: got %q", got)
	}
}

func TestContextDataDir(t *testing.T) {
	if got := contextDataDir("local"); got != filepath.Join(".gw", "contexts", "local", "data") {
		t.Errorf("contextDataDir('local')=%q", got)
	}
}
