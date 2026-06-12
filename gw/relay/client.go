package relay

import (
	_ "embed"
	"os"
	"path/filepath"
)

// runtimeTS is the static TS runtime emitted alongside the generated types — the
// adopter never vendors it, GenClient/WriteRuntime spit it out.
//
//go:embed templates/live-runtime.ts
var runtimeTS string

// RuntimeFile / TypesFile are the conventional output filenames.
const (
	RuntimeFile = "live-runtime.ts"
	TypesFile   = "live-types.ts"
)

// GenClient writes the typed live client into outDir: the generated payload
// types (TypesFile, from the adopter's Go event structs) plus the static runtime
// (RuntimeFile). This is what "the relay spits out the client" means — call it
// from the adopter's build (go:generate) with their channel registry.
func GenClient(channels []ChannelSpec, enums []EnumSpec, outDir string) error {
	types, err := GenerateTypes(channels, enums)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, TypesFile), []byte(types), 0o644); err != nil {
		return err
	}
	return WriteRuntime(outDir)
}

// WriteRuntime emits just the static runtime (no registry needed) — the
// `gnat-ws-relay gen runtime` path.
func WriteRuntime(outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, RuntimeFile), []byte(runtimeTS), 0o644)
}
