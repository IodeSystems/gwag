package gateway

import (
	"crypto/hmac"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// ChannelAuthTier is the auth posture applied to a pub/sub channel
// pattern. Tiers are ordered (lowest → strictest):
//
//   - ChannelAuthOpen     — no auth; hmac/ts on the request are ignored.
//   - ChannelAuthHMAC     — HMAC token over (channel, ts) verified
//     against the gateway's WithSubscriptionAuth secret. Hot-path
//     crypto-fast.
//   - ChannelAuthDelegate — delegate authorizer registered under
//     `_pubsub_auth/v1` gets the final say after HMAC. The delegate
//     wiring lands in the follow-up commit; for now this tier is
//     equivalent to ChannelAuthHMAC at runtime so operators can
//     declare it without waiting on the wire-up.
//
// The default tier when no WithChannelAuth pattern matches is
// ChannelAuthHMAC.
type ChannelAuthTier int

const (
	ChannelAuthOpen ChannelAuthTier = iota
	ChannelAuthHMAC
	ChannelAuthDelegate
)

func (t ChannelAuthTier) String() string {
	switch t {
	case ChannelAuthOpen:
		return "open"
	case ChannelAuthHMAC:
		return "hmac"
	case ChannelAuthDelegate:
		return "delegate+hmac"
	}
	return fmt.Sprintf("ChannelAuthTier(%d)", int(t))
}

// channelAuthRule is one operator-declared (pattern → tier) entry.
// Patterns use NATS-style wildcards (`.`-segmented, `*` for one
// segment, `>` for the remainder). Rules are stored in declaration
// order; first-hit-wins at Pub entry, strictest-wins for wildcard Sub.
type channelAuthRule struct {
	Pattern string
	Tier    ChannelAuthTier
}

// WithChannelAuth registers a (pattern, tier) auth rule for the
// gateway's pub/sub primitive (`Mutation.ps.pub` / `Subscription.ps.sub`).
//
// Patterns use NATS-style wildcards:
//
//   - `events.orders.42.update` — literal subject.
//   - `events.orders.*.update`  — `*` matches one segment.
//   - `events.orders.>`         — `>` matches the rest (1+ segments).
//
// Matching rules:
//
//   - At Pub entry the channel is literal. Rules are tried in
//     declaration order; the first matching rule wins.
//   - At Sub open the channel may itself be a wildcard pattern. The
//     gateway computes the strictest tier across every rule whose
//     pattern intersects the requested pattern. If no single rule
//     fully covers the requested pattern, the implicit default
//     (ChannelAuthHMAC) is folded into the strictest-wins
//     computation — so wildcard subs can't leak events from a
//     stricter pattern through a permissive one.
//
// The HMAC token (when required) binds to the channel string as
// requested — a token issued for `events.orders.42.update` does not
// satisfy a wildcard sub on `events.orders.>` and vice versa. The
// operator who hands out tokens controls the pattern surface.
//
// Multiple WithChannelAuth calls compose; declaration order matters.
func WithChannelAuth(pattern string, tier ChannelAuthTier) Option {
	return func(cfg *config) {
		cfg.channelAuth = append(cfg.channelAuth, channelAuthRule{Pattern: pattern, Tier: tier})
	}
}

// resolveChannelTier picks the tier governing `channel`.
//
//   - wildcard==false (Pub): first-hit-wins. No matching rule → HMAC.
//   - wildcard==true (Sub): strictest-wins across all intersecting
//     rules, plus the implicit default (HMAC) when no single rule
//     fully covers the requested pattern.
func (g *Gateway) resolveChannelTier(channel string, wildcard bool) ChannelAuthTier {
	rules := g.cfg.channelAuth
	if !wildcard {
		for _, r := range rules {
			if subjectMatchesPattern(r.Pattern, channel) {
				return r.Tier
			}
		}
		return ChannelAuthHMAC
	}
	strictest := ChannelAuthOpen
	matched := false
	fullyCovered := false
	for _, r := range rules {
		if !patternsIntersect(r.Pattern, channel) {
			continue
		}
		matched = true
		if r.Tier > strictest {
			strictest = r.Tier
		}
		if patternCovers(r.Pattern, channel) {
			fullyCovered = true
		}
	}
	if !matched || !fullyCovered {
		if ChannelAuthHMAC > strictest {
			strictest = ChannelAuthHMAC
		}
	}
	return strictest
}

// checkChannelAuth applies the tier policy for `channel`. wildcard
// indicates a Sub-open (vs Pub) call site so the wildcard-specific
// strictest-wins rule kicks in. Returns nil when the request passes.
//
// Open tier ignores hmac/ts. HMAC and Delegate tiers both run the
// HMAC verifier (delegate fall-through wiring is the next commit).
func (g *Gateway) checkChannelAuth(channel string, wildcard bool, hmacB64 string, ts int64) error {
	tier := g.resolveChannelTier(channel, wildcard)
	if tier == ChannelAuthOpen {
		return nil
	}
	if err := g.verifyChannelHMAC(channel, hmacB64, ts); err != nil {
		return err
	}
	// TODO(plan Tier 1 #5): when tier == ChannelAuthDelegate, consult
	// the _pubsub_auth/v1 delegate after HMAC passes; UNAVAILABLE /
	// NOT_CONFIGURED / transport falls through to HMAC-only, only
	// explicit DENIED short-circuits.
	return nil
}

// verifyChannelHMAC checks the (channel, hmac, ts) tuple. `channel`
// is the *requested* string — concrete for Pub, the wildcard pattern
// the client asked for in Sub. Reuses the gateway's
// WithSubscriptionAuth secret + skew window so operators have a
// single HMAC config surface; the rotation-aware Secrets map is
// honored via the empty-kid entry.
func (g *Gateway) verifyChannelHMAC(channel, hmacB64 string, ts int64) error {
	secret, ok := g.cfg.subAuth.lookupSecret("")
	if !ok {
		return fmt.Errorf("ps: channel auth requires a configured HMAC secret (WithSubscriptionAuth)")
	}
	if hmacB64 == "" {
		return fmt.Errorf("ps: hmac required for channel %q", channel)
	}
	skew := g.cfg.subAuth.SkewWindow
	if skew == 0 {
		skew = defaultSkewWindow
	}
	now := time.Now().Unix()
	if ts < now-int64(skew.Seconds()) {
		return fmt.Errorf("ps: hmac token too old (ts=%d, now=%d)", ts, now)
	}
	if ts > now+int64(skew.Seconds()) {
		return fmt.Errorf("ps: hmac token too new (ts=%d, now=%d)", ts, now)
	}
	provided, err := base64.StdEncoding.DecodeString(hmacB64)
	if err != nil {
		return fmt.Errorf("ps: hmac malformed: %w", err)
	}
	expected := computeSubscribeHMAC(secret, "", channel, ts)
	if !hmac.Equal(expected, provided) {
		return fmt.Errorf("ps: hmac mismatch for channel %q", channel)
	}
	return nil
}

// subjectMatchesPattern reports whether the NATS-style `pattern`
// (with optional `*` and `>` tokens) matches the literal `subject`.
//
// Examples:
//
//	matchSubject("events.orders.>", "events.orders.42.update") → true
//	matchSubject("events.*.update", "events.orders.update")     → true
//	matchSubject("events.orders.>", "events.orders")            → false (> needs ≥1 token)
func subjectMatchesPattern(pattern, subject string) bool {
	if pattern == "" || subject == "" {
		return false
	}
	p := strings.Split(pattern, ".")
	s := strings.Split(subject, ".")
	for i, tok := range p {
		if tok == ">" {
			// `>` matches one-or-more remaining tokens.
			return i < len(s)
		}
		if i >= len(s) {
			return false
		}
		if tok != "*" && tok != s[i] {
			return false
		}
	}
	return len(p) == len(s)
}

// patternsIntersect reports whether two NATS-style patterns share
// any concrete subject. Used to decide which WithChannelAuth rules
// are reachable from a wildcard Sub pattern.
func patternsIntersect(a, b string) bool {
	return tokensIntersect(strings.Split(a, "."), strings.Split(b, "."))
}

func tokensIntersect(a, b []string) bool {
	for len(a) > 0 && len(b) > 0 {
		switch {
		case a[0] == ">", b[0] == ">":
			// `>` matches any non-empty rest on the other side; the
			// loop condition already guarantees ≥1 token remains.
			return true
		case a[0] == "*" || b[0] == "*" || a[0] == b[0]:
			a = a[1:]
			b = b[1:]
		default:
			return false
		}
	}
	return len(a) == 0 && len(b) == 0
}

// patternCovers reports whether every concrete subject matching
// `subPat` also matches `authPat` — i.e. `authPat` is a (non-strict)
// superset pattern. Used to decide whether the implicit default-HMAC
// has to be folded into a wildcard Sub's strictest-wins computation.
func patternCovers(authPat, subPat string) bool {
	return tokensCover(strings.Split(authPat, "."), strings.Split(subPat, "."))
}

func tokensCover(a, b []string) bool {
	for len(a) > 0 && len(b) > 0 {
		switch {
		case a[0] == ">":
			// `>` on a's side soaks up any non-empty b remainder.
			return true
		case b[0] == ">":
			// b's `>` allows arbitrary tails; a must too — and a[0]
			// is not `>` (handled above), so a cannot cover.
			return false
		case a[0] == "*":
			// `*` matches one b segment regardless of value.
			a = a[1:]
			b = b[1:]
		case b[0] == "*":
			// b's segment can be anything; a needs to accept anything
			// at this position too — only `*` does, handled above.
			return false
		case a[0] == b[0]:
			a = a[1:]
			b = b[1:]
		default:
			return false
		}
	}
	return len(a) == 0 && len(b) == 0
}
