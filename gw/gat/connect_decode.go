package gat

// Decoding a Connect request straight from proto wire bytes into the
// huma input struct.
//
// The request path used to build a dynamicpb message — NewMessage,
// then proto.Unmarshal's reflective slow path (dynamicpb has no
// generated one), then a protoreflect.Value per field — only for the
// bind plan to copy each value out again. connect_wire.go removed the
// same detour on the response side; this is its mirror.
//
// The bind plan already knows which proto field feeds which Go field,
// so the decode plan is built from it, and inherits its refusals. What
// it adds is the part proto.Unmarshal used to own: the wire format and
// proto3's presence rules. Each rule below is pinned against
// proto.Unmarshal + bindPlan.bind in connect_decode_test.go, since a
// mistake here yields a plausible struct, not an error:
//
//   - a zero scalar on the wire is the field being unset (implicit
//     presence), and the binder skips unset fields — so a later zero
//     undoes an earlier value rather than being written as one.
//   - a scalar seen twice keeps the last value; a message seen twice
//     merges; a repeated field appends across occurrences.
//   - a repeated numeric field accepts packed and unpacked runs, in
//     any mix.
//   - a field arriving with the wrong wire type is an unknown field,
//     skipped, not an error.
//   - everything proto.Unmarshal rejects is rejected, including in
//     fields nothing binds: malformed varints and lengths, invalid
//     UTF-8 in proto3 strings, nesting past the recursion limit.
//
// The plan refuses, at mount time, any shape where reproducing the
// slow path would take more than this: real oneofs (setting one member
// clears the others), extensions, groups, a Go field narrower than its
// proto field (the binder's overflow error depends on the final value,
// not on each occurrence), and Go field paths that cross a pointer or
// overlap. Those operations keep the dynamicpb path.

import (
	"errors"
	"math"
	"reflect"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Errors match proto.Unmarshal's wording; the call sites only care
// that one is returned, but a client reading it should not be able to
// tell which decoder ran.
var (
	errWireDecode    = errors.New("proto: cannot parse invalid wire-format data")
	errWireRecursion = errors.New("proto: exceeded maximum recursion depth")
	errWireUTF8      = errors.New("proto: string field contains invalid UTF-8")

	// errWireType is internal: the field arrived with a wire type its
	// kind does not take, so it is skipped as unknown.
	errWireType = errors.New("gat: wire type mismatch")
)

// maxDecodeFieldNumber bounds the dense field table. gat's messages
// are synthesized from IR with sequential numbers; a message numbered
// past this keeps the dynamicpb path rather than paying for a sparse
// lookup.
const maxDecodeFieldNumber = 1024

// decodePlan is one message's wire-to-struct wiring. fields is indexed
// by field number; a nil entry is a number the message does not
// declare.
type decodePlan struct {
	fields []*decodeField
}

type decodeField struct {
	fd       protoreflect.FieldDescriptor
	kind     protoreflect.Kind
	list     bool // repeated, not a map
	packable bool // repeated numeric: a packed run is accepted too
	isMap    bool
	utf8     bool // proto3 string: invalid UTF-8 is an error
	presence bool // explicit presence: a zero value is still a value

	// path is the Go field the value lands in, relative to the struct
	// the enclosing plan decodes into. nil means nothing binds this
	// field: it is still parsed, for the errors, and then dropped.
	path []int
	elem reflect.Type // element type, for a bound list

	// sub decodes a message-kinded field (or a map entry). Bound when
	// path is set, validate-only otherwise.
	sub *decodePlan
}

// buildDecodePlan derives the wire decoder from a bind plan. A nil
// return means "keep the dynamicpb path" and is a normal outcome.
func buildDecodePlan(inputType reflect.Type, bp *bindPlan, md protoreflect.MessageDescriptor) *decodePlan {
	if bp == nil || inputType == nil || md == nil {
		return nil
	}
	b := &decodeBuilder{validate: map[protoreflect.FullName]*decodePlan{}}
	bound := map[protoreflect.FieldNumber]boundField{}
	var paths [][]int
	for _, f := range bp.params {
		bound[f.fd.Number()] = boundField{path: f.path, goType: fieldTypeAt(inputType, f.path)}
		paths = append(paths, f.path)
	}
	if bp.body != nil {
		bound[bp.body.fd.Number()] = boundField{path: bp.body.path, goType: fieldTypeAt(inputType, bp.body.path), sub: bp.body.sub}
		paths = append(paths, bp.body.path)
	}
	if !plainPaths(inputType, paths) {
		return nil
	}
	return b.bound(md, bound)
}

type boundField struct {
	path   []int
	goType reflect.Type
	sub    *structBind // message-kinded fields
}

type decodeBuilder struct {
	// validate caches validate-only plans by message, so a recursive
	// message type terminates.
	validate map[protoreflect.FullName]*decodePlan
}

// bound builds the plan for a message some of whose fields bind.
func (b *decodeBuilder) bound(md protoreflect.MessageDescriptor, bound map[protoreflect.FieldNumber]boundField) *decodePlan {
	if !decodableMessage(md) {
		return nil
	}
	// A real oneof clears its other members when one is set. The
	// binder only sees the survivor; reproducing that is not worth it
	// for a shape gat's IR rendering does not produce for inputs.
	for i := 0; i < md.Oneofs().Len(); i++ {
		if !md.Oneofs().Get(i).IsSynthetic() {
			return nil
		}
	}
	p := &decodePlan{fields: make([]*decodeField, maxFieldNumber(md)+1)}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		f := newDecodeField(fd)
		bf, ok := bound[fd.Number()]
		switch {
		case !ok:
			if !b.fillValidate(f) {
				return nil
			}
		case bf.sub != nil:
			// Message-kinded and bound: the binder's structBind says
			// which of its fields land where.
			elem := derefType(bf.goType)
			if f.list {
				if elem == nil || elem.Kind() != reflect.Slice {
					return nil
				}
				f.elem = elem.Elem()
				elem = derefType(f.elem)
			}
			sub := b.fromStructBind(elem, fd.Message(), bf.sub)
			if sub == nil {
				return nil
			}
			f.path, f.sub = bf.path, sub
		default:
			t := derefType(bf.goType)
			if f.list {
				if t == nil || t.Kind() != reflect.Slice {
					return nil
				}
				f.elem = t.Elem()
				t = derefType(f.elem)
			}
			if !lossless(fd.Kind(), t) {
				return nil
			}
			f.path = bf.path
		}
		p.fields[fd.Number()] = f
	}
	return p
}

// fromStructBind builds a bound plan for a nested message.
func (b *decodeBuilder) fromStructBind(t reflect.Type, md protoreflect.MessageDescriptor, sb *structBind) *decodePlan {
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	bound := make(map[protoreflect.FieldNumber]boundField, len(sb.fields))
	paths := make([][]int, 0, len(sb.fields))
	for _, f := range sb.fields {
		bound[f.fd.Number()] = boundField{path: f.idx, goType: fieldTypeAt(t, f.idx), sub: f.sub}
		paths = append(paths, f.idx)
	}
	if !plainPaths(t, paths) {
		return nil
	}
	return b.bound(md, bound)
}

// fillValidate gives an unbound field what it needs to be parsed and
// checked without being stored.
func (b *decodeBuilder) fillValidate(f *decodeField) bool {
	if f.kind != protoreflect.MessageKind {
		return true
	}
	f.sub = b.validatePlan(f.fd.Message())
	return f.sub != nil
}

func (b *decodeBuilder) validatePlan(md protoreflect.MessageDescriptor) *decodePlan {
	if p, ok := b.validate[md.FullName()]; ok {
		return p
	}
	if !decodableMessage(md) {
		return nil
	}
	p := &decodePlan{fields: make([]*decodeField, maxFieldNumber(md)+1)}
	b.validate[md.FullName()] = p // before recursing: the type may contain itself
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := newDecodeField(fields.Get(i))
		if !b.fillValidate(f) {
			delete(b.validate, md.FullName())
			return nil
		}
		p.fields[f.fd.Number()] = f
	}
	return p
}

func newDecodeField(fd protoreflect.FieldDescriptor) *decodeField {
	f := &decodeField{
		fd:       fd,
		kind:     fd.Kind(),
		isMap:    fd.IsMap(),
		presence: fd.HasPresence(),
		utf8:     fd.Kind() == protoreflect.StringKind && enforceUTF8(fd),
	}
	f.list = fd.IsList()
	f.packable = f.list && scalarWireType(f.kind) != protowire.BytesType
	return f
}

// decodableMessage reports whether md avoids every shape the decoder
// does not reproduce: extensions (proto.Unmarshal resolves them from
// the global registry), groups, and numbers past the dense table.
func decodableMessage(md protoreflect.MessageDescriptor) bool {
	if md.ExtensionRanges().Len() > 0 || maxFieldNumber(md) > maxDecodeFieldNumber {
		return false
	}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		if fields.Get(i).Kind() == protoreflect.GroupKind {
			return false
		}
	}
	return true
}

func maxFieldNumber(md protoreflect.MessageDescriptor) protoreflect.FieldNumber {
	var max protoreflect.FieldNumber
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		if n := fields.Get(i).Number(); n > max {
			max = n
		}
	}
	return max
}

// enforceUTF8 mirrors protobuf's internal strs.EnforceUTF8: proto3
// strings are validated, proto2 are not, and editions say per field.
func enforceUTF8(fd protoreflect.FieldDescriptor) bool {
	if fd.Syntax() == protoreflect.Editions {
		if e, ok := fd.(interface{ EnforceUTF8() bool }); ok {
			return e.EnforceUTF8()
		}
	}
	return fd.Syntax() == protoreflect.Proto3
}

// plainPaths reports whether each path reaches its leaf through plain
// struct fields only, and no path is a prefix of another. The decoder
// resets a field to zero when the wire unsets it; through a pointer
// that would have to allocate what the binder left nil, and with
// overlapping paths the reset would reach a neighbour.
func plainPaths(t reflect.Type, paths [][]int) bool {
	for i, p := range paths {
		cur := t
		for n, idx := range p {
			if cur.Kind() != reflect.Struct || idx >= cur.NumField() {
				return false
			}
			cur = cur.Field(idx).Type
			if n < len(p)-1 && cur.Kind() == reflect.Pointer {
				return false
			}
		}
		for j, q := range paths {
			if i != j && isPrefix(p, q) {
				return false
			}
		}
	}
	return true
}

func isPrefix(p, q []int) bool {
	if len(p) > len(q) {
		return false
	}
	for i := range p {
		if p[i] != q[i] {
			return false
		}
	}
	return true
}

// lossless reports whether every value of proto kind pk fits the Go
// type. bindableInto already settled that the pairing is meaningful;
// this narrows it to the pairings that cannot overflow, since the
// binder's overflow check sees only the final value and the decoder
// sees each occurrence.
func lossless(pk protoreflect.Kind, t reflect.Type) bool {
	if !bindableScalar(pk, t) {
		return false
	}
	switch pk {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.FloatKind:
		return t.Bits() >= 32
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind, protoreflect.DoubleKind:
		return t.Bits() >= 64
	}
	return true // bool, string, bytes
}

// scalarWireType is the wire type a single value of kind k uses.
func scalarWireType(k protoreflect.Kind) protowire.Type {
	switch k {
	case protoreflect.BoolKind, protoreflect.EnumKind,
		protoreflect.Int32Kind, protoreflect.Int64Kind,
		protoreflect.Sint32Kind, protoreflect.Sint64Kind,
		protoreflect.Uint32Kind, protoreflect.Uint64Kind:
		return protowire.VarintType
	case protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind:
		return protowire.Fixed32Type
	case protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
		return protowire.Fixed64Type
	}
	return protowire.BytesType
}

// decode parses one message from b into dst, an addressable struct, or
// only validates it when dst is invalid. depth is the remaining
// recursion budget, counted the way proto.Unmarshal counts it.
func (p *decodePlan) decode(b []byte, dst reflect.Value, depth int) error {
	if depth--; depth < 0 {
		return errWireRecursion
	}
	for len(b) > 0 {
		num, wtyp, n := protowire.ConsumeTag(b)
		if n < 0 || num > protowire.MaxValidNumber {
			return errWireDecode
		}
		b = b[n:]
		var f *decodeField
		if int(num) < len(p.fields) {
			f = p.fields[num]
		}
		err := errWireType
		if f != nil {
			n, err = f.decode(b, wtyp, dst, depth)
		}
		if err == errWireType {
			// Unknown, or known with the wrong wire type: skipped.
			n = protowire.ConsumeFieldValue(num, wtyp, b)
			if n < 0 {
				return errWireDecode
			}
		} else if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// decode parses one occurrence of f, returning the bytes consumed.
func (f *decodeField) decode(b []byte, wtyp protowire.Type, dst reflect.Value, depth int) (int, error) {
	bound := f.path != nil && dst.IsValid()
	var target reflect.Value
	if bound {
		target = fieldAt(dst, f.path)
	}

	if f.isMap || f.kind == protoreflect.MessageKind {
		if wtyp != protowire.BytesType {
			return 0, errWireType
		}
		v, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return 0, errWireDecode
		}
		switch {
		case !bound:
			// A map entry is a two-field message; validating it as one
			// checks what proto.Unmarshal's map path checks, at the
			// same recursion cost.
			return n, f.sub.decode(v, reflect.Value{}, depth)
		case f.list:
			s := allocDeref(target)
			return n, f.sub.decode(v, allocDeref(appendZero(s)), depth)
		default:
			// Presence on appearance, merge on repeat — what Mutable did.
			return n, f.sub.decode(v, allocDeref(target), depth)
		}
	}

	if f.kind == protoreflect.StringKind || f.kind == protoreflect.BytesKind {
		if wtyp != protowire.BytesType {
			return 0, errWireType
		}
		v, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return 0, errWireDecode
		}
		if f.utf8 && !utf8.Valid(v) {
			return 0, errWireUTF8
		}
		if !bound {
			return n, nil
		}
		if f.list {
			setLen(appendZero(allocDeref(target)), f.kind, v)
			return n, nil
		}
		if len(v) == 0 && !f.presence {
			resetField(target)
			return n, nil
		}
		setLen(target, f.kind, v)
		return n, nil
	}

	want := scalarWireType(f.kind)
	if f.packable && wtyp == protowire.BytesType {
		run, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return 0, errWireDecode
		}
		var s reflect.Value
		if bound && len(run) > 0 {
			s = allocDeref(target)
			s.Grow(packedCount(run, want))
		}
		for len(run) > 0 {
			raw, m := consumeScalar(run, want)
			if m < 0 {
				return 0, errWireDecode
			}
			run = run[m:]
			if bound {
				setRaw(appendZero(s), f.kind, raw)
			}
		}
		return n, nil
	}
	if wtyp != want {
		return 0, errWireType
	}
	raw, n := consumeScalar(b, want)
	if n < 0 {
		return 0, errWireDecode
	}
	switch {
	case !bound:
	case f.list:
		setRaw(appendZero(allocDeref(target)), f.kind, raw)
	case !f.presence && rawIsZero(f.kind, raw):
		// Implicit presence: zero is "unset", and the binder leaves
		// unset fields alone. The struct started zero, so undoing any
		// earlier occurrence is the same as never having written it.
		resetField(target)
	default:
		setRaw(target, f.kind, raw)
	}
	return n, nil
}

func consumeScalar(b []byte, wt protowire.Type) (uint64, int) {
	switch wt {
	case protowire.VarintType:
		return protowire.ConsumeVarint(b)
	case protowire.Fixed32Type:
		v, n := protowire.ConsumeFixed32(b)
		return uint64(v), n
	default:
		return protowire.ConsumeFixed64(b)
	}
}

// packedCount is the element count of a packed run, for sizing the
// slice once. A malformed run only makes it an estimate; decoding
// rejects the run anyway.
func packedCount(run []byte, wt protowire.Type) int {
	switch wt {
	case protowire.Fixed32Type:
		return len(run) / 4
	case protowire.Fixed64Type:
		return len(run) / 8
	}
	n := 0
	for _, c := range run {
		if c < 0x80 {
			n++
		}
	}
	return n
}

// rawIsZero reports whether a wire value is its kind's zero, using the
// same truncation the value will get when stored (zigzag maps zero to
// zero, so sint32 needs no decoding). -0.0 is not zero, as dynamicpb's
// presence check has it.
func rawIsZero(k protoreflect.Kind, raw uint64) bool {
	switch k {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.FloatKind:
		return uint32(raw) == 0
	}
	return raw == 0
}

// setRaw stores a numeric or bool wire value into dst, allocating
// through pointers. lossless has already ruled out overflow.
func setRaw(dst reflect.Value, k protoreflect.Kind, raw uint64) {
	dst = allocDeref(dst)
	switch k {
	case protoreflect.BoolKind:
		dst.SetBool(raw != 0)
	case protoreflect.Int32Kind:
		dst.SetInt(int64(int32(raw)))
	case protoreflect.Sint32Kind:
		dst.SetInt(int64(int32(protowire.DecodeZigZag(raw & math.MaxUint32))))
	case protoreflect.Sfixed32Kind:
		dst.SetInt(int64(int32(uint32(raw))))
	case protoreflect.Int64Kind, protoreflect.Sfixed64Kind:
		dst.SetInt(int64(raw))
	case protoreflect.Sint64Kind:
		dst.SetInt(protowire.DecodeZigZag(raw))
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		dst.SetUint(uint64(uint32(raw)))
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		dst.SetUint(raw)
	case protoreflect.FloatKind:
		dst.SetFloat(float64(math.Float32frombits(uint32(raw))))
	case protoreflect.DoubleKind:
		dst.SetFloat(math.Float64frombits(raw))
	}
}

// setLen stores a string or bytes payload. Bytes are copied: v aliases
// connect's pooled read buffer.
func setLen(dst reflect.Value, k protoreflect.Kind, v []byte) {
	dst = allocDeref(dst)
	if k == protoreflect.StringKind {
		dst.SetString(string(v))
		return
	}
	out := make([]byte, len(v))
	copy(out, v)
	dst.SetBytes(out)
}

// fieldAt walks a path that plainPaths has cleared: struct fields
// only, so nothing is allocated on the way.
func fieldAt(v reflect.Value, path []int) reflect.Value {
	for _, i := range path {
		v = v.Field(i)
	}
	return v
}

// resetField returns a field to its zero value — nil, for a pointer.
// Unconditional: reflect's IsZero counts -0.0 as zero, and an earlier
// -0.0 has to be undone like any other value.
func resetField(v reflect.Value) {
	v.SetZero()
}

// appendZero extends slice s by one zero element and returns it.
func appendZero(s reflect.Value) reflect.Value {
	n := s.Len()
	if n == s.Cap() {
		s.Grow(1)
	}
	s.SetLen(n + 1)
	el := s.Index(n)
	el.SetZero()
	return el
}
