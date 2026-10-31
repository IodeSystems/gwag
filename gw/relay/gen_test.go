package relay

import (
	"os"
	"os/exec"
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

// TestLiveRuntime runs the generated runtime under node against a fake
// WebSocket (testdata/live-runtime.test.mjs). It needs node 22.6+ to load
// TypeScript, so it skips elsewhere.
func TestLiveRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	if err := WriteRuntime(dir); err != nil {
		t.Fatal(err)
	}
	test, err := os.ReadFile("testdata/live-runtime.test.mjs")
	if err != nil {
		t.Fatal(err)
	}
	react := filepath.Join(dir, "node_modules", "react")
	if err := os.MkdirAll(react, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		filepath.Join(dir, "live-runtime.test.mjs"): string(test),
		filepath.Join(react, "package.json"):        `{"name":"react","type":"module","main":"index.js"}`,
		filepath.Join(react, "index.js"):            `export const useEffect = () => {}, useRef = () => ({}), useSyncExternalStore = () => {}`,
	} {
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := exec.Command(node, "--experimental-strip-types", "--eval", "").Run(); err != nil {
		t.Skip("node cannot load TypeScript")
	}
	cmd := exec.Command(node, "--experimental-strip-types", "--no-warnings", "--test", "live-runtime.test.mjs")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
