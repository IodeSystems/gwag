package oidc

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// Flags are the command-line settings for SSO, shared by gwag up and
// the example gateway. Register them with RegisterFlags; Build makes
// the SSO, or nil when --sso-issuer is empty.
type Flags struct {
	Issuer               string
	ClientID             string
	ClientSecretEnv      string
	RedirectURL          string
	Scopes               string
	AllowUnverifiedEmail bool
	SessionTTL           time.Duration
	CLITokenTTL          time.Duration
	GroupsClaim          string
	Rules                ruleList
}

type ruleList []string

func (l *ruleList) String() string     { return strings.Join(*l, ",") }
func (l *ruleList) Set(v string) error { *l = append(*l, v); return nil }

// RegisterFlags adds the --sso-* flags to fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	f := &Flags{}
	fs.StringVar(&f.Issuer, "sso-issuer", "", "OpenID Connect issuer URL for admin UI sign-in (e.g. https://accounts.google.com). Empty: SSO off. See docs/admin-auth.md#sso")
	fs.StringVar(&f.ClientID, "sso-client-id", "", "OAuth client id registered at the --sso-issuer provider")
	fs.StringVar(&f.ClientSecretEnv, "sso-client-secret-env", "GWAG_SSO_CLIENT_SECRET", "Environment variable that holds the OAuth client secret. Unset or empty variable: a public client (PKCE only)")
	fs.StringVar(&f.RedirectURL, "sso-redirect-url", "", "Callback URL registered at the provider (https://HOST/auth/callback). Empty: /auth/callback on the address the browser used, X-Forwarded-Proto and X-Forwarded-Host honoured")
	fs.StringVar(&f.Scopes, "sso-scopes", strings.Join(DefaultScopes, ","), "Comma-separated scopes to request")
	fs.BoolVar(&f.AllowUnverifiedEmail, "sso-allow-unverified-email", false, "Admit an email the provider has not marked email_verified. Off: such a sign-in is refused")
	fs.DurationVar(&f.SessionTTL, "sso-session-ttl", DefaultSessionTTL, "How long an SSO session lasts")
	fs.DurationVar(&f.CLITokenTTL, "sso-cli-token-ttl", DefaultCLITokenTTL, "How long a gwag login --sso bearer lasts; never longer than --sso-session-ttl")
	fs.StringVar(&f.GroupsClaim, "sso-groups-claim", "groups", "ID token or userinfo claim that lists the person's groups, for group: rules")
	fs.Var(&f.Rules, "sso-rule", "Role rule KIND:VALUE=ROLE, repeatable, first match wins: email:alice@example.com=admin, domain:example.com=dev, group:gwag-admins=admin. No match: sign-in refused")
	return f
}

// Build returns the SSO the flags describe, nil when --sso-issuer is
// empty.
func (f *Flags) Build() (*SSO, error) {
	if strings.TrimSpace(f.Issuer) == "" {
		return nil, nil
	}
	cfg := Config{
		Issuer:               f.Issuer,
		ClientID:             f.ClientID,
		RedirectURL:          strings.TrimSpace(f.RedirectURL),
		AllowUnverifiedEmail: f.AllowUnverifiedEmail,
		SessionTTL:           f.SessionTTL,
		CLITokenTTL:          f.CLITokenTTL,
		GroupsClaim:          strings.TrimSpace(f.GroupsClaim),
	}
	if f.ClientSecretEnv != "" {
		cfg.ClientSecret = os.Getenv(f.ClientSecretEnv)
	}
	for _, s := range strings.Split(f.Scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			cfg.Scopes = append(cfg.Scopes, s)
		}
	}
	for _, s := range f.Rules {
		r, err := ParseRule(s)
		if err != nil {
			return nil, fmt.Errorf("--sso-rule %w", err)
		}
		cfg.Rules = append(cfg.Rules, r)
	}
	return New(cfg)
}
