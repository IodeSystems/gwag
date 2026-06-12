package relay

import (
	"errors"
	"testing"
	"time"
)

func TestCapSignVerify_RoundTrip(t *testing.T) {
	secret := []byte("s3cret")
	now := time.Unix(1_000, 0)
	tok, err := Sign(secret, Cap{Channel: "events.drive.123", Exp: now.Unix() + 60})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := Verify(secret, tok, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Channel != "events.drive.123" {
		t.Fatalf("channel = %q", got.Channel)
	}
}

func TestCapVerify_Rejections(t *testing.T) {
	secret := []byte("s3cret")
	now := time.Unix(1_000, 0)
	good := mustSign(t, secret, Cap{Channel: "c", Exp: now.Unix() + 60})

	cases := []struct {
		name  string
		token string
		want  error
	}{
		{"wrong secret", mustSign(t, []byte("other"), Cap{Channel: "c", Exp: now.Unix() + 60}), ErrSignature},
		{"tampered payload", "ZZZ." + afterDot(good), ErrSignature},
		{"tampered sig", beforeDot(good) + ".ZZZZ", ErrSignature},
		{"no dot", "deadbeef", ErrMalformed},
		{"bad base64", "!!!.???", ErrMalformed},
		{"empty", "", ErrMalformed},
		{"expired", mustSign(t, secret, Cap{Channel: "c", Exp: now.Unix() - 1}), ErrExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Verify(secret, tc.token, now); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func mustSign(t *testing.T, secret []byte, c Cap) string {
	t.Helper()
	tok, err := Sign(secret, c)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

func beforeDot(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return s[:i]
		}
	}
	return s
}

func afterDot(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return s[i+1:]
		}
	}
	return s
}
