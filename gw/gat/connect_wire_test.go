package gat

// appendMessage hand-writes proto wire format. Wire bugs are the quiet
// kind — output that still parses but means something else — so every
// case here is checked against proto.Marshal over the equivalent
// dynamicpb message rather than reasoned about.
//
// Two assertions per case, deliberately:
//
//   - the bytes match, which catches field ordering, packing and
//     presence decisions; and
//   - re-parsing both into fresh messages yields proto.Equal, which is
//     the property that actually matters if the byte check ever has to
//     be relaxed.

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

type wireChild struct {
	Note string `json:"note"`
	Size int32  `json:"size"`
}

type wireAll struct {
	Str      string      `json:"str"`
	I32      int32       `json:"i32"`
	I64      int64       `json:"i64"`
	U32      uint32      `json:"u32"`
	U64      uint64      `json:"u64"`
	F32      float32     `json:"f32"`
	F64      float64     `json:"f64"`
	Flag     bool        `json:"flag"`
	Blob     []byte      `json:"blob"`
	Nums     []int32     `json:"nums"` // packed
	Strs     []string    `json:"strs"` // not packed
	Nested   wireChild   `json:"nested"`
	Children []wireChild `json:"children"`
}

// wireDesc builds a message covering every branch appendField takes.
func wireDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("wire.proto"),
		Package: proto.String("wiretest.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Child"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("note", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, false),
					field("size", 2, descriptorpb.FieldDescriptorProto_TYPE_INT32, false),
				},
			},
			{
				Name: proto.String("All"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("str", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, false),
					field("i32", 2, descriptorpb.FieldDescriptorProto_TYPE_INT32, false),
					field("i64", 3, descriptorpb.FieldDescriptorProto_TYPE_INT64, false),
					field("u32", 4, descriptorpb.FieldDescriptorProto_TYPE_UINT32, false),
					field("u64", 5, descriptorpb.FieldDescriptorProto_TYPE_UINT64, false),
					field("f32", 6, descriptorpb.FieldDescriptorProto_TYPE_FLOAT, false),
					field("f64", 7, descriptorpb.FieldDescriptorProto_TYPE_DOUBLE, false),
					field("flag", 8, descriptorpb.FieldDescriptorProto_TYPE_BOOL, false),
					field("blob", 9, descriptorpb.FieldDescriptorProto_TYPE_BYTES, false),
					field("nums", 10, descriptorpb.FieldDescriptorProto_TYPE_INT32, true),
					field("strs", 11, descriptorpb.FieldDescriptorProto_TYPE_STRING, true),
					msgField("nested", 12, ".wiretest.v1.Child", false),
					msgField("children", 13, ".wiretest.v1.Child", true),
				},
			},
		},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{fdp},
	})
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	d, err := files.FindDescriptorByName("wiretest.v1.All")
	if err != nil {
		t.Fatalf("find All: %v", err)
	}
	return d.(protoreflect.MessageDescriptor)
}

// bothEncodings returns the emitter's bytes and proto.Marshal's bytes
// for the same value.
func bothEncodings(t *testing.T, md protoreflect.MessageDescriptor, body any) (direct, viaProto []byte) {
	t.Helper()
	plan := buildEncodePlan(derefType(reflect.TypeOf(body)), md)
	if plan == nil {
		t.Fatalf("expected an encode plan for %T", body)
	}
	plan.sortFields()

	direct, err := appendMessage(nil, plan, reflect.ValueOf(body))
	if err != nil {
		t.Fatalf("appendMessage: %v", err)
	}

	msg := dynamicpb.NewMessage(md)
	if err := plan.encode(reflect.ValueOf(body), msg); err != nil {
		t.Fatalf("plan.encode: %v", err)
	}
	viaProto, err = proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	return direct, viaProto
}

func assertSameWire(t *testing.T, md protoreflect.MessageDescriptor, body any) {
	t.Helper()
	direct, viaProto := bothEncodings(t, md, body)

	if string(direct) != string(viaProto) {
		t.Errorf("wire bytes differ\ndirect:   %x\nviaProto: %x", direct, viaProto)
	}

	// Semantic check, independent of byte layout.
	a, b := dynamicpb.NewMessage(md), dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(direct, a); err != nil {
		t.Fatalf("emitted bytes do not parse: %v", err)
	}
	if err := proto.Unmarshal(viaProto, b); err != nil {
		t.Fatalf("reference bytes do not parse: %v", err)
	}
	if !proto.Equal(a, b) {
		t.Errorf("decoded messages differ\ndirect:   %v\nviaProto: %v", a, b)
	}
}

func TestAppendMessage_MatchesProtoMarshal_AllKinds(t *testing.T) {
	md := wireDesc(t)
	assertSameWire(t, md, wireAll{
		Str:    "hello",
		I32:    -7,
		I64:    9007199254740993, // past 2^53
		U32:    4294967295,
		U64:    18446744073709551615,
		F32:    0.5,
		F64:    -1.25,
		Flag:   true,
		Blob:   []byte{0x01, 0x02, 0xff},
		Nums:   []int32{1, 2, 300, -4},
		Strs:   []string{"a", "bb"},
		Nested: wireChild{Note: "inner", Size: 3},
		Children: []wireChild{
			{Note: "one", Size: 1},
			{Note: "two"},
		},
	})
}

// proto3 implicit presence: a zero scalar must be omitted entirely.
// Emitting it would be valid wire format that decodes the same, but it
// would diverge from proto.Marshal's bytes and inflate every response.
func TestAppendMessage_OmitsZeroValues(t *testing.T) {
	md := wireDesc(t)
	assertSameWire(t, md, wireAll{Str: "only"})

	direct, _ := bothEncodings(t, md, wireAll{Str: "only"})
	// tag + len + "only" = 6, then the zero nested message's own empty
	// record (tag + len 0) = 2. A zero SCALAR leaking in would push it
	// past 8.
	if len(direct) != 8 {
		t.Errorf("expected 8 bytes, got %d (%x)", len(direct), direct)
	}
}

func TestAppendMessage_Empty(t *testing.T) {
	md := wireDesc(t)
	assertSameWire(t, md, wireAll{})

	// Not zero bytes: the zero nested message still occupies an empty
	// record, because message fields have explicit presence.
	direct, _ := bothEncodings(t, md, wireAll{})
	if len(direct) != 2 {
		t.Errorf("expected only the empty nested record (2 bytes), got %d (%x)", len(direct), direct)
	}
}

// Packing is the rule most likely to be got wrong silently: repeated
// numerics share one length-delimited run, repeated strings do not.
func TestAppendMessage_PackedVsUnpacked(t *testing.T) {
	md := wireDesc(t)

	t.Run("packed numerics", func(t *testing.T) {
		assertSameWire(t, md, wireAll{Nums: []int32{1, 2, 3, 4, 5}})
	})
	t.Run("unpacked strings", func(t *testing.T) {
		assertSameWire(t, md, wireAll{Strs: []string{"a", "b", "c"}})
	})
	t.Run("empty slices are absent", func(t *testing.T) {
		assertSameWire(t, md, wireAll{Nums: []int32{}, Strs: []string{}})
	})
	t.Run("single element", func(t *testing.T) {
		assertSameWire(t, md, wireAll{Nums: []int32{42}, Strs: []string{"x"}})
	})
}

func TestAppendMessage_NestedAndRepeatedMessages(t *testing.T) {
	md := wireDesc(t)

	t.Run("nested only", func(t *testing.T) {
		assertSameWire(t, md, wireAll{Nested: wireChild{Note: "n", Size: 9}})
	})
	t.Run("repeated only", func(t *testing.T) {
		assertSameWire(t, md, wireAll{Children: []wireChild{{Note: "a"}, {Size: 2}, {}}})
	})
	t.Run("zero nested is present but empty", func(t *testing.T) {
		assertSameWire(t, md, wireAll{Str: "x", Nested: wireChild{}})
	})
}

// Negative 32-bit values are sign-extended to ten-byte varints by the
// reference encoder; a naive uint32 conversion would emit five and
// decode as a huge positive number.
func TestAppendMessage_NegativeInt32(t *testing.T) {
	md := wireDesc(t)
	for _, n := range []int32{-1, -128, -2147483648} {
		assertSameWire(t, md, wireAll{I32: n})
	}
}

func TestAppendMessage_LargeList(t *testing.T) {
	md := wireDesc(t)
	kids := make([]wireChild, 25)
	nums := make([]int32, 25)
	for i := range kids {
		kids[i] = wireChild{Note: "child", Size: int32(i)}
		nums[i] = int32(i * 1000)
	}
	assertSameWire(t, md, wireAll{Children: kids, Nums: nums})
}
