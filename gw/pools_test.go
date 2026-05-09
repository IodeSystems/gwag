package gateway

import (
	"strings"
	"testing"
)

// Locks the registration alphabet down to "unstable" + "vN" (N ≥ 1)
// plus the empty-defaults-to-v1 back-compat hatch. The tier model in
// plan §4 is the only vocabulary that should ever land in registry KV.
func TestParseVersion_Accepted(t *testing.T) {
	cases := []struct {
		in       string
		wantStr  string
		wantN    int
	}{
		{"", "v1", 1},
		{"v1", "v1", 1},
		{"v2", "v2", 2},
		{"v10", "v10", 10},
		{"v100", "v100", 100},
		{"unstable", "unstable", 0},
	}
	for _, c := range cases {
		gotStr, gotN, err := parseVersion(c.in)
		if err != nil {
			t.Errorf("parseVersion(%q): unexpected error: %v", c.in, err)
			continue
		}
		if gotStr != c.wantStr || gotN != c.wantN {
			t.Errorf("parseVersion(%q) = (%q, %d), want (%q, %d)",
				c.in, gotStr, gotN, c.wantStr, c.wantN)
		}
	}
}

func TestParseVersion_Rejected(t *testing.T) {
	cases := []struct {
		in       string
		wantSubs string // substring expected in the error
	}{
		// "stable" is the computed alias — never a registerable tier.
		{"stable", "computed alias"},

		// Bare digits, uppercase, leading zeros: pre-tier-model
		// permissiveness, now rejected so registry KV stays
		// canonical.
		{"3", "unstable"},
		{"V3", "unstable"},
		{"v0", "leading zeros"},
		{"v01", "leading zeros"},
		{"v", "unstable"},
		{"v-1", "unstable"},
		{"vNext", "unstable"},
		{"latest", "unstable"},
		{"foo", "unstable"},
	}
	for _, c := range cases {
		_, _, err := parseVersion(c.in)
		if err == nil {
			t.Errorf("parseVersion(%q): expected error, got nil", c.in)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSubs) {
			t.Errorf("parseVersion(%q): error %q missing %q", c.in, err.Error(), c.wantSubs)
		}
	}
}
