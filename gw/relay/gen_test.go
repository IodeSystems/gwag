package relay

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type sampleEventType string

type sampleEvent struct {
	Type    sampleEventType `json:"type"`
	ID      int64           `json:"id,string"` // always-string-id contract
	Count   int32           `json:"count"`
	Note    string          `json:"note,omitempty"`
	private string          //nolint:unused // must be skipped (unexported)
}

func sampleSpecs() ([]ChannelSpec, []EnumSpec) {
	channels := []ChannelSpec{{Name: "Thing", Event: reflect.TypeOf(sampleEvent{})}}
	enums := []EnumSpec{{
		Name:   "ThingType",
		Type:   reflect.TypeOf(sampleEventType("")),
		Values: []string{"created", "updated"},
	}}
	return channels, enums
}

func TestGenerateTypes_ShapesAndSchemaAgnostic(t *testing.T) {
	channels, enums := sampleSpecs()
	src, err := GenerateTypes(channels, enums)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"export type ThingType = 'created' | 'updated'",
		"export type ThingEvent = {",
		"  type: ThingType",
		"  id: string", // int64 json:",string" → string
		"  count: number",
		"  note: string | null", // omitempty → nullable
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("missing %q\n---\n%s", want, src)
		}
	}
	if strings.Contains(src, "private") {
		t.Fatalf("unexported field leaked:\n%s", src)
	}
	// The emitter must stay schema-agnostic: no transport/GraphQL identifiers.
	for _, forbidden := range []string{"graphql", "gqlClient", "redline", "SubResponse", "import "} {
		if strings.Contains(src, forbidden) {
			t.Fatalf("emitter leaked %q (must be payload-types only)\n%s", forbidden, src)
		}
	}
}

func TestGenClient_WritesTypesAndRuntime(t *testing.T) {
	channels, enums := sampleSpecs()
	dir := t.TempDir()
	if err := GenClient(channels, enums, dir); err != nil {
		t.Fatalf("genclient: %v", err)
	}
	types, err := os.ReadFile(filepath.Join(dir, TypesFile))
	if err != nil || !strings.Contains(string(types), "ThingEvent") {
		t.Fatalf("types file: %v / %s", err, types)
	}
	rt, err := os.ReadFile(filepath.Join(dir, RuntimeFile))
	if err != nil || !strings.Contains(string(rt), "export function useSub") {
		t.Fatalf("runtime file: %v", err)
	}
}
