package main

import (
	"strings"
	"testing"
)

// runDiff parses two SDLs and returns the diff. Test helper.
func runDiff(t *testing.T, oldSDL, newSDL string) []change {
	t.Helper()
	oldM, err := parseSchemaModel(oldSDL)
	if err != nil {
		t.Fatalf("parse old: %v", err)
	}
	newM, err := parseSchemaModel(newSDL)
	if err != nil {
		t.Fatalf("parse new: %v", err)
	}
	return diffModels(oldM, newM)
}

// findChange returns the first change whose msg contains substr, or nil.
func findChange(changes []change, substr string) *change {
	for i := range changes {
		if strings.Contains(changes[i].msg, substr) {
			return &changes[i]
		}
	}
	return nil
}

func TestDiffArgDefaultChanged(t *testing.T) {
	oldSDL := `type Query { users(limit: Int = 10): [String] }`
	newSDL := `type Query { users(limit: Int = 100): [String] }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "arg default changed")
	if c == nil {
		t.Fatalf("expected an arg default-change entry, got %+v", changes)
	}
	if c.severity != "breaking" {
		t.Errorf("severity = %q, want breaking", c.severity)
	}
	if !strings.Contains(c.msg, `Query.users(limit)`) || !strings.Contains(c.msg, `"10"`) || !strings.Contains(c.msg, `"100"`) {
		t.Errorf("msg = %q, want subject + old + new", c.msg)
	}
}

func TestDiffArgDefaultRemoved(t *testing.T) {
	oldSDL := `type Query { users(limit: Int = 10): [String] }`
	newSDL := `type Query { users(limit: Int): [String] }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "arg default removed")
	if c == nil {
		t.Fatalf("expected arg default-removed entry, got %+v", changes)
	}
	if c.severity != "breaking" {
		t.Errorf("severity = %q, want breaking", c.severity)
	}
}

func TestDiffArgDefaultAdded(t *testing.T) {
	oldSDL := `type Query { users(limit: Int): [String] }`
	newSDL := `type Query { users(limit: Int = 10): [String] }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "arg default added")
	if c == nil {
		t.Fatalf("expected arg default-added entry, got %+v", changes)
	}
	if c.severity != "info" {
		t.Errorf("severity = %q, want info", c.severity)
	}
}

func TestDiffArgDefaultUnchanged(t *testing.T) {
	sdl := `type Query { users(limit: Int = 10): [String] }`
	changes := runDiff(t, sdl, sdl)
	if c := findChange(changes, "default"); c != nil {
		t.Fatalf("expected no default-related changes, got %+v", c)
	}
}

func TestDiffInputFieldDefaultChanged(t *testing.T) {
	oldSDL := `input Filter { limit: Int = 10 }`
	newSDL := `input Filter { limit: Int = 100 }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "input field default changed")
	if c == nil {
		t.Fatalf("expected input default-change entry, got %+v", changes)
	}
	if c.severity != "breaking" {
		t.Errorf("severity = %q, want breaking", c.severity)
	}
	if !strings.Contains(c.msg, "Filter.limit") {
		t.Errorf("msg = %q, missing Filter.limit", c.msg)
	}
}

func TestDiffInputFieldDefaultRemoved(t *testing.T) {
	oldSDL := `input Filter { limit: Int = 10 }`
	newSDL := `input Filter { limit: Int }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "input field default removed")
	if c == nil {
		t.Fatalf("expected input default-removed entry, got %+v", changes)
	}
	if c.severity != "breaking" {
		t.Errorf("severity = %q, want breaking", c.severity)
	}
}

func TestDiffInputFieldDefaultAdded(t *testing.T) {
	oldSDL := `input Filter { limit: Int }`
	newSDL := `input Filter { limit: Int = 10 }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "input field default added")
	if c == nil {
		t.Fatalf("expected input default-added entry, got %+v", changes)
	}
	if c.severity != "info" {
		t.Errorf("severity = %q, want info", c.severity)
	}
}

func TestDiffInputFieldDefaultUnchanged(t *testing.T) {
	sdl := `input Filter { limit: Int = 10 }`
	changes := runDiff(t, sdl, sdl)
	if c := findChange(changes, "default"); c != nil {
		t.Fatalf("expected no default-related changes, got %+v", c)
	}
}
