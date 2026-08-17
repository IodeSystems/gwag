package gat

// Direct proto → args-map conversion for the Connect/gRPC ingress.
//
// The ingress used to reach the args map by text: protojson.Marshal
// the incoming message, then json.Unmarshal the bytes back into a
// map[string]any. Two full serialization passes to move between two
// in-memory representations. messageToArgs walks the message once
// instead.
//
// It reproduces protojson's `UseProtoNames: true, EmitUnpopulated:
// false` output exactly — key names, number stringification, enum
// names, base64 bytes — because bindInput matches these keys against
// ir.Operation.Arg names and silently skips what it cannot map
// (dispatch_inproc.go:166). A divergence here would not error; it
// would drop the argument and hand the handler a zero value. The
// differential test in connect_args_test.go pins the two against each
// other with protojson as the oracle.
//
// Not to be confused with protoMessageToMap in proto_convert.go: that
// one serves the proto-UPSTREAM dispatcher, which speaks the
// GraphQL-canonical convention (lowerCamel keys, integer enums). The
// two conventions are genuinely different and cannot share a walker.

import (
	"encoding/base64"
	"strconv"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// messageToArgs converts msg into the args map bindInput expects.
// Unpopulated fields are omitted, matching EmitUnpopulated: false —
// bindInput skips absent keys, so an omitted field leaves the huma
// input's own default in place.
func messageToArgs(msg *dynamicpb.Message) map[string]any {
	if msg == nil {
		return map[string]any{}
	}
	return rangeToMap(msg)
}

// rangeToMap walks the populated fields of m. Range visits only what
// is set, which is exactly the EmitUnpopulated: false contract, and
// avoids the descriptor-wide scan protoMessageToMap does.
func rangeToMap(m protoreflect.Message) map[string]any {
	out := make(map[string]any, m.Descriptor().Fields().Len())
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		out[string(fd.Name())] = fieldToAny(fd, v)
		return true
	})
	return out
}

// fieldToAny renders one field, dispatching on cardinality first —
// map and list have to be checked before the scalar kind switch,
// since a repeated string field still reports StringKind.
func fieldToAny(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	switch {
	case fd.IsMap():
		mp := v.Map()
		out := make(map[string]any, mp.Len())
		valFd := fd.MapValue()
		mp.Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
			// protojson renders map keys as strings whatever the key
			// kind — JSON object keys can't be anything else.
			out[k.String()] = valueToAny(valFd, mv)
			return true
		})
		return out
	case fd.IsList():
		l := v.List()
		out := make([]any, l.Len())
		for i := 0; i < l.Len(); i++ {
			out[i] = valueToAny(fd, l.Get(i))
		}
		return out
	default:
		return valueToAny(fd, v)
	}
}

// valueToAny renders a single (non-repeated) value the way protojson
// would encode it, then decode back through encoding/json.
//
// The two shifts that matter, both forced by JSON's limits:
//
//   - 64-bit integers become decimal strings; JSON numbers are
//     float64 and would lose precision past 2^53.
//   - enums become their value name; protojson emits names, and the
//     round-trip through json.Unmarshal preserved them as strings.
//
// Everything else lands on the Go type json.Unmarshal would have
// produced: bool, string, float64 for 32-bit ints and floats.
func valueToAny(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return v.Bool()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return float64(v.Int())
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return float64(v.Uint())
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return strconv.FormatInt(v.Int(), 10)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return strconv.FormatUint(v.Uint(), 10)
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return v.Float()
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.BytesKind:
		return base64.StdEncoding.EncodeToString(v.Bytes())
	case protoreflect.EnumKind:
		num := v.Enum()
		if ed := fd.Enum().Values().ByNumber(num); ed != nil {
			return string(ed.Name())
		}
		// Unknown number with no declared name — protojson falls back
		// to the bare number.
		return float64(num)
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return rangeToMap(v.Message())
	}
	return nil
}
