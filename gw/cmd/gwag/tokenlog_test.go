package main

import (
	"strings"
	"testing"
)

func TestTokenLine(t *testing.T) {
	const val = "deadbeef"
	cases := []struct {
		name          string
		path          string
		tty, service  bool
		wantVal       bool
		wantSubstring string
	}{
		{"tty file", "/d/dev-token", true, false, true, "/d/dev-token"},
		{"tty memory", "", true, false, true, "in-memory"},
		{"tty service file", "/d/dev-token", true, true, true, "/d/dev-token"},
		{"pipe file", "/d/dev-token", false, false, false, "read the file"},
		{"service file", "/d/dev-token", false, true, false, "gwag service login"},
		{"pipe memory", "", false, false, false, "in-memory"},
	}
	for _, c := range cases {
		got := tokenLine("dev token", val, c.path, c.tty, c.service)
		if strings.Contains(got, val) != c.wantVal {
			t.Errorf("%s: value present=%v, want %v: %q", c.name, !c.wantVal, c.wantVal, got)
		}
		if !strings.Contains(got, c.wantSubstring) {
			t.Errorf("%s: %q lacks %q", c.name, got, c.wantSubstring)
		}
	}
}
