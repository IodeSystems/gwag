package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

// testProtoBytes loads the named .proto file for tests migrated off
// AddProtoDescriptor. Searches well-known locations relative to the
// gw/ test working dir; all stable filenames within the repo (no
// fragile go:embed go-up-paths). Pass the returned bytes to
// AddProtoBytes.
//
// For multi-file .protos, callers use a `map[string][]byte`
// (typically with the bytes for each transitive .proto) and pass
// that via the ProtoImports(...) option.
func testProtoBytes(tb testing.TB, name string) []byte {
	tb.Helper()
	candidates := []string{
		filepath.Join("proto", "adminauth", "v1", name),
		filepath.Join("proto", "adminevents", "v1", name),
		filepath.Join("proto", "callerauth", "v1", name),
		filepath.Join("proto", "eventsauth", "v1", name),
		filepath.Join("proto", "pubsubauth", "v1", name),
		filepath.Join("proto", "controlplane", "v1", name),
		filepath.Join("..", "examples", "multi", "protos", name),
		filepath.Join("..", "examples", "auth", "protos", name),
	}
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			return b
		}
	}
	tb.Fatalf("test proto not found: %s (checked %v)", name, candidates)
	return nil
}
