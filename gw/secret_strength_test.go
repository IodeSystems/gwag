package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
)

const strongTestSecret = "Zx3rQ8vN1mKp7TgYb4HdWc9LsAe2UoJf6RiXq5VnMtBy0Pa"

func TestCheckSecretStrength(t *testing.T) {
	random := make([]byte, 48)
	_, _ = rand.Read(random)
	for name, s := range map[string]string{
		"base64 48":  base64.StdEncoding.EncodeToString(random),
		"fixture":    strongTestSecret,
		"hex 64":     "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"passphrase": "correct horse battery staple 4 Gwag!",
	} {
		if err := CheckSecretStrength([]byte(s)); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	for name, s := range map[string]string{
		"empty":         "",
		"short":         "Zx3rQ8vN1mKp7TgYb4HdWc9",
		"31 random":     strongTestSecret[:31],
		"repeated char": strings.Repeat("a", 64),
		"repeated unit": strings.Repeat("abc", 20),
		"repeated long": strings.Repeat(strongTestSecret[:20], 4),
		"alphabet run":  "abcdefghijklmnopqrstuvwxyz0123456789",
		"digit run":     "01234567890123456789012345678901234567890",
		"descending":    "zyxwvutsrqponmlkjihgfedcba9876543210",
		"two chars":     strings.Repeat("ab", 8) + strings.Repeat("ba", 8) + "abbaabba",
	} {
		err := CheckSecretStrength([]byte(s))
		if err == nil {
			t.Errorf("%s: accepted (%.0f bits)", name, SecretEntropyBits([]byte(s)))
			continue
		}
		if !strings.Contains(err.Error(), "openssl rand -base64 48") {
			t.Errorf("%s: error does not say how to make one: %v", name, err)
		}
	}
}

func TestGenerateSecret_PassesCheck(t *testing.T) {
	a, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateSecret()
	if a == b {
		t.Fatal("two secrets equal")
	}
	if err := CheckSecretStrength([]byte(a)); err != nil {
		t.Fatalf("generated secret refused: %v", err)
	}
}

func TestDeclarationsHook_WeakSecretPanics(t *testing.T) {
	for name, secret := range map[string]string{
		"short":    "s3cret",
		"repeated": strings.Repeat("a", 64),
	} {
		func() {
			defer func() {
				r := recover()
				msg, _ := r.(string)
				if !strings.Contains(msg, "Secret") || !strings.Contains(msg, "openssl rand -base64 48") {
					t.Errorf("%s: panic=%v", name, r)
				}
			}()
			New(WithoutMetrics(), WithDeclarationsHook(DeclarationsHookOptions{URL: "http://x", Secret: []byte(secret)})).Close()
		}()
	}
	// No secret: unsigned, accepted. A strong one: accepted.
	New(WithoutMetrics(), WithDeclarationsHook(DeclarationsHookOptions{URL: "http://x"})).Close()
	New(WithoutMetrics(), WithDeclarationsHook(DeclarationsHookOptions{URL: "http://x", Secret: []byte(strongTestSecret)})).Close()
}

func TestShortHexSecretWarns(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	New(WithoutMetrics(), WithSignerSecret([]byte("short"))).Close()
	New(WithoutMetrics(), WithSubscriptionAuth(SubscriptionAuthOptions{Secret: []byte("short")})).Close()
	out := buf.String()
	if !strings.Contains(out, "signer-secret") || !strings.Contains(out, "subscribe-secret") {
		t.Errorf("no warnings in log: %q", out)
	}

	buf.Reset()
	long := make([]byte, 16)
	New(WithoutMetrics(), WithSignerSecret(long)).Close()
	if strings.Contains(buf.String(), "weak secret") {
		t.Errorf("16 bytes warned: %q", buf.String())
	}
}
