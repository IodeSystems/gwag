package oidc

import (
	"flag"
	"testing"
	"time"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

func TestFlags(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	f := RegisterFlags(fs)
	if sso, err := f.Build(); sso != nil || err != nil {
		t.Fatalf("no issuer: %v %v", sso, err)
	}
	t.Setenv("MY_SECRET", "s3")
	err := fs.Parse([]string{
		"--sso-issuer", "https://idp.example.com", "--sso-client-id", "gw",
		"--sso-client-secret-env", "MY_SECRET", "--sso-scopes", "openid, email,groups",
		"--sso-session-ttl", "1h", "--sso-groups-claim", "roles",
		"--sso-rule", "email:a@example.com=dev", "--sso-rule", "domain:example.com=admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	sso, err := f.Build()
	if err != nil {
		t.Fatal(err)
	}
	c := sso.cfg
	if c.ClientSecret != "s3" || len(c.Scopes) != 3 || c.Scopes[2] != "groups" || c.SessionTTL != time.Hour || c.GroupsClaim != "roles" {
		t.Errorf("cfg = %+v", c)
	}
	if len(c.Rules) != 2 || c.Rules[0].Role != gateway.AdminSessionDev || c.Rules[1].Kind != RuleDomain {
		t.Errorf("rules = %+v", c.Rules)
	}

	fs = flag.NewFlagSet("x", flag.ContinueOnError)
	f = RegisterFlags(fs)
	_ = fs.Parse([]string{"--sso-issuer", "https://idp", "--sso-client-id", "gw", "--sso-rule", "email:a@b=owner"})
	if _, err := f.Build(); err == nil {
		t.Error("bad rule accepted")
	}
	fs = flag.NewFlagSet("x", flag.ContinueOnError)
	f = RegisterFlags(fs)
	_ = fs.Parse([]string{"--sso-issuer", "https://idp", "--sso-client-id", "gw"})
	if _, err := f.Build(); err == nil {
		t.Error("no rules accepted")
	}
}
