package ir

import "testing"

// The Long scalar's honest number|string contract: a JS-safe magnitude serializes as a
// number, anything that would lose precision as a JS number serializes as a string.
func TestLongScalar_NumberStringHonest(t *testing.T) {
	long, _ := StandardScalarsWith(true)

	cases := []struct {
		name string
		in   any
		want any
	}{
		{"small id → number", int64(113334), int64(113334)},
		{"boundary 2^53-1 → number", maxSafeInteger, maxSafeInteger},
		{"above boundary → string", maxSafeInteger + 1, "9007199254740992"},
		{"negative above boundary → string", -maxSafeInteger - 1, "-9007199254740992"},
		// Lenient input: even a stringified value from the convert path is classified by magnitude.
		{"stringified small → number", "113334", int64(113334)},
		{"stringified big → string", "9007199254740992", "9007199254740992"},
		{"float small → number", float64(42), int64(42)},
	}
	for _, c := range cases {
		if got := long.Serialize(c.in); got != c.want {
			t.Errorf("%s: Serialize(%v) = %T(%v), want %T(%v)", c.name, c.in, got, got, c.want, c.want)
		}
	}
}

// Default (legacy) encoding is always a string — back-compat for gateways that don't opt in.
func TestLongScalar_LegacyDefaultAlwaysString(t *testing.T) {
	long, _ := StandardScalars()
	if got := long.Serialize(int64(113334)); got != "113334" {
		t.Fatalf("legacy Long should always be a string, got %T(%v)", got, got)
	}
}

// Input is lenient regardless of output mode: a JSON number or a decimal string both
// normalize to the decimal-string the dispatch binds to int64.
func TestLongScalar_ParseLenient(t *testing.T) {
	long, _ := StandardScalarsWith(true)
	if got := long.ParseValue(float64(113334)); got != "113334" {
		t.Errorf("number input should normalize to decimal string, got %T(%v)", got, got)
	}
	if got := long.ParseValue("113334"); got != "113334" {
		t.Errorf("string input should normalize, got %T(%v)", got, got)
	}
}
