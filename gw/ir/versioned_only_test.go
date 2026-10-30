package ir

import (
	"strings"
	"testing"
)

func TestVersionedOnly_DropsFlatCopy(t *testing.T) {
	svc := &Service{
		Namespace:  "demo",
		OriginKind: KindOpenAPI,
		Version:    "v1",
		Operations: []*Operation{
			{Name: "ping", Kind: OpQuery, Output: &TypeRef{Builtin: ScalarString}},
			{Name: "ticks", Kind: OpSubscription, Output: &TypeRef{Builtin: ScalarString}},
		},
	}
	render := func(versionedOnly bool) string {
		schema, err := RenderGraphQLRuntime([]*Service{svc}, NewDispatchRegistry(), RuntimeOptions{VersionedOnly: versionedOnly})
		if err != nil {
			t.Fatalf("RenderGraphQLRuntime: %v", err)
		}
		return PrintSchemaSDL(schema)
	}

	sdl := render(false)
	if strings.Count(sdl, "ping:") != 2 || !strings.Contains(sdl, "demo_ticks:") {
		t.Fatalf("default should carry the flat copy:\n%s", sdl)
	}
	sdl = render(true)
	if strings.Count(sdl, "ping:") != 1 || !strings.Contains(sdl, "v1: DemoV1QueryNamespace!") {
		t.Errorf("want ping only under v1:\n%s", sdl)
	}
	if !strings.Contains(sdl, "demo_v1_ticks:") || strings.Contains(sdl, "demo_ticks:") {
		t.Errorf("want subscription demo_v1_ticks only:\n%s", sdl)
	}
}
