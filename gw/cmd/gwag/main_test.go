package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAllowTier(t *testing.T) {
	t.Run("default-all", func(t *testing.T) {
		got, err := parseAllowTier("unstable,stable,vN")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := []string{"unstable", "stable", "vN"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("vN-only", func(t *testing.T) {
		got, err := parseAllowTier("vN")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"vN"}) {
			t.Errorf("got %v, want [vN]", got)
		}
	})

	t.Run("whitespace-tolerated", func(t *testing.T) {
		got, err := parseAllowTier(" stable , vN ")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"stable", "vN"}) {
			t.Errorf("got %v, want [stable vN]", got)
		}
	})

	t.Run("rejects-unknown", func(t *testing.T) {
		_, err := parseAllowTier("vN,prod")
		if err == nil {
			t.Fatalf("expected error for unknown tier")
		}
		if !strings.Contains(err.Error(), "unknown tier") {
			t.Errorf("error %q missing 'unknown tier'", err.Error())
		}
	})

	t.Run("rejects-empty", func(t *testing.T) {
		if _, err := parseAllowTier(""); err == nil {
			t.Errorf("expected error for empty")
		}
		if _, err := parseAllowTier(",,,"); err == nil {
			t.Errorf("expected error for all-empty entries")
		}
	})
}
