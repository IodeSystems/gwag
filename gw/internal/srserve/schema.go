package srserve

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoprint"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
)

// Schema types.
const (
	TypeProtobuf = "PROTOBUF"
	TypeAvro     = "AVRO"
	TypeJSON     = "JSON"
)

// schemaRec is one schema id.
type schemaRec struct {
	ID   int    `json:"id"`
	Type string `json:"schemaType"`
	// Schema is the default format: .proto text for PROTOBUF.
	Schema string `json:"schema"`
	// Serialized is base64 of the FileDescriptorProto (PROTOBUF only):
	// what ?format=serialized returns.
	Serialized string         `json:"serialized,omitempty"`
	References []sr.Reference `json:"references,omitempty"`
	// Keys identify the schema for lookups and for deduplication
	// (keyOf). An imported schema has one per form it came in.
	Keys     []string `json:"keys"`
	Imported bool     `json:"imported,omitempty"`
}

// resolver returns the schema registered as version of subject
// (version -1: the latest).
type resolver func(ctx context.Context, subject string, version int) (*schemaRec, error)

// schemaType applies Confluent's default: no schemaType is AVRO.
func schemaType(t string) string {
	if t == "" {
		return TypeAvro
	}
	return strings.ToUpper(t)
}

// rootPath is the path a schema's own file compiles under; it must
// not collide with a reference name.
const rootPath = "gwag-sr-root.proto"

// compile compiles a PROTOBUF schema (.proto text or base64 of a
// FileDescriptorProto) with its references.
func compile(ctx context.Context, schema string, refs []sr.Reference, resolve resolver) (protoreflect.FileDescriptor, error) {
	t := &sr.Tree{Root: rootPath}
	seen := map[string]bool{}
	var walk func(refs []sr.Reference, depth int) error
	walk = func(refs []sr.Reference, depth int) error {
		if depth > 32 {
			return fmt.Errorf("references nest deeper than 32")
		}
		for _, r := range refs {
			if seen[r.Name] {
				continue
			}
			seen[r.Name] = true
			rec, err := resolve(ctx, r.Subject, r.Version)
			if err != nil {
				return fmt.Errorf("reference %s (subject %s version %d): %w", r.Name, r.Subject, r.Version, err)
			}
			if err := walk(rec.References, depth+1); err != nil {
				return err
			}
			src := rec.Serialized
			if src == "" {
				src = rec.Schema
			}
			t.Files = append(t.Files, sr.TreeFile{Path: r.Name, Source: src})
		}
		return nil
	}
	if err := walk(refs, 0); err != nil {
		return nil, err
	}
	t.Files = append(t.Files, sr.TreeFile{Path: rootPath, Source: schema})
	return t.Compile(ctx)
}

// keyOf identifies a compiled schema up to what does not change the
// wire or the declarations: file name, source info, json_name and
// proto3_optional on extensions (compilers differ on those). The
// references are part of the key, as Confluent compares them too.
func keyOf(fd protoreflect.FileDescriptor, refs []sr.Reference) (string, error) {
	fdp := protodesc.ToFileDescriptorProto(fd)
	fdp.Name = nil
	fdp.SourceCodeInfo = nil
	var fields func([]*descriptorpb.FieldDescriptorProto, bool)
	fields = func(fs []*descriptorpb.FieldDescriptorProto, ext bool) {
		for _, f := range fs {
			f.JsonName = nil
			if ext {
				f.Proto3Optional = nil
			}
		}
	}
	var msgs func([]*descriptorpb.DescriptorProto)
	msgs = func(ms []*descriptorpb.DescriptorProto) {
		for _, m := range ms {
			fields(m.Field, false)
			fields(m.Extension, true)
			msgs(m.NestedType)
		}
	}
	fields(fdp.Extension, true)
	msgs(fdp.MessageType)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(fdp)
	if err != nil {
		return "", err
	}
	return rawKey(TypeProtobuf, string(b), refs), nil
}

// rawKey identifies a schema by its exact text (types gwag does not
// parse, and schemas that do not compile).
func rawKey(typ, schema string, refs []sr.Reference) string {
	h := sha256.New()
	h.Write([]byte(typ))
	h.Write([]byte{0})
	h.Write([]byte(schema))
	h.Write([]byte{0})
	rb, _ := json.Marshal(normRefs(refs))
	h.Write(rb)
	return hex.EncodeToString(h.Sum(nil))
}

func normRefs(refs []sr.Reference) []sr.Reference {
	if len(refs) == 0 {
		return nil
	}
	return refs
}

// isSerialized reports whether s is base64 of a FileDescriptorProto.
func isSerialized(s string) bool {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return false
	}
	return proto.Unmarshal(b, &descriptorpb.FileDescriptorProto{}) == nil
}

// printProto renders fd as .proto text.
func printProto(fd protoreflect.FileDescriptor) (string, error) {
	d, err := desc.WrapFile(fd)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := (&protoprint.Printer{}).PrintProtoFile(d, &b); err != nil {
		return "", err
	}
	return b.String(), nil
}

// serialize is base64 of fd's FileDescriptorProto without source info.
func serialize(fd protoreflect.FileDescriptor) (string, error) {
	fdp := protodesc.ToFileDescriptorProto(fd)
	fdp.SourceCodeInfo = nil
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(fdp)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// prepare turns a schema a client sent into a record (no ID): for
// PROTOBUF it compiles the schema, keeps what was sent and derives
// the other format. A PROTOBUF schema that does not compile is an
// error.
func prepare(ctx context.Context, in sr.Schema, resolve resolver) (*schemaRec, error) {
	rec := &schemaRec{Type: schemaType(in.SchemaType), References: normRefs(in.References)}
	if rec.Type != TypeProtobuf {
		rec.Schema = in.Schema
		rec.Keys = []string{rawKey(rec.Type, in.Schema, rec.References)}
		return rec, nil
	}
	fd, err := compile(ctx, in.Schema, rec.References, resolve)
	if err != nil {
		return nil, err
	}
	k, err := keyOf(fd, rec.References)
	if err != nil {
		return nil, err
	}
	rec.Keys = []string{k}
	if isSerialized(in.Schema) {
		rec.Serialized = strings.TrimSpace(in.Schema)
		if rec.Schema, err = printProto(fd); err != nil {
			return nil, err
		}
	} else {
		rec.Schema = in.Schema
		if rec.Serialized, err = serialize(fd); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

func sameRefs(a, b []sr.Reference) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// matches reports whether a and b are the same schema: same type and
// references, and a shared key or identical text.
func matches(a, b *schemaRec) bool {
	if a.Type != b.Type || !sameRefs(a.References, b.References) {
		return false
	}
	if a.Schema == b.Schema || (a.Serialized != "" && a.Serialized == b.Serialized) {
		return true
	}
	for _, x := range a.Keys {
		for _, y := range b.Keys {
			if x == y {
				return true
			}
		}
	}
	return false
}
