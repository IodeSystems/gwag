package gat

// Emitting a Connect response straight to proto wire bytes.
//
// The response path builds a dynamicpb message and hands it to
// proto.Marshal. A memory profile of a 25-row response puts roughly
// four fifths of the request's allocations in that arrangement:
// dynamicpb.NewMessage once per row, protoreflect.Value boxing per
// field, and proto's reflective marshalMessageSlow, which is the only
// marshaller available because dynamicpb carries no generated fast
// path.
//
// None of that work is inherent. The encode plan already knows, at
// mount time, which Go field feeds which proto field; given that, the
// bytes can be written directly and the intermediate message skipped
// altogether.
//
// This does mean owning the wire format. Three rules carry most of it,
// and getting any of them wrong yields output that still parses while
// meaning something else, so each is asserted against proto.Marshal in
// connect_wire_test.go rather than reasoned about:
//
//   - proto3 implicit presence: a zero scalar is omitted, not encoded.
//   - repeated numeric fields are packed (one LEN-delimited run);
//     repeated strings, bytes and messages are not (one tag each).
//   - a nested message is length-prefixed, so its body has to be
//     encoded before its header can be written.

import (
	"fmt"
	"math"
	"reflect"
	"sort"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// sortFields orders a plan's fields by field number. The wire format
// permits any order, but ascending is what proto.Marshal emits, and
// matching it lets the differential test compare bytes directly
// instead of only comparing decoded messages.
func (p *encodePlan) sortFields() {
	sort.Slice(p.fields, func(i, j int) bool {
		return p.fields[i].fd.Number() < p.fields[j].fd.Number()
	})
	for _, f := range p.fields {
		if f.sub != nil {
			f.sub.sortFields()
		}
	}
}

// appendMessage writes v into dst as the proto encoding of the
// message plan describes, returning the extended slice.
func appendMessage(dst []byte, plan *encodePlan, v reflect.Value) ([]byte, error) {
	v = derefValue(v)
	if !v.IsValid() {
		return dst, nil
	}
	for _, ef := range plan.fields {
		src, ok := fieldValueAt(v, ef.index)
		if !ok {
			continue
		}
		var err error
		dst, err = appendField(dst, ef, src)
		if err != nil {
			return dst, fmt.Errorf("field %s: %w", ef.fd.Name(), err)
		}
	}
	return dst, nil
}

func appendField(dst []byte, ef encodeField, src reflect.Value) ([]byte, error) {
	src = derefValue(src)
	if !src.IsValid() {
		return dst, nil
	}
	fd := ef.fd
	num := fd.Number()

	if fd.IsList() {
		if src.Kind() != reflect.Slice && src.Kind() != reflect.Array {
			return dst, fmt.Errorf("expected slice, got %s", src.Kind())
		}
		n := src.Len()
		if n == 0 {
			// An empty repeated field is absent on the wire, packed or not.
			return dst, nil
		}
		if ef.sub != nil {
			// Repeated message: one LEN-delimited record per element.
			for i := 0; i < n; i++ {
				var err error
				dst, err = appendSubMessage(dst, num, ef.sub, derefValue(src.Index(i)))
				if err != nil {
					return dst, fmt.Errorf("[%d]: %w", i, err)
				}
			}
			return dst, nil
		}
		if fd.IsPacked() {
			// One tag, one length, then the values back to back. Encode
			// the payload first because its length prefixes it.
			var payload []byte
			for i := 0; i < n; i++ {
				var err error
				payload, err = appendScalarValue(payload, fd, derefValue(src.Index(i)))
				if err != nil {
					return dst, fmt.Errorf("[%d]: %w", i, err)
				}
			}
			dst = protowire.AppendTag(dst, num, protowire.BytesType)
			dst = protowire.AppendBytes(dst, payload)
			return dst, nil
		}
		// Unpacked: strings, bytes, and any numeric field explicitly
		// marked [packed = false].
		for i := 0; i < n; i++ {
			var err error
			dst, err = appendTagged(dst, fd, derefValue(src.Index(i)))
			if err != nil {
				return dst, fmt.Errorf("[%d]: %w", i, err)
			}
		}
		return dst, nil
	}

	if ef.sub != nil {
		// Message fields carry explicit presence, so an empty one is not
		// the same as an absent one: it encodes as a zero-length record
		// and sets has_<field>. A nil pointer is genuinely absent and
		// already returned above, via derefValue. The dynamicpb path
		// draws the line in the same place — it calls Mutable, which
		// marks the field present whatever the contents.
		return appendSubMessage(dst, num, ef.sub, src)
	}

	// proto3 implicit presence: zero means absent.
	if src.IsZero() {
		return dst, nil
	}
	return appendTagged(dst, fd, src)
}

// appendSubMessage writes a length-delimited nested message. The body
// is built into a scratch slice first, since its length has to precede
// it.
func appendSubMessage(dst []byte, num protowire.Number, plan *encodePlan, v reflect.Value) ([]byte, error) {
	body, err := appendMessage(nil, plan, v)
	if err != nil {
		return dst, err
	}
	dst = protowire.AppendTag(dst, num, protowire.BytesType)
	dst = protowire.AppendBytes(dst, body)
	return dst, nil
}

// appendTagged writes one tag plus its value, choosing the wire type
// from the field's kind.
func appendTagged(dst []byte, fd protoreflect.FieldDescriptor, v reflect.Value) ([]byte, error) {
	wt, err := wireTypeOf(fd.Kind())
	if err != nil {
		return dst, err
	}
	dst = protowire.AppendTag(dst, fd.Number(), wt)
	return appendScalarValue(dst, fd, v)
}

// appendScalarValue writes a bare value, no tag — the form packed
// repeated fields need.
func appendScalarValue(dst []byte, fd protoreflect.FieldDescriptor, v reflect.Value) ([]byte, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protowire.AppendVarint(dst, protowire.EncodeBool(v.Bool())), nil
	case protoreflect.StringKind:
		return protowire.AppendString(dst, v.String()), nil
	case protoreflect.BytesKind:
		return protowire.AppendBytes(dst, v.Bytes()), nil
	case protoreflect.Int32Kind, protoreflect.Int64Kind:
		return protowire.AppendVarint(dst, uint64(v.Int())), nil
	case protoreflect.Sint32Kind:
		return protowire.AppendVarint(dst, protowire.EncodeZigZag(v.Int())), nil
	case protoreflect.Sint64Kind:
		return protowire.AppendVarint(dst, protowire.EncodeZigZag(v.Int())), nil
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
		return protowire.AppendVarint(dst, v.Uint()), nil
	case protoreflect.Sfixed32Kind:
		return protowire.AppendFixed32(dst, uint32(int32(v.Int()))), nil
	case protoreflect.Fixed32Kind:
		return protowire.AppendFixed32(dst, uint32(v.Uint())), nil
	case protoreflect.Sfixed64Kind:
		return protowire.AppendFixed64(dst, uint64(v.Int())), nil
	case protoreflect.Fixed64Kind:
		return protowire.AppendFixed64(dst, v.Uint()), nil
	case protoreflect.FloatKind:
		return protowire.AppendFixed32(dst, math.Float32bits(float32(v.Float()))), nil
	case protoreflect.DoubleKind:
		return protowire.AppendFixed64(dst, math.Float64bits(v.Float())), nil
	}
	return dst, fmt.Errorf("unsupported kind %s", fd.Kind())
}

func wireTypeOf(k protoreflect.Kind) (protowire.Type, error) {
	switch k {
	case protoreflect.BoolKind,
		protoreflect.Int32Kind, protoreflect.Int64Kind,
		protoreflect.Sint32Kind, protoreflect.Sint64Kind,
		protoreflect.Uint32Kind, protoreflect.Uint64Kind:
		return protowire.VarintType, nil
	case protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind:
		return protowire.Fixed32Type, nil
	case protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
		return protowire.Fixed64Type, nil
	case protoreflect.StringKind, protoreflect.BytesKind, protoreflect.MessageKind, protoreflect.GroupKind:
		return protowire.BytesType, nil
	}
	return 0, fmt.Errorf("no wire type for kind %s", k)
}
