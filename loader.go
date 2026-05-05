package gateway

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func loadProto(path string) (protoreflect.FileDescriptor, error) {
	dir, file := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			ImportPaths: []string{dir},
		}),
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
