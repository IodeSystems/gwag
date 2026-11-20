package paramfmt

import (
	"encoding/json"
	"testing"
)

func TestString(t *testing.T) {
	cases := []struct {
		in   any
		want string
		ok   bool
	}{
		{nil, "", false},
		{"a b", "a b", true},
		{float64(3e9), "3000000000", true},
		{float64(2), "2", true},
		{2.5, "2.5", true},
		{1e21, "1000000000000000000000", true},
		{1.5e-7, "0.00000015", true},
		{float32(2.5), "2.5", true},
		{json.Number("12345678901234567890"), "12345678901234567890", true},
		{json.Number("1e3"), "1e3", true},
		{int32(7), "7", true},
		{true, "true", true},
	}
	for _, c := range cases {
		got, ok := String(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("String(%#v) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
