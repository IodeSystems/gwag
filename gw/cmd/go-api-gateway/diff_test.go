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

// ---------------------------------------------------------------------
// Optional-vs-required removal — the §3.1 relaxation. Optional
// removal is info; required removal stays breaking.
// ---------------------------------------------------------------------

// TestDiffOptionalArgRemovedNoDefault — nullable arg with no default
// removed: info. Callers who didn't pass it are unaffected; callers
// who did get a recoverable validation error.
func TestDiffOptionalArgRemovedNoDefault(t *testing.T) {
	oldSDL := `type Query { users(limit: Int): [String] }`
	newSDL := `type Query { users: [String] }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "optional arg removed")
	if c == nil {
		t.Fatalf("expected optional arg removed entry, got %+v", changes)
	}
	if c.severity != "info" {
		t.Errorf("severity = %q, want info", c.severity)
	}
	if findChange(changes, "required arg removed") != nil {
		t.Errorf("nullable arg should not surface as required removed")
	}
}

// TestDiffOptionalArgRemovedWithDefault — nullable arg with a default
// removed: still info. The default is gone too, but the type was
// nullable so the caller can already recover.
func TestDiffOptionalArgRemovedWithDefault(t *testing.T) {
	oldSDL := `type Query { users(limit: Int = 10): [String] }`
	newSDL := `type Query { users: [String] }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "optional arg removed")
	if c == nil {
		t.Fatalf("expected optional arg removed entry, got %+v", changes)
	}
	if c.severity != "info" {
		t.Errorf("severity = %q, want info", c.severity)
	}
}

// TestDiffRequiredArgRemoved — non-null arg removed: breaking.
// Callers who passed it had it baked into their query string; now
// the validator rejects unknown args.
func TestDiffRequiredArgRemoved(t *testing.T) {
	oldSDL := `type Query { user(id: ID!): String }`
	newSDL := `type Query { user: String }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "required arg removed")
	if c == nil {
		t.Fatalf("expected required arg removed entry, got %+v", changes)
	}
	if c.severity != "breaking" {
		t.Errorf("severity = %q, want breaking", c.severity)
	}
	if findChange(changes, "optional arg removed") != nil {
		t.Errorf("non-null arg should not surface as optional removed")
	}
}

// TestDiffOptionalInputFieldRemoved — same logic on inputs.
func TestDiffOptionalInputFieldRemoved(t *testing.T) {
	oldSDL := `input Filter { limit: Int, name: String }`
	newSDL := `input Filter { name: String }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "optional input field removed")
	if c == nil {
		t.Fatalf("expected optional input field removed entry, got %+v", changes)
	}
	if c.severity != "info" {
		t.Errorf("severity = %q, want info", c.severity)
	}
}

// TestDiffRequiredInputFieldRemoved — non-null input field removed:
// breaking.
func TestDiffRequiredInputFieldRemoved(t *testing.T) {
	oldSDL := `input Filter { limit: Int!, name: String }`
	newSDL := `input Filter { name: String }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "required input field removed")
	if c == nil {
		t.Fatalf("expected required input field removed entry, got %+v", changes)
	}
	if c.severity != "breaking" {
		t.Errorf("severity = %q, want breaking", c.severity)
	}
}

// TestDiffListArgOptionalOuterRemoved — `[String!]` (nullable list
// of non-null strings) is optional at the outermost wrapping → info.
// The bang-detector keys off the OUTER bang, which is what controls
// whether the value as a whole is required.
func TestDiffListArgOptionalOuterRemoved(t *testing.T) {
	oldSDL := `type Query { users(tags: [String!]): [String] }`
	newSDL := `type Query { users: [String] }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "optional arg removed")
	if c == nil {
		t.Fatalf("expected optional arg removed for [String!], got %+v", changes)
	}
	if c.severity != "info" {
		t.Errorf("severity = %q, want info", c.severity)
	}
}

// TestDiffListArgRequiredRemoved — `[String!]!` is required →
// breaking.
func TestDiffListArgRequiredRemoved(t *testing.T) {
	oldSDL := `type Query { users(tags: [String!]!): [String] }`
	newSDL := `type Query { users: [String] }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "required arg removed")
	if c == nil {
		t.Fatalf("expected required arg removed for [String!]!, got %+v", changes)
	}
	if c.severity != "breaking" {
		t.Errorf("severity = %q, want breaking", c.severity)
	}
}

// ---------------------------------------------------------------------
// Output-field removal stays breaking regardless of nullability:
// selecting a missing output field fails validation against any
// client codegen, even if the field was nullable.
// ---------------------------------------------------------------------

// TestDiffOutputFieldRemovedNonNull stays breaking.
func TestDiffOutputFieldRemovedNonNull(t *testing.T) {
	oldSDL := `type User { id: ID!, name: String! } type Query { user: User }`
	newSDL := `type User { id: ID! } type Query { user: User }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "field removed: User.name")
	if c == nil {
		t.Fatalf("expected field-removed entry for User.name, got %+v", changes)
	}
	if c.severity != "breaking" {
		t.Errorf("severity = %q, want breaking", c.severity)
	}
}

// TestDiffOutputFieldRemovedNullable — still breaking. A client
// query selecting the field validated against the old schema and
// fails against the new one.
func TestDiffOutputFieldRemovedNullable(t *testing.T) {
	oldSDL := `type User { id: ID!, name: String } type Query { user: User }`
	newSDL := `type User { id: ID! } type Query { user: User }`
	changes := runDiff(t, oldSDL, newSDL)
	c := findChange(changes, "field removed: User.name")
	if c == nil {
		t.Fatalf("expected field-removed entry for User.name, got %+v", changes)
	}
	if c.severity != "breaking" {
		t.Errorf("severity = %q, want breaking", c.severity)
	}
}

// ---------------------------------------------------------------------
// No-op: identical schemas produce zero changes.
// ---------------------------------------------------------------------

func TestDiffIdenticalSchemasNoChanges(t *testing.T) {
	sdl := `type Query { users(limit: Int = 10, name: String): [User] } input Filter { limit: Int! } type User { id: ID!, name: String }`
	changes := runDiff(t, sdl, sdl)
	if len(changes) != 0 {
		t.Fatalf("identical schemas produced %d changes:\n%+v", len(changes), changes)
	}
}
