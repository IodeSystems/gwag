package oidc

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

// Rule kinds.
const (
	// RuleEmail matches one email, case-insensitive.
	RuleEmail = "email"
	// RuleDomain matches every email at one domain (the part after @,
	// case-insensitive, exact: example.com does not match
	// a.example.com).
	RuleDomain = "domain"
	// RuleGroup matches when the groups claim lists the value
	// (case-sensitive: providers send ids or paths).
	RuleGroup = "group"
)

// Rule gives Role to the people it matches.
type Rule struct {
	Kind  string
	Value string
	Role  gateway.AdminSessionRole
}

// ParseRule reads KIND:VALUE=ROLE, e.g. email:alice@example.com=admin,
// domain:example.com=dev, group:gwag-admins=admin.
func ParseRule(s string) (Rule, error) {
	kind, rest, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return Rule{}, fmt.Errorf("%q: want KIND:VALUE=ROLE (KIND email, domain or group; ROLE admin or dev)", s)
	}
	i := strings.LastIndex(rest, "=")
	if i < 0 {
		return Rule{}, fmt.Errorf("%q: want KIND:VALUE=ROLE (KIND email, domain or group; ROLE admin or dev)", s)
	}
	r := Rule{Kind: strings.ToLower(strings.TrimSpace(kind)), Value: strings.TrimSpace(rest[:i]), Role: gateway.AdminSessionRole(strings.ToLower(strings.TrimSpace(rest[i+1:])))}
	if err := r.validate(); err != nil {
		return Rule{}, fmt.Errorf("%q: %w", s, err)
	}
	return r, nil
}

func (r Rule) validate() error {
	switch r.Kind {
	case RuleEmail, RuleDomain, RuleGroup:
	default:
		return fmt.Errorf("kind %q: want email, domain or group", r.Kind)
	}
	if r.Value == "" {
		return errors.New("value is empty")
	}
	if r.Kind == RuleEmail && !strings.Contains(r.Value, "@") {
		return fmt.Errorf("email %q has no @", r.Value)
	}
	if r.Kind == RuleDomain && strings.Contains(r.Value, "@") {
		return fmt.Errorf("domain %q: give the part after @", r.Value)
	}
	switch r.Role {
	case gateway.AdminSessionAdmin, gateway.AdminSessionDev:
	default:
		return fmt.Errorf("role %q: want admin or dev", r.Role)
	}
	return nil
}

func (r Rule) matches(email string, groups []string) bool {
	switch r.Kind {
	case RuleEmail:
		return strings.EqualFold(strings.TrimSpace(r.Value), email)
	case RuleDomain:
		_, dom, ok := strings.Cut(email, "@")
		return ok && strings.EqualFold(strings.TrimPrefix(r.Value, "."), dom)
	case RuleGroup:
		return slices.Contains(groups, r.Value)
	}
	return false
}

// roleOf is the role of the first rule that matches; ok false when
// none does.
func (s *SSO) roleOf(email string, groups []string) (gateway.AdminSessionRole, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", false
	}
	for _, r := range s.cfg.Rules {
		if r.matches(email, groups) {
			return r.Role, true
		}
	}
	return "", false
}

func (s *SSO) hasGroupRules() bool {
	for _, r := range s.cfg.Rules {
		if r.Kind == RuleGroup {
			return true
		}
	}
	return false
}

// ruleGroups keeps the groups a rule names, in claim order.
func (s *SSO) ruleGroups(groups []string) []string {
	var out []string
	for _, g := range groups {
		for _, r := range s.cfg.Rules {
			if r.Kind == RuleGroup && r.Value == g && !slices.Contains(out, g) {
				out = append(out, g)
			}
		}
	}
	return out
}
