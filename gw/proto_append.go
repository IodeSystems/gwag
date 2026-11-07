package gateway

import (
	"encoding/base64"
	"math"
	"sort"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/IodeSystems/graphql-go/v2/language/ast"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// appendProtoMessage walks `msg` in lockstep with `selSet` (the local
// GraphQL selection AST for this message) and emits a JSON object
// covering only the selected fields. Field names use the cached
// lowerCamel jsonKey (see fieldInfosFor); response keys honor
// per-field aliases.
//
// The "no AppendDispatcher detour through messageToMap" version of
// the proto dispatch path — skips the full-message map allocation
// in favor of one append-only write into `dst`. Per-request alloc
// drops to roughly the depth of the response (one slice grow per
// JSON layer) instead of one map per nested message.
//
// A nil selection emits every set field. Use appendProtoObject to
// pass the request's fragments and variables.
func appendProtoMessage(dst []byte, msg protoreflect.Message, selSet *ast.SelectionSet) []byte {
	return appendProtoObject(dst, msg, selSet, nil, fieldCollector{})
}

// appendProtoObject emits msg projected onto the selection `sel`
// merged with `more` (one selection set per occurrence of a response
// key that is selected more than once; nil in the common case).
//
// A plain selection (fields only, no directives, no repeated response
// key; see plainSelection) is emitted in order. Any other selection
// goes through field collection (fc.collect): named and inline
// fragments, @skip / @include, merged repeated keys. Proto schemas
// have no unions or interfaces (a oneof renders as nullable fields),
// so every fragment that graphql-go validated applies.
func appendProtoObject(dst []byte, msg protoreflect.Message, sel *ast.SelectionSet, more []*ast.SelectionSet, fc fieldCollector) []byte {
	dst = append(dst, '{')
	if sel == nil && len(more) == 0 {
		// No selection — defensive fallback. Emit every set field
		// using the cached field info. Real GraphQL queries always
		// carry a selection on object fields.
		first := true
		for _, info := range fieldInfosFor(msg.Descriptor()) {
			if !msg.Has(info.FD) {
				continue
			}
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = appendJSONString(dst, info.JSONKey)
			dst = append(dst, ':')
			dst = appendProtoFieldValue(dst, info.FD, msg.Get(info.FD), nil, nil, fc)
		}
		return append(dst, '}')
	}
	info := protoAppendInfoFor(msg.Descriptor())
	first := true
	if len(more) == 0 && plainSelection(sel) {
		for _, s := range sel.Selections {
			f := s.(*ast.Field)
			dst, first = appendProtoField(dst, msg, info, first, selectionResponseKey(f), f.Name.Value, f.SelectionSet, nil, fc)
		}
		return append(dst, '}')
	}
	var buf [plainSelectionMax]collectedField
	fields := fc.collect(buf[:0], sel, fragmentGate{}, nil)
	for _, s := range more {
		fields = fc.collect(fields, s, fragmentGate{}, nil)
	}
	for i := range fields {
		c := &fields[i]
		var sub *ast.SelectionSet
		var subMore []*ast.SelectionSet
		if len(c.sels) > 0 {
			sub, subMore = c.sels[0], c.sels[1:]
		}
		dst, first = appendProtoField(dst, msg, info, first, c.key, c.name, sub, subMore, fc)
	}
	return append(dst, '}')
}

// appendProtoField emits one `"key":value` pair for the field `name`
// of msg, preceded by a comma unless first. It returns the updated
// first flag; a field the message does not have emits nothing.
func appendProtoField(dst []byte, msg protoreflect.Message, info *protoAppendInfo, first bool, key, name string, sel *ast.SelectionSet, more []*ast.SelectionSet, fc fieldCollector) ([]byte, bool) {
	if name == "__typename" {
		if !first {
			dst = append(dst, ',')
		}
		dst = appendJSONString(dst, key)
		dst = append(dst, ':')
		return appendJSONString(dst, info.typeName), false
	}
	fd, ok := info.byJSONKey[name]
	if !ok {
		// Unknown field in selection — graphql-go validates
		// against the local schema, which the renderer built
		// from these very descriptors, so this should be
		// unreachable. Be defensive.
		return dst, first
	}
	if !first {
		dst = append(dst, ',')
	}
	dst = appendJSONString(dst, key)
	dst = append(dst, ':')
	if !msg.Has(fd) {
		// A field with presence (message, proto2 scalar, proto3
		// `optional`, oneof member) is null when unset, as
		// protojson emits it and as the map path omits it.
		if fd.HasPresence() {
			return append(dst, "null"...), false
		}
		return appendProtoDefault(dst, fd), false
	}
	return appendProtoFieldValue(dst, fd, msg.Get(fd), sel, more, fc), false
}

// appendProtoFieldValue dispatches on list vs scalar; recurses into
// nested messages with the sub-selection.
func appendProtoFieldValue(dst []byte, fd protoreflect.FieldDescriptor, v protoreflect.Value, sel *ast.SelectionSet, more []*ast.SelectionSet, fc fieldCollector) []byte {
	if fd.IsMap() {
		return appendProtoMap(dst, fd, v.Map())
	}
	if fd.IsList() {
		dst = append(dst, '[')
		l := v.List()
		for i := 0; i < l.Len(); i++ {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendProtoLeaf(dst, fd, l.Get(i), sel, more, fc)
		}
		return append(dst, ']')
	}
	return appendProtoLeaf(dst, fd, v, sel, more, fc)
}

// appendProtoMap emits a map field as a JSON object (the IR types it
// as the JSON scalar, so there is no sub-selection). Keys are sorted,
// as encoding/json sorts them on the map path.
func appendProtoMap(dst []byte, fd protoreflect.FieldDescriptor, mp protoreflect.Map) []byte {
	keys := make([]protoreflect.MapKey, 0, mp.Len())
	mp.Range(func(k protoreflect.MapKey, _ protoreflect.Value) bool {
		keys = append(keys, k)
		return true
	})
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	valFd := fd.MapValue()
	dst = append(dst, '{')
	for i, k := range keys {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendJSONString(dst, k.String())
		dst = append(dst, ':')
		dst = appendProtoLeaf(dst, valFd, mp.Get(k), nil, nil, fieldCollector{})
	}
	return append(dst, '}')
}

// appendProtoLeaf emits a single (non-list) proto value as JSON.
// Sub-selection only matters for MessageKind.
func appendProtoLeaf(dst []byte, fd protoreflect.FieldDescriptor, v protoreflect.Value, sel *ast.SelectionSet, more []*ast.SelectionSet, fc fieldCollector) []byte {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		if v.Bool() {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return strconv.AppendInt(dst, v.Int(), 10)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return strconv.AppendUint(dst, v.Uint(), 10)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		// Stringified for precision parity with messageToMap's
		// formatInt64 — matches the Long scalar emission on the SDL
		// side.
		dst = append(dst, '"')
		dst = strconv.AppendInt(dst, v.Int(), 10)
		return append(dst, '"')
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		dst = append(dst, '"')
		dst = strconv.AppendUint(dst, v.Uint(), 10)
		return append(dst, '"')
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			// JSON has no NaN / Inf; emit null to match what
			// json.Marshal does in the map path.
			return append(dst, "null"...)
		}
		abs := math.Abs(f)
		fmtByte := byte('f')
		if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
			fmtByte = 'e'
		}
		return strconv.AppendFloat(dst, f, fmtByte, -1, 64)
	case protoreflect.StringKind:
		return appendJSONString(dst, v.String())
	case protoreflect.BytesKind:
		dst = append(dst, '"')
		// Standard base64 with padding (proto3 JSON spec).
		enc := base64.StdEncoding
		buf := v.Bytes()
		size := enc.EncodedLen(len(buf))
		// Reserve via append; encode in place.
		out := make([]byte, size)
		enc.Encode(out, buf)
		dst = append(dst, out...)
		return append(dst, '"')
	case protoreflect.EnumKind:
		// scalarToAny returns int32(v.Enum()); the corresponding JSON
		// representation matches proto3-JSON canonical form (integer
		// for unnamed serialisation). graphql-go's enum types index
		// by int32 numerical value (see types.go enumFromDescriptor),
		// which matches the value graphql-codegen consumers expect to
		// see post-Serialize. Emit the enum's name as a JSON string —
		// graphql-go's enum output writer would call
		// Enum.Serialize(int32) → name → JSON string. We short-circuit
		// here, but to stay format-compatible we still emit the name.
		ev := fd.Enum().Values().ByNumber(v.Enum())
		if ev == nil {
			return strconv.AppendInt(dst, int64(v.Enum()), 10)
		}
		return appendJSONString(dst, string(ev.Name()))
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return appendProtoObject(dst, v.Message(), sel, more, fc)
	}
	return append(dst, "null"...)
}

// appendProtoDefault emits the JSON form of a proto3 default value
// when the field is unset. messageToMap's path uses `Has(fd)` to skip
// unset fields entirely; for graphql conformance (the local schema
// declares NonNull on most fields), unset becomes the zero-value
// JSON. Matches what protojson.MarshalAppend would produce for an
// unset field in `emit_unpopulated` mode.
func appendProtoDefault(dst []byte, fd protoreflect.FieldDescriptor) []byte {
	if fd.IsList() {
		return append(dst, "[]"...)
	}
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return append(dst, "false"...)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return append(dst, '0')
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return append(dst, `"0"`...)
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return append(dst, '0')
	case protoreflect.StringKind:
		return append(dst, `""`...)
	case protoreflect.BytesKind:
		return append(dst, `""`...)
	case protoreflect.EnumKind:
		// Default enum value is the value with number 0.
		ev := fd.Enum().Values().ByNumber(0)
		if ev != nil {
			return appendJSONString(dst, string(ev.Name()))
		}
		return append(dst, '0')
	}
	return append(dst, "null"...)
}

// protoAppendInfo is the per-descriptor data the append walker reads:
// fields by JSON key, and the local GraphQL type name that __typename
// returns (the proto type builder names objects with exportedName of
// the full name, e.g. "Auth_V1_User").
type protoAppendInfo struct {
	byJSONKey map[string]protoreflect.FieldDescriptor
	typeName  string
}

// protoAppendInfoCache memoises protoAppendInfo per descriptor, like
// protocodec.FieldInfos.
var protoAppendInfoCache sync.Map // map[protoreflect.MessageDescriptor]*protoAppendInfo

func protoAppendInfoFor(md protoreflect.MessageDescriptor) *protoAppendInfo {
	if v, ok := protoAppendInfoCache.Load(md); ok {
		return v.(*protoAppendInfo)
	}
	infos := fieldInfosFor(md)
	out := &protoAppendInfo{
		byJSONKey: make(map[string]protoreflect.FieldDescriptor, len(infos)),
		typeName:  exportedName(string(md.FullName())),
	}
	for _, info := range infos {
		out.byJSONKey[info.JSONKey] = info.FD
	}
	if v, loaded := protoAppendInfoCache.LoadOrStore(md, out); loaded {
		return v.(*protoAppendInfo)
	}
	return out
}

// appendJSONString writes a JSON-encoded string literal to dst. The
// fork's appendJSONString does the same with a tighter ASCII fast
// path; we don't have access to it from outside the package, so we
// inline a minimal version here. Covers escape rules for ", \, /,
// control chars, and non-ASCII UTF-8.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			if b == '"' || b == '\\' || b < 0x20 {
				dst = append(dst, s[start:i]...)
				switch b {
				case '"':
					dst = append(dst, '\\', '"')
				case '\\':
					dst = append(dst, '\\', '\\')
				case '\n':
					dst = append(dst, '\\', 'n')
				case '\r':
					dst = append(dst, '\\', 'r')
				case '\t':
					dst = append(dst, '\\', 't')
				case '\b':
					dst = append(dst, '\\', 'b')
				case '\f':
					dst = append(dst, '\\', 'f')
				default:
					dst = append(dst, '\\', 'u', '0', '0',
						hexDigit(b>>4), hexDigit(b&0xF))
				}
				i++
				start = i
				continue
			}
			i++
			continue
		}
		// Multi-byte UTF-8 — pass through verbatim.
		_, n := utf8.DecodeRuneInString(s[i:])
		i += n
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

func hexDigit(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'a' + (b - 10)
}
