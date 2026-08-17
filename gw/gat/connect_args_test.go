package gat

// messageToArgs replaced a protojson.Marshal + json.Unmarshal round
// trip. The replacement is only safe if it produces the same map the
// round trip did — bindInput matches these keys against IR arg names
// and silently skips what it can't map, so any divergence surfaces as
// a handler reading a zero value, not as an error.
//
// These tests keep protojson as the oracle: build a message, run both
// paths, require deep equality. A protojson upgrade that changes an
// encoding will fail here rather than in production.

import (
	"encoding/json"
	"reflect"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// viaProtojson is the path messageToArgs replaced, kept verbatim as
// the reference implementation.
func viaProtojson(t *testing.T, msg *dynamicpb.Message) map[string]any {
	t.Helper()
	b, err := protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: false,
	}.Marshal(msg)
	if err != nil {
		t.Fatalf("protojson marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// argsTestFileDesc builds a descriptor exercising every kind the
// walker branches on, including a snake_case field name — the case
// where the GraphQL-canonical protoMessageToMap would have diverged
// (it lowerCamels the key, so `user_id` would arrive as `userId` and
// bindInput would drop it).
func argsTestFileDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("argstest.proto"),
		Package: proto.String("argstest.v1"),
		Syntax:  proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Color"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("COLOR_UNSPECIFIED"), Number: proto.Int32(0)},
				{Name: proto.String("COLOR_RED"), Number: proto.Int32(1)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Nested"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("note", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, false),
				},
			},
			{
				Name: proto.String("Req"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("name", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, false),
					field("user_id", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, false),
					field("limit", 3, descriptorpb.FieldDescriptorProto_TYPE_INT32, false),
					field("big", 4, descriptorpb.FieldDescriptorProto_TYPE_INT64, false),
					field("ubig", 5, descriptorpb.FieldDescriptorProto_TYPE_UINT64, false),
					field("ratio", 6, descriptorpb.FieldDescriptorProto_TYPE_DOUBLE, false),
					field("flag", 7, descriptorpb.FieldDescriptorProto_TYPE_BOOL, false),
					field("blob", 8, descriptorpb.FieldDescriptorProto_TYPE_BYTES, false),
					field("tags", 9, descriptorpb.FieldDescriptorProto_TYPE_STRING, true),
					enumField("color", 10, ".argstest.v1.Color"),
					msgField("nested", 11, ".argstest.v1.Nested", false),
					msgField("children", 12, ".argstest.v1.Nested", true),
				},
			},
		},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fd}})
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	d, err := files.FindDescriptorByName("argstest.v1.Req")
	if err != nil {
		t.Fatalf("FindDescriptorByName: %v", err)
	}
	return d.(protoreflect.MessageDescriptor)
}

func field(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, repeated bool) *descriptorpb.FieldDescriptorProto {
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	if repeated {
		label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	}
	return &descriptorpb.FieldDescriptorProto{
		Name: proto.String(name), Number: proto.Int32(num),
		Type: typ.Enum(), Label: label.Enum(),
	}
}

func enumField(name string, num int32, typeName string) *descriptorpb.FieldDescriptorProto {
	f := field(name, num, descriptorpb.FieldDescriptorProto_TYPE_ENUM, false)
	f.TypeName = proto.String(typeName)
	return f
}

func msgField(name string, num int32, typeName string, repeated bool) *descriptorpb.FieldDescriptorProto {
	f := field(name, num, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, repeated)
	f.TypeName = proto.String(typeName)
	return f
}

func set(msg *dynamicpb.Message, name string, v protoreflect.Value) {
	msg.Set(msg.Descriptor().Fields().ByName(protoreflect.Name(name)), v)
}

func TestMessageToArgs_MatchesProtojson_AllKinds(t *testing.T) {
	md := argsTestFileDesc(t)
	msg := dynamicpb.NewMessage(md)

	set(msg, "name", protoreflect.ValueOfString("alice"))
	set(msg, "user_id", protoreflect.ValueOfString("u-42"))
	set(msg, "limit", protoreflect.ValueOfInt32(25))
	set(msg, "big", protoreflect.ValueOfInt64(9007199254740993)) // past 2^53
	set(msg, "ubig", protoreflect.ValueOfUint64(18446744073709551615))
	set(msg, "ratio", protoreflect.ValueOfFloat64(0.5))
	set(msg, "flag", protoreflect.ValueOfBool(true))
	set(msg, "blob", protoreflect.ValueOfBytes([]byte{0x01, 0x02, 0xff}))
	set(msg, "color", protoreflect.ValueOfEnum(1))

	tags := msg.Mutable(md.Fields().ByName("tags")).List()
	tags.Append(protoreflect.ValueOfString("core"))
	tags.Append(protoreflect.ValueOfString("beta"))

	nested := msg.Mutable(md.Fields().ByName("nested")).Message()
	nested.Set(nested.Descriptor().Fields().ByName("note"), protoreflect.ValueOfString("hi"))

	children := msg.Mutable(md.Fields().ByName("children")).List()
	child := children.NewElement()
	child.Message().Set(child.Message().Descriptor().Fields().ByName("note"), protoreflect.ValueOfString("kid"))
	children.Append(child)

	want := viaProtojson(t, msg)
	got := messageToArgs(msg)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("messageToArgs diverges from protojson\ngot:  %#v\nwant: %#v", got, want)
	}
	// Guard the specific key the GraphQL-canonical walker would have
	// mangled — this is the silent-drop case, so assert it directly.
	if _, ok := got["user_id"]; !ok {
		t.Errorf("snake_case key user_id missing; got keys %v", keysOf(got))
	}
}

func TestMessageToArgs_OmitsUnpopulated(t *testing.T) {
	md := argsTestFileDesc(t)
	msg := dynamicpb.NewMessage(md)
	set(msg, "name", protoreflect.ValueOfString("only"))

	want := viaProtojson(t, msg)
	got := messageToArgs(msg)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sparse message diverges\ngot:  %#v\nwant: %#v", got, want)
	}
	if len(got) != 1 {
		t.Errorf("expected only the populated field, got %v", keysOf(got))
	}
}

func TestMessageToArgs_EmptyAndNil(t *testing.T) {
	md := argsTestFileDesc(t)

	if got := messageToArgs(nil); len(got) != 0 {
		t.Errorf("nil message → %v; want empty map", got)
	}
	empty := dynamicpb.NewMessage(md)
	if got, want := messageToArgs(empty), viaProtojson(t, empty); !reflect.DeepEqual(got, want) {
		t.Errorf("empty message diverges\ngot:  %#v\nwant: %#v", got, want)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
