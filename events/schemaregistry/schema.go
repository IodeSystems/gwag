package schemaregistry

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Builtin reports whether a registry knows the import path without a
// reference: google/protobuf/*, google/type/* and confluent/*.
//
// Stability: experimental
func Builtin(path string) bool {
	return strings.HasPrefix(path, "google/protobuf/") || strings.HasPrefix(path, "google/type/") || strings.HasPrefix(path, "confluent/")
}

// TreeFile is one file of a Tree.
//
// Stability: experimental
type TreeFile struct {
	// Path is the import path the file is compiled under.
	Path string
	// Source is the .proto text as the registry returned it.
	Source string
	// Subject and Version are where the file is registered (empty for
	// the root of a schema fetched by id).
	Subject string
	Version int
	ID      int
}

// Tree is a schema and every schema it references, as .proto source.
//
// Stability: experimental
type Tree struct {
	// Root is the path of the schema itself.
	Root string
	// Files holds the root and each reference, references first.
	Files []TreeFile
}

// Tree fetches the references of s (recursively, as .proto text) and
// returns s and them as a Tree with s at path root.
//
// Stability: experimental
func (c *Client) Tree(ctx context.Context, s *Schema, root string) (*Tree, error) {
	t := &Tree{Root: root}
	seen := map[string]bool{}
	var walk func(refs []Reference) error
	walk = func(refs []Reference) error {
		for _, r := range refs {
			if seen[r.Name] {
				continue
			}
			seen[r.Name] = true
			rs, err := c.SubjectVersion(ctx, r.Subject, fmt.Sprint(r.Version), "")
			if err != nil {
				return fmt.Errorf("reference %s (subject %s version %d): %w", r.Name, r.Subject, r.Version, err)
			}
			if err := walk(rs.References); err != nil {
				return err
			}
			t.Files = append(t.Files, TreeFile{Path: r.Name, Source: rs.Schema, Subject: r.Subject, Version: r.Version, ID: rs.ID})
		}
		return nil
	}
	if err := walk(s.References); err != nil {
		return nil, err
	}
	t.Files = append(t.Files, TreeFile{Path: root, Source: s.Schema, Subject: s.Subject, Version: s.Version, ID: s.ID})
	return t, nil
}

// Compile parses the tree and returns the root file. Imports outside
// the tree resolve to the protobuf well-known types.
//
// Stability: experimental
func (t *Tree) Compile(ctx context.Context) (protoreflect.FileDescriptor, error) {
	src := map[string]string{}
	for _, f := range t.Files {
		src[f.Path] = f.Source
	}
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
			s, ok := src[path]
			if !ok {
				return protocompile.SearchResult{}, os.ErrNotExist
			}
			if fdp := serialized(s); fdp != nil {
				fdp.Name = proto.String(path)
				return protocompile.SearchResult{Proto: fdp}, nil
			}
			return protocompile.SearchResult{Source: io.NopCloser(strings.NewReader(s))}, nil
		})),
	}
	res, err := c.Compile(ctx, t.Root)
	if err != nil {
		return nil, fmt.Errorf("compile registry schema: %w", err)
	}
	return res[0], nil
}

// serialized returns the FileDescriptorProto in s when s is base64 of
// one (format=serialized, or a registry that keeps what was
// registered), else nil.
func serialized(s string) *descriptorpb.FileDescriptorProto {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	fdp := &descriptorpb.FileDescriptorProto{}
	if proto.Unmarshal(b, fdp) != nil {
		return nil
	}
	return fdp
}

// FileByID returns the compiled file of schema id. Cached.
//
// Stability: experimental
func (c *Client) FileByID(ctx context.Context, id int) (protoreflect.FileDescriptor, error) {
	if v, ok := c.files.Load(id); ok {
		return v.(protoreflect.FileDescriptor), nil
	}
	s, err := c.SchemaByID(ctx, id, "")
	if err != nil {
		return nil, err
	}
	if s.SchemaType != "" && s.SchemaType != "PROTOBUF" {
		return nil, fmt.Errorf("schema id %d is %s, not PROTOBUF", id, s.SchemaType)
	}
	t, err := c.Tree(ctx, s, fmt.Sprintf("schema-%d.proto", id))
	if err != nil {
		return nil, err
	}
	fd, err := t.Compile(ctx)
	if err != nil {
		return nil, fmt.Errorf("schema id %d: %w", id, err)
	}
	c.files.Store(id, fd)
	return fd, nil
}

// SchemaOf returns fd as a registry schema: base64 of its
// FileDescriptorProto (without source info), the format Confluent's
// own serializers register. refs are its non-builtin imports, which
// the caller resolves.
//
// Stability: experimental
func SchemaOf(fd protoreflect.FileDescriptor, refs []Reference) (Schema, error) {
	fdp := protodesc.ToFileDescriptorProto(fd)
	canonical(fdp)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(fdp)
	if err != nil {
		return Schema{}, err
	}
	return Schema{Schema: base64.StdEncoding.EncodeToString(b), SchemaType: "PROTOBUF", References: refs}, nil
}

// canonical drops what differs between compilers of the same source,
// so a file pushed from .proto source (gwag schema push) and the same
// file in generated code (the Serde at publish) register as one
// schema: source info, and proto3_optional on extensions (protoc sets
// it, protocompile does not; an extension always has presence).
func canonical(fdp *descriptorpb.FileDescriptorProto) {
	fdp.SourceCodeInfo = nil
	clear := func(fs []*descriptorpb.FieldDescriptorProto) {
		for _, f := range fs {
			f.Proto3Optional = nil
		}
	}
	clear(fdp.Extension)
	var walk func(ms []*descriptorpb.DescriptorProto)
	walk = func(ms []*descriptorpb.DescriptorProto) {
		for _, m := range ms {
			clear(m.Extension)
			walk(m.NestedType)
		}
	}
	walk(fdp.MessageType)
}

// RegisterOptions controls EnsureFile.
//
// Stability: experimental
type RegisterOptions struct {
	// Register registers the file and its references where missing.
	// Without it every one must already be registered.
	Register bool
	// Normalize passes normalize=true.
	Normalize bool
	// RefSubject names the subject of an import. Default: the import
	// path, as Confluent's serializers do.
	RefSubject func(path string) string
}

// ErrNotRegistered wraps a lookup that found no registered schema.
//
// Stability: experimental
var ErrNotRegistered = errors.New("schema not registered")

type ensureKey struct {
	subject, path string
	register      bool
}

// EnsureFile finds (or, with Register, registers) fd under subject and
// each non-builtin import under its reference subject, imports first.
// It returns the registered root: Subject, Version, ID.
//
// Stability: experimental
func (c *Client) EnsureFile(ctx context.Context, subject string, fd protoreflect.FileDescriptor, o RegisterOptions) (*Schema, error) {
	if o.RefSubject == nil {
		o.RefSubject = func(p string) string { return p }
	}
	k := ensureKey{subject, fd.Path(), o.Register}
	if v, ok := c.ensured.Load(k); ok {
		return v.(*Schema), nil
	}
	var refs []Reference
	imps := fd.Imports()
	for i := 0; i < imps.Len(); i++ {
		dep := imps.Get(i).FileDescriptor
		if Builtin(dep.Path()) {
			continue
		}
		r, err := c.EnsureFile(ctx, o.RefSubject(dep.Path()), dep, o)
		if err != nil {
			return nil, err
		}
		refs = append(refs, Reference{Name: dep.Path(), Subject: r.Subject, Version: r.Version})
	}
	s, err := SchemaOf(fd, refs)
	if err != nil {
		return nil, err
	}
	if o.Register {
		if _, err := c.Register(ctx, subject, s, o.Normalize); err != nil {
			return nil, fmt.Errorf("register %s under %s: %w", fd.Path(), subject, err)
		}
	}
	found, err := c.Lookup(ctx, subject, s, o.Normalize)
	if err != nil {
		if IsNotFound(err) {
			return nil, fmt.Errorf("%s under subject %s: %w (%v)", fd.Path(), subject, ErrNotRegistered, err)
		}
		return nil, err
	}
	if found.Subject == "" {
		found.Subject = subject
	}
	c.ensured.Store(k, found)
	return found, nil
}
