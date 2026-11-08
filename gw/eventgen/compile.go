package eventgen

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"

	psv1 "github.com/iodesystems/gwag/v2/gw/proto/ps/v1"
	uploadv1 "github.com/iodesystems/gwag/v2/gw/proto/upload/v1"
)

// embedded are gwag's own option files, served under both the repo
// path (gw/proto/...) and the short path (gwag/...), so users import
// them without a copy on disk. They win over files on -I.
var embedded = map[string]protoreflect.FileDescriptor{
	"gw/proto/ps/v1/options.proto":     psv1.File_gw_proto_ps_v1_options_proto,
	"gwag/ps/v1/options.proto":         psv1.File_gw_proto_ps_v1_options_proto,
	"gw/proto/upload/v1/options.proto": uploadv1.File_gw_proto_upload_v1_options_proto,
	"gwag/upload/v1/options.proto":     uploadv1.File_gw_proto_upload_v1_options_proto,
}

// EmbeddedImports returns the import paths gwag serves itself.
//
// Stability: experimental
func EmbeddedImports() []string {
	out := make([]string, 0, len(embedded))
	for p := range embedded {
		out = append(out, p)
	}
	return out
}

type embeddedResolver struct{}

func (embeddedResolver) FindFileByPath(path string) (protocompile.SearchResult, error) {
	fd, ok := embedded[path]
	if !ok {
		return protocompile.SearchResult{}, os.ErrNotExist
	}
	fdp := protodesc.ToFileDescriptorProto(fd)
	fdp.Name = proto.String(path)
	return protocompile.SearchResult{Proto: fdp}, nil
}

// protoNames maps command-line files to their names relative to an
// import path, as protoc does.
func protoNames(importPaths, files []string) ([]string, error) {
	out := make([]string, 0, len(files))
	for _, f := range files {
		n, err := protoName(importPaths, f)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func protoName(importPaths []string, file string) (string, error) {
	if abs, err := filepath.Abs(file); err == nil {
		if _, err := os.Stat(abs); err == nil {
			for _, ip := range importPaths {
				ipAbs, err := filepath.Abs(ip)
				if err != nil {
					continue
				}
				rel, err := filepath.Rel(ipAbs, abs)
				if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return filepath.ToSlash(rel), nil
				}
			}
			return "", fmt.Errorf("%s is not under any import path (-I %s)", file, strings.Join(importPaths, " -I "))
		}
	}
	for _, ip := range importPaths {
		if _, err := os.Stat(filepath.Join(ip, file)); err == nil {
			return filepath.ToSlash(filepath.Clean(file)), nil
		}
	}
	return "", fmt.Errorf("%s: not found on disk or under -I %s", file, strings.Join(importPaths, " -I "))
}

// compile returns the files to generate and every file they need,
// dependencies first.
func compile(ctx context.Context, importPaths, names []string) ([]protoreflect.FileDescriptor, []protoreflect.FileDescriptor, error) {
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(protocompile.CompositeResolver{
			embeddedResolver{},
			&protocompile.SourceResolver{ImportPaths: importPaths},
		}),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	res, err := c.Compile(ctx, names...)
	if err != nil {
		return nil, nil, err
	}
	files := make([]protoreflect.FileDescriptor, len(res))
	for i, f := range res {
		files[i] = f
	}
	var all []protoreflect.FileDescriptor
	seen := map[string]bool{}
	var visit func(fd protoreflect.FileDescriptor)
	visit = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		imps := fd.Imports()
		for i := 0; i < imps.Len(); i++ {
			visit(imps.Get(i).FileDescriptor)
		}
		all = append(all, fd)
	}
	for _, f := range files {
		visit(f)
	}
	return files, all, nil
}

func buildRequest(names []string, all []protoreflect.FileDescriptor, param string) *pluginpb.CodeGeneratorRequest {
	req := &pluginpb.CodeGeneratorRequest{FileToGenerate: names}
	if param != "" {
		req.Parameter = proto.String(param)
	}
	gen := map[string]bool{}
	for _, n := range names {
		gen[n] = true
	}
	for _, fd := range all {
		fdp := protodesc.ToFileDescriptorProto(fd)
		req.ProtoFile = append(req.ProtoFile, fdp)
		if gen[fd.Path()] {
			req.SourceFileDescriptors = append(req.SourceFileDescriptors, proto.Clone(fdp).(*descriptorpb.FileDescriptorProto))
		}
	}
	return req
}
