package protocodec

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// testMessage builds `t.v1.Msg` with one field of each shape the codec
// branches on, plus a nested `t.v1.Inner`.
func testMessage(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	field := func(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, label *descriptorpb.FieldDescriptorProto_Label, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(name),
			Number: proto.Int32(num),
			Type:   typ.Enum(),
			Label:  label,
		}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("t.proto"),
		Package: proto.String("t.v1"),
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
				Name: proto.String("Inner"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("blob", 1, descriptorpb.FieldDescriptorProto_TYPE_BYTES, opt, ""),
				},
			},
			{
				Name: proto.String("Msg"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("user_id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, opt, ""),
					field("blob", 2, descriptorpb.FieldDescriptorProto_TYPE_BYTES, opt, ""),
					field("color", 3, descriptorpb.FieldDescriptorProto_TYPE_ENUM, opt, ".t.v1.Color"),
					field("tags", 4, descriptorpb.FieldDescriptorProto_TYPE_STRING, rep, ""),
					field("inner", 5, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, opt, ".t.v1.Inner"),
					field("count", 6, descriptorpb.FieldDescriptorProto_TYPE_UINT64, opt, ""),
					field("ratio", 7, descriptorpb.FieldDescriptorProto_TYPE_FLOAT, opt, ""),
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd.Messages().ByName("Msg")
}

type upload struct{}

var gwagOpts = Options{
	SkipArg:        func(v any) bool { _, ok := v.(*upload); return ok },
	BytesFromSlice: true,
}

func TestBytesFromSlice(t *testing.T) {
	md := testMessage(t)
	args := map[string]any{"blob": []byte("hi")}

	msg := dynamicpb.NewMessage(md)
	if err := gwagOpts.ArgsToMessage(args, msg); err != nil {
		t.Fatalf("BytesFromSlice: %v", err)
	}
	if got := string(msg.Get(md.Fields().ByName("blob")).Bytes()); got != "hi" {
		t.Fatalf("blob = %q", got)
	}

	err := Options{}.ArgsToMessage(args, dynamicpb.NewMessage(md))
	if err == nil || err.Error() != "field blob: expected string for bytes, got []uint8" {
		t.Fatalf("zero Options on []byte: %v", err)
	}
	err = gwagOpts.ArgsToMessage(map[string]any{"blob": 7}, dynamicpb.NewMessage(md))
	if err == nil || err.Error() != "field blob: expected string or []byte for bytes, got int" {
		t.Fatalf("BytesFromSlice on int: %v", err)
	}
	for _, o := range []Options{{}, gwagOpts} {
		msg := dynamicpb.NewMessage(md)
		if err := o.ArgsToMessage(map[string]any{"blob": "s"}, msg); err != nil {
			t.Fatalf("string into bytes: %v", err)
		}
	}
}

func TestSkipArg(t *testing.T) {
	md := testMessage(t)
	args := map[string]any{
		"blob":  &upload{},
		"inner": map[string]any{"blob": &upload{}},
	}
	msg := dynamicpb.NewMessage(md)
	if err := gwagOpts.ArgsToMessage(args, msg); err != nil {
		t.Fatalf("SkipArg: %v", err)
	}
	if msg.Has(md.Fields().ByName("blob")) {
		t.Fatal("skipped arg was written")
	}
	inner := msg.Get(md.Fields().ByName("inner")).Message()
	if inner.Has(inner.Descriptor().Fields().ByName("blob")) {
		t.Fatal("SkipArg not applied to nested message")
	}

	err := Options{}.ArgsToMessage(args, dynamicpb.NewMessage(md))
	if err == nil || !strings.Contains(err.Error(), "expected string for bytes") {
		t.Fatalf("zero Options must reject the value SkipArg would skip: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	md := testMessage(t)
	args := map[string]any{
		"userId":  "9007199254740993",
		"color":   "COLOR_RED",
		"tags":    []any{"a", "b"},
		"inner":   map[string]any{"blob": "x"},
		"count":   "9223372036854775807",
		"ratio":   0.5,
		"unknown": 1,
	}
	for name, o := range map[string]Options{"gat": {}, "gwag": gwagOpts} {
		msg := dynamicpb.NewMessage(md)
		if err := o.ArgsToMessage(args, msg); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := MessageToMap(msg)
		want := map[string]any{
			"userId": "9007199254740993",
			"color":  int32(1),
			"count":  "9223372036854775807",
			"ratio":  float64(0.5),
			"inner":  map[string]any{"blob": "x"},
		}
		for k, w := range want {
			if k == "inner" {
				if got := out[k].(map[string]any)["blob"]; got != "x" {
					t.Fatalf("%s: inner.blob = %#v", name, got)
				}
				continue
			}
			if out[k] != w {
				t.Fatalf("%s: %s = %#v (%T), want %#v", name, k, out[k], out[k], w)
			}
		}
		tags, _ := out["tags"].([]any)
		if len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
			t.Fatalf("%s: tags = %#v", name, out["tags"])
		}
		if _, ok := out["blob"]; ok {
			t.Fatalf("%s: unset field emitted", name)
		}
	}
}

func TestMessageToMapNil(t *testing.T) {
	if m := MessageToMap(nil); m == nil || len(m) != 0 {
		t.Fatalf("MessageToMap(nil) = %#v", m)
	}
}

func TestFieldInfosCachedPerDescriptor(t *testing.T) {
	a, b := testMessage(t), testMessage(t)
	ia, ib := FieldInfos(a), FieldInfos(b)
	if ia[0].FD == ib[0].FD {
		t.Fatal("distinct descriptors with one FullName shared a cache entry")
	}
	if &FieldInfos(a)[0] != &ia[0] {
		t.Fatal("FieldInfos not memoised")
	}
	if ia[0].JSONKey != "userId" {
		t.Fatalf("JSONKey = %q", ia[0].JSONKey)
	}
}

func TestLowerCamel(t *testing.T) {
	for in, want := range map[string]string{
		"":          "",
		"user_id":   "userId",
		"UserName":  "userName",
		"a__b":      "aB",
		"trailing_": "trailing",
	} {
		if got := LowerCamel(in); got != want {
			t.Errorf("LowerCamel(%q) = %q, want %q", in, got, want)
		}
	}
}
