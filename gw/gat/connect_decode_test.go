package gat

// The wire decoder replaced proto.Unmarshal into dynamicpb + bindPlan
// on the binary request path. Same contract as the other walkers: it
// must leave the input struct exactly as the old path did, and reject
// exactly what the old path rejected.
//
// So every case runs both and compares. The hand-written cases pin the
// rules the decoder owns by name (presence, last-one-wins, merge,
// packed/unpacked); the generated ones throw random record sequences —
// duplicates, wrong wire types, unknown fields, zeros, bad UTF-8 — and
// random corruptions of them at both decoders, since a wire rule
// nobody thought to write a case for is exactly the kind that breaks.

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"net/http"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

type decChild struct {
	Note string   `json:"note"`
	Size int32    `json:"size"`
	Tags []string `json:"tags"`
}

type decPayload struct {
	I32   int32   `json:"i32"`
	I64   int64   `json:"i64"`
	U32   uint32  `json:"u32"`
	U64   uint64  `json:"u64"`
	S32   int32   `json:"s32"`
	S64   int64   `json:"s64"`
	Fx32  uint32  `json:"fx32"`
	Fx64  uint64  `json:"fx64"`
	SFx32 int32   `json:"sfx32"`
	SFx64 int64   `json:"sfx64"`
	Flt   float32 `json:"flt"`
	Dbl   float64 `json:"dbl"`
	Flag  bool    `json:"flag"`
	Str   string  `json:"str"`
	Blob  []byte  `json:"blob"`
	Wide  int64   `json:"wide"` // proto int32: widening is lossless

	PI32 *int32  `json:"p_i32"` // implicit presence into a pointer
	Opt  *string `json:"opt"`   // proto3 optional
	OptN int64   `json:"opt_n"` // proto3 optional, non-pointer

	Nums  []int32   `json:"nums"`
	Sints []int64   `json:"sints"`
	Fxs   []uint32  `json:"fxs"`
	Dbls  []float64 `json:"dbls"`
	Flags []bool    `json:"flags"`
	Strs  []string  `json:"strs"`
	Blobs [][]byte  `json:"blobs"`
	PNums []*int64  `json:"p_nums"`

	Child     decChild    `json:"child"`
	PChild    *decChild   `json:"p_child"`
	Children  []decChild  `json:"children"`
	PChildren []*decChild `json:"p_children"`
}

type decInput struct {
	Limit int64    `query:"limit"`
	Name  *string  `query:"name"`
	IDs   []string `query:"ids"`
	Body  decPayload
}

var decOp = irOp(qArg("limit"), qArg("name"), qArg("ids"), bArg("body"))

func pfield(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, packed bool) *descriptorpb.FieldDescriptorProto {
	f := field(name, num, typ, true)
	f.Options = &descriptorpb.FieldOptions{Packed: proto.Bool(packed)}
	return f
}

func optField(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, oneof int32) *descriptorpb.FieldDescriptorProto {
	f := field(name, num, typ, false)
	f.Proto3Optional = proto.Bool(true)
	f.OneofIndex = proto.Int32(oneof)
	return f
}

// decDesc returns the request message. Fields with no Go counterpart
// (enum, map, unbound message, recursive node) are there to be parsed
// and dropped, with the same errors.
func decDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	ty := func(s string) descriptorpb.FieldDescriptorProto_Type {
		return descriptorpb.FieldDescriptorProto_Type(descriptorpb.FieldDescriptorProto_Type_value["TYPE_"+s])
	}
	labels := msgField("labels", 40, ".dectest.v1.Payload.LabelsEntry", true)
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("dec.proto"),
		Package: proto.String("dectest.v1"),
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
				Name: proto.String("Child"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("note", 1, ty("STRING"), false),
					field("size", 2, ty("INT32"), false),
					field("tags", 3, ty("STRING"), true),
					field("extra", 4, ty("STRING"), false), // no Go field
				},
			},
			{
				Name:  proto.String("Node"),
				Field: []*descriptorpb.FieldDescriptorProto{msgField("next", 1, ".dectest.v1.Node", false), field("v", 2, ty("STRING"), false)},
			},
			{
				Name: proto.String("Payload"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("i32", 1, ty("INT32"), false),
					field("i64", 2, ty("INT64"), false),
					field("u32", 3, ty("UINT32"), false),
					field("u64", 4, ty("UINT64"), false),
					field("s32", 5, ty("SINT32"), false),
					field("s64", 6, ty("SINT64"), false),
					field("fx32", 7, ty("FIXED32"), false),
					field("fx64", 8, ty("FIXED64"), false),
					field("sfx32", 9, ty("SFIXED32"), false),
					field("sfx64", 10, ty("SFIXED64"), false),
					field("flt", 11, ty("FLOAT"), false),
					field("dbl", 12, ty("DOUBLE"), false),
					field("flag", 13, ty("BOOL"), false),
					field("str", 14, ty("STRING"), false),
					field("blob", 15, ty("BYTES"), false),
					field("wide", 16, ty("INT32"), false),
					field("p_i32", 17, ty("INT32"), false),
					optField("opt", 18, ty("STRING"), 0),
					optField("opt_n", 19, ty("INT64"), 1),
					field("nums", 20, ty("INT32"), true),
					pfield("sints", 21, ty("SINT64"), false), // unpacked by declaration
					field("fxs", 22, ty("FIXED32"), true),
					field("dbls", 23, ty("DOUBLE"), true),
					field("flags", 24, ty("BOOL"), true),
					field("strs", 25, ty("STRING"), true),
					field("blobs", 26, ty("BYTES"), true),
					field("p_nums", 27, ty("INT64"), true),
					msgField("child", 28, ".dectest.v1.Child", false),
					msgField("p_child", 29, ".dectest.v1.Child", false),
					msgField("children", 30, ".dectest.v1.Child", true),
					msgField("p_children", 31, ".dectest.v1.Child", true),
					enumField("color", 41, ".dectest.v1.Color"),
					func() *descriptorpb.FieldDescriptorProto {
						f := enumField("colors", 42, ".dectest.v1.Color")
						f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
						return f
					}(),
					labels,
					msgField("loose", 43, ".dectest.v1.Child", false),
					msgField("node", 44, ".dectest.v1.Node", false),
				},
				OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("_opt")}, {Name: proto.String("_opt_n")}},
				NestedType: []*descriptorpb.DescriptorProto{{
					Name:    proto.String("LabelsEntry"),
					Field:   []*descriptorpb.FieldDescriptorProto{field("key", 1, ty("STRING"), false), field("value", 2, ty("STRING"), false)},
					Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
				}},
			},
			{
				Name: proto.String("Req"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("limit", 1, ty("INT64"), false),
					field("name", 2, ty("STRING"), false),
					field("ids", 3, ty("STRING"), true),
					msgField("body", 4, ".dectest.v1.Payload", false),
					field("ignored", 5, ty("INT64"), false), // no IR arg
				},
			},
		},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}})
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	d, err := files.FindDescriptorByName("dectest.v1.Req")
	if err != nil {
		t.Fatalf("find Req: %v", err)
	}
	return d.(protoreflect.MessageDescriptor)
}

// bothDecodes runs wire through the decoder and through
// proto.Unmarshal + bindPlan.bind.
func bothDecodes(t *testing.T, typ reflect.Type, op *ir.Operation, md protoreflect.MessageDescriptor, wire []byte) (fast, slow any, fastErr, slowErr error) {
	t.Helper()
	dec := newRequestDecoder(typ, op, md)
	if dec == nil {
		t.Fatalf("expected a decode plan for %s", typ)
	}
	fast, fastErr = dec.decode(wire)

	msg := dynamicpb.NewMessage(md)
	slowErr = unmarshalNoPanic(wire, msg)
	if slowErr == nil {
		bp := buildBindPlan(typ, op, bodyArgOf(op), md)
		in := reflect.New(typ)
		if err := bp.bind(in.Elem(), msg); err != nil {
			t.Fatalf("bind: %v", err)
		}
		slow = in.Interface()
	}
	return fast, slow, fastErr, slowErr
}

// errSlowPanic marks input proto.Unmarshal panics on rather than
// rejecting. protobuf-go's map-entry decoding (the path dynamicpb
// takes) does this when an entry repeats its key or value field with a
// wrong wire type: the bad occurrence overwrites the decoded value
// with an invalid one, which then panics on insert. There is nothing
// to compare against there; the decoder only has to not panic.
var errSlowPanic = errors.New("proto.Unmarshal panicked")

func unmarshalNoPanic(wire []byte, msg *dynamicpb.Message) (err error) {
	defer func() {
		if recover() != nil {
			err = errSlowPanic
		}
	}()
	return proto.UnmarshalOptions{AllowPartial: true}.Unmarshal(wire, msg)
}

// assertSameDecode compares the two paths on wire and reports which
// outcome both reached: "ok", "error", or "panic" (old path only).
func assertSameDecode(t *testing.T, md protoreflect.MessageDescriptor, wire []byte) string {
	t.Helper()
	fast, slow, fastErr, slowErr := bothDecodes(t, reflect.TypeFor[decInput](), decOp, md, wire)
	if slowErr == errSlowPanic {
		return "panic"
	}
	if (fastErr != nil) != (slowErr != nil) {
		t.Fatalf("error mismatch on %x\nfast: %v\nslow: %v", wire, fastErr, slowErr)
	}
	if slowErr != nil {
		return "error"
	}
	if !sameDecoded(reflect.ValueOf(fast), reflect.ValueOf(slow)) {
		t.Fatalf("decodes differ on %x\nfast: %+v\nslow: %+v", wire, fast, slow)
	}
	return "ok"
}

// sameDecoded is reflect.DeepEqual with floats compared by bit
// pattern: NaN equals itself, and -0 does not equal 0.
func sameDecoded(a, b reflect.Value) bool {
	if a.Kind() != b.Kind() || a.Type() != b.Type() {
		return false
	}
	switch a.Kind() {
	case reflect.Float32, reflect.Float64:
		return math.Float64bits(a.Float()) == math.Float64bits(b.Float())
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return sameDecoded(a.Elem(), b.Elem())
	case reflect.Slice:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if !sameDecoded(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if !sameDecoded(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

// --- wire building helpers ---------------------------------------------

type rec []byte

func vrec(num protowire.Number, v uint64) rec {
	return protowire.AppendVarint(protowire.AppendTag(nil, num, protowire.VarintType), v)
}
func f32rec(num protowire.Number, v uint32) rec {
	return protowire.AppendFixed32(protowire.AppendTag(nil, num, protowire.Fixed32Type), v)
}
func f64rec(num protowire.Number, v uint64) rec {
	return protowire.AppendFixed64(protowire.AppendTag(nil, num, protowire.Fixed64Type), v)
}
func brec(num protowire.Number, b []byte) rec {
	return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), b)
}
func cat(rs ...rec) []byte {
	var out []byte
	for _, r := range rs {
		out = append(out, r...)
	}
	return out
}
func body(rs ...rec) rec { return brec(4, cat(rs...)) }

// --- hand-written cases --------------------------------------------------

func TestDecodePlan_MatchesUnmarshalBind(t *testing.T) {
	md := decDesc(t)
	neg := func(i int64) uint64 { return uint64(i) }
	packed := func(vs ...uint64) []byte {
		var b []byte
		for _, v := range vs {
			b = protowire.AppendVarint(b, v)
		}
		return b
	}
	child := func(rs ...rec) []byte { return cat(rs...) }

	cases := map[string][]byte{
		"empty":  nil,
		"params": cat(vrec(1, 9007199254740993), brec(2, []byte("bob")), brec(3, []byte("a")), brec(3, []byte("b"))),
		"every scalar": cat(body(
			vrec(1, neg(-5)), vrec(2, neg(-1<<40)), vrec(3, math.MaxUint32), vrec(4, math.MaxUint64),
			vrec(5, protowire.EncodeZigZag(-7)), vrec(6, protowire.EncodeZigZag(math.MinInt64)),
			f32rec(7, 0xdeadbeef), f64rec(8, math.MaxUint64), f32rec(9, uint32(0xfffffffe)), f64rec(10, neg(-3)),
			f32rec(11, math.Float32bits(1.5)), f64rec(12, math.Float64bits(-2.25)), vrec(13, 1),
			brec(14, []byte("héllo")), brec(15, []byte{0, 1, 2}), vrec(16, neg(math.MinInt32)),
		)),
		"int32 varint truncates": cat(body(vrec(1, 1<<32|7), vrec(3, 1<<32), vrec(16, 1<<33))),
		"out of order":           cat(body(vrec(13, 1), vrec(1, 2)), vrec(1, 3), brec(2, []byte("x"))),
		"last scalar wins":       cat(vrec(1, 3), vrec(1, 4), body(brec(14, []byte("a")), brec(14, []byte("b")))),
		"later zero unsets":      cat(vrec(1, 3), vrec(1, 0), brec(2, []byte("x")), brec(2, nil), body(vrec(17, 5), vrec(17, 0), brec(15, []byte{1}), brec(15, nil))),
		"zero into pointer":      cat(brec(2, nil), body(vrec(17, 0))),
		"optional keeps zero":    cat(body(brec(18, nil), vrec(19, 0))),
		"optional last wins":     cat(body(brec(18, []byte("a")), brec(18, nil), vrec(19, 4), vrec(19, 0))),
		"packed and unpacked mix": cat(body(
			brec(20, packed(1, 2, neg(-3))), vrec(20, 4), brec(20, packed()), brec(20, packed(5)),
			vrec(21, protowire.EncodeZigZag(-1)), brec(21, packed(protowire.EncodeZigZag(2))),
			f32rec(22, 9), brec(22, protowire.AppendFixed32(protowire.AppendFixed32(nil, 1), 2)),
			brec(23, protowire.AppendFixed64(nil, math.Float64bits(0.5))), f64rec(23, math.Float64bits(1)),
			brec(24, packed(1, 0, 1)), vrec(24, 0),
			brec(27, packed(0, 7)), vrec(27, 0),
		)),
		"repeated strings and bytes": cat(body(brec(25, nil), brec(25, []byte("x")), brec(26, nil), brec(26, []byte{9}))),
		"message presence":           cat(body(brec(28, nil), brec(29, nil))),
		"message merges": cat(body(
			brec(28, child(brec(1, []byte("a")), vrec(2, 3), brec(3, []byte("t1")))),
			brec(28, child(vrec(2, 0), brec(3, []byte("t2")))),
			brec(29, child(brec(1, []byte("p")))), brec(29, child(vrec(2, 8))),
		), body(vrec(13, 1))),
		"repeated messages": cat(body(
			brec(30, child(brec(1, []byte("a")))), brec(30, nil), brec(30, child(brec(3, []byte("x")), brec(3, []byte("y")))),
			brec(31, nil), brec(31, child(vrec(2, 1))),
		)),
		"unknown fields": cat(vrec(99, 1), f64rec(100, 2), brec(101, []byte("zz")), vrec(5, 9), body(vrec(77, 1), brec(28, child(vrec(9, 1))))),
		"unbound fields": cat(body(
			vrec(41, 1), brec(42, packed(0, 1, 7)), vrec(42, 3),
			brec(40, cat(brec(1, []byte("k")), brec(2, []byte("v")))), brec(40, nil),
			brec(43, child(brec(1, []byte("n")))), brec(44, cat(brec(1, cat(brec(2, []byte("deep")))))),
			brec(28, child(brec(4, []byte("extra")))),
		)),
		"wrong wire types skipped": cat(f32rec(1, 5), vrec(2, 1), vrec(4, 1), body(
			brec(1, []byte{1}), f64rec(14, 1), vrec(28, 1), f32rec(30, 1), vrec(25, 1), f32rec(20, 1),
		)),
		"group skipped": cat(protowire.AppendTag(nil, 50, protowire.StartGroupType), vrec(1, 1), protowire.AppendTag(nil, 50, protowire.EndGroupType), vrec(1, 2)),

		// Malformed: both must refuse.
		"truncated varint":       {0x08, 0x80},
		"truncated length":       {0x12, 0x05, 'a'},
		"bad length in body":     cat(brec(4, []byte{0x72, 0x09, 'x'})),
		"truncated fixed32":      {0x3d, 1, 2},
		"bad packed run":         cat(body(brec(22, []byte{1, 2, 3}))),
		"bad packed varint":      cat(body(brec(20, []byte{0x80}))),
		"field zero":             {0x00, 0x01},
		"field number too large": protowire.AppendVarint(protowire.AppendVarint(nil, uint64(protowire.MaxValidNumber+1)<<3), 1),
		"end group alone":        protowire.AppendTag(nil, 50, protowire.EndGroupType),
		"invalid utf8 bound":     cat(brec(2, []byte{0xff})),
		"invalid utf8 unbound":   cat(body(brec(28, child(brec(4, []byte{0xff}))))),
		"invalid utf8 map key":   cat(body(brec(40, cat(brec(1, []byte{0xc3}))))),
		"invalid utf8 list":      cat(body(brec(25, []byte{0xfe}))),
		"bad unknown field":      cat(brec(99, []byte{1})[:2]),
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) { assertSameDecode(t, md, wire) })
	}
}

// The input protobuf-go panics on (see errSlowPanic): a map entry whose
// key repeats with the wrong wire type. The old path took the process's
// handler goroutine down with it; the decoder skips the bad record as
// unknown, as it does everywhere else.
func TestDecodePlan_MapEntryProtoPanics(t *testing.T) {
	md := decDesc(t)
	entry := cat(brec(1, []byte("k")), vrec(1, 5))
	wire := cat(body(brec(40, entry)))
	if err := unmarshalNoPanic(wire, dynamicpb.NewMessage(md)); err != errSlowPanic {
		t.Logf("proto.Unmarshal no longer panics here (err=%v); errSlowPanic may be removable", err)
	}
	if _, err := newRequestDecoder(reflect.TypeFor[decInput](), decOp, md).decode(wire); err != nil {
		t.Errorf("decode: %v", err)
	}
}

// proto.Unmarshal caps nesting at protowire.DefaultRecursionLimit. The
// decoder has to count the same way, including through fields nothing
// binds.
func TestDecodePlan_RecursionLimit(t *testing.T) {
	md := decDesc(t)
	nest := func(levels int) []byte {
		var b []byte
		for i := 0; i < levels; i++ {
			b = brec(1, b)
		}
		return cat(body(brec(44, b)))
	}
	limit := protowire.DefaultRecursionLimit
	sawOK, sawErr := false, false
	for _, levels := range []int{limit - 4, limit - 3, limit - 2, limit - 1, limit} {
		wire := nest(levels)
		assertSameDecode(t, md, wire)
		_, _, err, _ := bothDecodes(t, reflect.TypeFor[decInput](), decOp, md, wire)
		if err == nil {
			sawOK = true
		} else {
			sawErr = true
		}
	}
	if !sawOK || !sawErr {
		t.Errorf("the range did not straddle the limit (ok=%v err=%v)", sawOK, sawErr)
	}
}

// --- generated cases -----------------------------------------------------

type wireGen struct{ r *rand.Rand }

func (g wireGen) varint() uint64 {
	vals := []uint64{0, 0, 1, 2, 127, 128, 1 << 31, 1 << 32, math.MaxUint32, math.MaxUint64, uint64(1<<63 + 5)}
	if g.r.Intn(3) == 0 {
		return g.r.Uint64() >> uint(g.r.Intn(64))
	}
	return vals[g.r.Intn(len(vals))]
}

func (g wireGen) text() []byte {
	vals := []string{"", "", "a", "héllo", "\x00"}
	if g.r.Intn(40) == 0 {
		return []byte{0xff, 'x'} // invalid UTF-8, rarely
	}
	return []byte(vals[g.r.Intn(len(vals))])
}

// value appends one value for fd with wire type wt, valid for wt.
func (g wireGen) value(b []byte, fd protoreflect.FieldDescriptor, wt protowire.Type, depth int) []byte {
	switch wt {
	case protowire.VarintType:
		return protowire.AppendVarint(b, g.varint())
	case protowire.Fixed32Type:
		return protowire.AppendFixed32(b, uint32(g.varint()))
	case protowire.Fixed64Type:
		v := g.varint()
		if fd != nil && fd.Kind() == protoreflect.DoubleKind && g.r.Intn(2) == 0 {
			v = math.Float64bits(g.r.NormFloat64())
		}
		return protowire.AppendFixed64(b, v)
	case protowire.StartGroupType:
		return protowire.AppendTag(b, fd.Number(), protowire.EndGroupType)
	}
	// BytesType
	if fd == nil {
		return protowire.AppendBytes(b, g.text())
	}
	switch {
	case fd.Kind() == protoreflect.MessageKind:
		var sub []byte
		if depth < 4 {
			sub = g.message(fd.Message(), depth+1)
		}
		return protowire.AppendBytes(b, sub)
	case fd.IsList() && scalarWireType(fd.Kind()) != protowire.BytesType:
		// A packed run.
		var run []byte
		ewt := scalarWireType(fd.Kind())
		for i := g.r.Intn(4); i > 0; i-- {
			run = g.value(run, fd, ewt, depth)
		}
		return protowire.AppendBytes(b, run)
	case fd.Kind() == protoreflect.StringKind || fd.Kind() == protoreflect.BytesKind:
		return protowire.AppendBytes(b, g.text())
	}
	// Bytes-typed value for a scalar field: wrong wire type, any payload.
	return protowire.AppendBytes(b, g.text())
}

func (g wireGen) message(md protoreflect.MessageDescriptor, depth int) []byte {
	var b []byte
	fields := md.Fields()
	for n := g.r.Intn(8); n > 0; n-- {
		if g.r.Intn(12) == 0 {
			// Unknown field number.
			num := protowire.Number(90 + g.r.Intn(5))
			wt := []protowire.Type{protowire.VarintType, protowire.Fixed32Type, protowire.Fixed64Type, protowire.BytesType}[g.r.Intn(4)]
			b = protowire.AppendTag(b, num, wt)
			b = g.value(b, nil, wt, depth)
			continue
		}
		fd := fields.Get(g.r.Intn(fields.Len()))
		wt := scalarWireType(fd.Kind())
		if fd.IsList() && wt != protowire.BytesType && g.r.Intn(2) == 0 {
			wt = protowire.BytesType // packed
		}
		if g.r.Intn(8) == 0 {
			wt = []protowire.Type{protowire.VarintType, protowire.Fixed32Type, protowire.Fixed64Type, protowire.BytesType, protowire.StartGroupType}[g.r.Intn(5)]
		}
		b = protowire.AppendTag(b, fd.Number(), wt)
		b = g.value(b, fd, wt, depth)
	}
	return b
}

func TestDecodePlan_MatchesUnmarshalBind_Generated(t *testing.T) {
	md := decDesc(t)
	g := wireGen{r: rand.New(rand.NewSource(1))}
	bodyFd := md.Fields().ByName("body")
	seen := map[string]int{}
	for i := 0; i < 20000; i++ {
		var wire []byte
		if i%2 == 0 {
			// Weight toward the body, where most of the shapes live.
			wire = brec(4, g.message(bodyFd.Message(), 1))
			wire = append(g.message(md, 0), wire...)
		} else {
			wire = g.message(md, 0)
		}
		seen[assertSameDecode(t, md, wire)]++

		// Corrupt a copy: truncate, or flip a byte.
		if len(wire) > 0 {
			bad := append([]byte(nil), wire...)
			if g.r.Intn(2) == 0 {
				bad = bad[:g.r.Intn(len(bad))]
			} else {
				bad[g.r.Intn(len(bad))] ^= byte(1 << uint(g.r.Intn(8)))
			}
			seen[assertSameDecode(t, md, bad)]++
		}
	}
	// Both outcomes have to be exercised in volume, or the generator has
	// drifted into producing only one kind of input.
	t.Logf("outcomes: %v", seen)
	if seen["ok"] < 5000 || seen["error"] < 5000 {
		t.Errorf("generator coverage too thin: %v", seen)
	}
}

// --- refusals ------------------------------------------------------------

func TestDecodePlan_Refuses(t *testing.T) {
	ty := descriptorpb.FieldDescriptorProto_TYPE_INT64

	t.Run("narrowing", func(t *testing.T) {
		type in struct {
			Limit int32 `query:"limit"`
		}
		md := bindDesc(t, "NarrowReq", []*descriptorpb.FieldDescriptorProto{field("limit", 1, ty, false)})
		if buildBindPlan(reflect.TypeFor[in](), irOp(qArg("limit")), "", md) == nil {
			t.Fatal("bind plan should form; the refusal under test is the decoder's")
		}
		if newRequestDecoder(reflect.TypeFor[in](), irOp(qArg("limit")), md) != nil {
			t.Error("decoder formed for int64 into int32")
		}
	})

	t.Run("pointer embed", func(t *testing.T) {
		type in struct {
			*BindScope
			Limit int64 `query:"limit"`
		}
		md := bindDesc(t, "PtrEmbedReq", []*descriptorpb.FieldDescriptorProto{
			field("dir", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, false),
			field("limit", 2, ty, false),
		})
		op := irOp(qArg("dir"), qArg("limit"))
		if buildBindPlan(reflect.TypeFor[in](), op, "", md) == nil {
			t.Fatal("bind plan should form")
		}
		if newRequestDecoder(reflect.TypeFor[in](), op, md) != nil {
			t.Error("decoder formed through a pointer embed")
		}
	})

	t.Run("real oneof", func(t *testing.T) {
		type in struct {
			A int64 `query:"a"`
			B int64 `query:"b"`
		}
		a, b := field("a", 1, ty, false), field("b", 2, ty, false)
		a.OneofIndex, b.OneofIndex = proto.Int32(0), proto.Int32(0)
		md := bindDescOneof(t, "OneofReq", []*descriptorpb.FieldDescriptorProto{a, b})
		op := irOp(qArg("a"), qArg("b"))
		if buildBindPlan(reflect.TypeFor[in](), op, "", md) == nil {
			t.Fatal("bind plan should form")
		}
		if newRequestDecoder(reflect.TypeFor[in](), op, md) != nil {
			t.Error("decoder formed for a real oneof")
		}
	})

	t.Run("no bind plan", func(t *testing.T) {
		md := bindDesc(t, "EnumReq2", []*descriptorpb.FieldDescriptorProto{enumField("color", 1, ".bindtest.enumreq2.Color")})
		if newRequestDecoder(reflect.TypeFor[bindEnumInput](), irOp(qArg("color")), md) != nil {
			t.Error("decoder formed where the bind plan refuses")
		}
	})

	t.Run("input checks", func(t *testing.T) {
		sd := methodFor(t, "CheckedReq", []*descriptorpb.FieldDescriptorProto{field("limit", 1, ty, false)})
		if wireDecoderFor(reflect.TypeFor[bindEmbedInput](), irOp(qArg("limit")), &inputCheck{}, sd) != nil {
			t.Error("decoder formed for an operation with input checks")
		}
		if wireDecoderFor(reflect.TypeFor[bindEmbedInput](), irOp(qArg("limit")), nil, sd) == nil {
			t.Error("no decoder for the same operation without checks")
		}
	})
}

func bindDescOneof(t *testing.T, name string, fields []*descriptorpb.FieldDescriptorProto) protoreflect.MessageDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("oneof_" + name + ".proto"),
		Package: proto.String("oneoftest"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String(name), Field: fields,
			OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("pick")}},
		}},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}})
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName("oneoftest." + name))
	if err != nil {
		t.Fatal(err)
	}
	return d.(protoreflect.MessageDescriptor)
}

// methodFor builds a method whose input is a message of fields.
func methodFor(t *testing.T, name string, fields []*descriptorpb.FieldDescriptorProto) protoreflect.MethodDescriptor {
	t.Helper()
	typ := ".methodtest." + name
	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("method_" + name + ".proto"),
		Package:     proto.String("methodtest"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String(name), Field: fields}},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name:   proto.String("S"),
			Method: []*descriptorpb.MethodDescriptorProto{{Name: proto.String("M"), InputType: proto.String(typ), OutputType: proto.String(typ)}},
		}},
	}
	f, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	return f.Services().Get(0).Methods().Get(0)
}

// The benchmark and the real gat ops must take the fast path; if the
// decoder silently stopped forming for them, every test above would
// still pass while production quietly paid for dynamicpb.
func TestDecodePlan_FormsForRealGatOperations(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Decoded", "1.0.0"))
	g, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	Register(api, g, huma.Operation{OperationID: "getThing", Method: http.MethodGet, Path: "/things/{id}"},
		func(ctx context.Context, in *struct {
			ID string `path:"id"`
		}) (*struct{ Body struct{ Name string } }, error) {
			return nil, nil
		})
	Register(api, g, huma.Operation{OperationID: "listThings", Method: http.MethodGet, Path: "/things"},
		func(ctx context.Context, in *struct {
			Limit int `query:"limit"`
		}) (*struct{ Body struct{ Names []string } }, error) {
			return nil, nil
		})
	type createBody struct {
		Name  string   `json:"name"`
		Count int      `json:"count"`
		Tags  []string `json:"tags"`
	}
	Register(api, g, huma.Operation{OperationID: "createThing", Method: http.MethodPost, Path: "/things"},
		func(ctx context.Context, in *struct {
			Dry  bool `query:"dry"`
			Body createBody
		}) (*struct{ Body struct{ Name string } }, error) {
			return nil, nil
		})
	if err := RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	fds, err := ir.RenderProtoFiles(g.services)
	if err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	formed := map[string]bool{}
	for _, c := range g.captured {
		files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
			for i := 0; i < fd.Services().Len(); i++ {
				if md := fd.Services().Get(i).Methods().ByName(protoreflect.Name(c.op.OperationID)); md != nil {
					formed[c.op.OperationID] = wireDecoderFor(c.inputType, c.irOp, c.check, md) != nil
				}
			}
			return true
		})
	}
	for _, op := range []string{"getThing", "listThings", "createThing"} {
		if !formed[op] {
			t.Errorf("%s: no decode plan — the operation silently kept the dynamicpb path", op)
		}
	}
}
