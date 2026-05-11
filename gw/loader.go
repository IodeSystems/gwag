package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// loadProto reads a .proto from disk and compiles it via protocompile
// with SourceInfoStandard so leading / trailing comments survive into
// the returned FileDescriptor. Imports are resolved against the
// entrypoint's directory.
func loadProto(path string) (protoreflect.FileDescriptor, error) {
	dir, file := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			ImportPaths: []string{dir},
		}),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	files, err := c.Compile(context.Background(), file)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", path, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("compile %s: empty result", path)
	}
	return files[0], nil
}

// compileProtoBytes compiles raw .proto entrypoint bytes via
// protocompile with SourceInfoStandard. `entry` is the virtual
// filename used in error messages and as the import-resolution
// anchor; `imports` maps each import path referenced from `body`
// (or transitively) to its raw .proto bytes. Well-known imports
// (google/protobuf/*) resolve automatically via WithStandardImports.
//
// Same shape as the OpenAPI bytes-ingest path: callers ship raw
// source, the gateway compiles. Comments survive end-to-end so they
// land in the GraphQL SDL and the MCP search corpus.
func compileProtoBytes(entry string, body []byte, imports map[string][]byte) (protoreflect.FileDescriptor, error) {
	if entry == "" {
		entry = "entry.proto"
	}
	files := map[string][]byte{entry: body}
	for k, v := range imports {
		files[k] = v
	}
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			Accessor: func(p string) (io.ReadCloser, error) {
				if b, ok := files[p]; ok {
					return io.NopCloser(bytes.NewReader(b)), nil
				}
				return nil, fmt.Errorf("proto import not found: %s", p)
			},
		}),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	out, err := c.Compile(context.Background(), entry)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", entry, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("compile %s: empty result", entry)
	}
	return out[0], nil
}
