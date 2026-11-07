package gateway

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"math"
)

// Secret strength. A shared HMAC key guessed offline lets an attacker
// forge every signature made with it, so a weak one is refused where
// the secret is a raw string (the declarations hook). The check is a
// floor against laziness ("changeme", "aaaa...", "abcabc..."), not a
// proof of randomness: it cannot see a dictionary phrase.
//
// Rule: a secret needs at least MinSecretBytes bytes AND an estimated
// MinSecretEntropyBits bits of entropy, where the estimate is:
//
//  1. Period collapse. If the secret is a shorter unit repeated
//     ("abcabcabc", "xyzxyz"), only the unit counts.
//  2. Run discount. A byte that continues a constant-step run with
//     the two bytes before it ("aaaa", "abcdef", "97531") counts as
//     nothing.
//  3. Shannon entropy H (bits per byte, from the byte frequencies of
//     what is left) times the number of bytes left.
//
// A random base64 string of 48 bytes (`openssl rand -base64 48`)
// scores about 250 bits. A random 32-character hex string scores
// about 118 bits and is refused: use 48 or more.

const (
	// MinSecretBytes is the shortest accepted raw secret.
	MinSecretBytes = 32
	// MinSecretEntropyBits is the lowest accepted entropy estimate.
	MinSecretEntropyBits = 128
	// shortHexSecretBytes is the decoded length under which a hex
	// secret logs a startup warning.
	shortHexSecretBytes = 16
)

// SecretEntropyBits is the entropy estimate described above, in bits.
//
// Stability: experimental
func SecretEntropyBits(secret []byte) float64 {
	b := dropRuns(secretUnit(secret))
	if len(b) == 0 {
		return 0
	}
	var freq [256]int
	for _, c := range b {
		freq[c]++
	}
	n := float64(len(b))
	h := 0.0
	for _, f := range freq {
		if f == 0 {
			continue
		}
		p := float64(f) / n
		h -= p * math.Log2(p)
	}
	return h * n
}

// secretUnit returns the shortest prefix whose repetition makes b.
func secretUnit(b []byte) []byte {
	for p := 1; p <= len(b)/2; p++ {
		ok := true
		for i := p; i < len(b); i++ {
			if b[i] != b[i-p] {
				ok = false
				break
			}
		}
		if ok {
			return b[:p]
		}
	}
	return b
}

// dropRuns removes every byte that continues a constant-step run.
func dropRuns(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i, c := range b {
		if i >= 2 && int(c)-int(b[i-1]) == int(b[i-1])-int(b[i-2]) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// CheckSecretStrength returns nil when secret meets the rule above,
// otherwise an error that says how to make a good one.
//
// Stability: experimental
func CheckSecretStrength(secret []byte) error {
	const hint = "make one with `openssl rand -base64 48` or `gwag secret gen`"
	if len(secret) < MinSecretBytes {
		return fmt.Errorf("secret is %d bytes, need at least %d: %s", len(secret), MinSecretBytes, hint)
	}
	if bits := SecretEntropyBits(secret); bits < MinSecretEntropyBits {
		return fmt.Errorf("secret looks too predictable (estimated %.0f bits of entropy, need at least %d; repetition and runs do not count): %s", bits, MinSecretEntropyBits, hint)
	}
	return nil
}

// GenerateSecret returns a random secret of 48 bytes, base64-encoded
// (64 characters, 384 bits). It passes CheckSecretStrength.
//
// Stability: experimental
func GenerateSecret() (string, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// warnShortHexSecret logs a startup warning for a decoded hex secret
// under 16 bytes. It does not refuse: existing configs keep working.
func warnShortHexSecret(name string, secret []byte) {
	if len(secret) == 0 || len(secret) >= shortHexSecretBytes {
		return
	}
	slog.Warn("gwag: weak secret: shorter than 16 bytes decoded; use 32 random bytes (`openssl rand -hex 32`)",
		"secret", name, "bytes", len(secret))
}
