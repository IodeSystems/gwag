// Package protocodec converts between GraphQL-canonical values and
// dynamicpb messages for proto upstreams. gwag (gw/) and gat (gw/gat)
// both dispatch GraphQL fields to gRPC services and share this walker;
// the points where they differ are fields on Options.
//
// GraphQL-canonical means: lowerCamel keys, args decoded the way
// graphql-go hands them to a resolver (map[string]any, []any,
// primitives), 64-bit integers as decimal strings, enums as int32
// numbers (the Value graphql-go enum configs are built with).
//
// Map fields (proto `map<K,V>`) are not handled: both directions take
// the message path and dynamicpb panics on the type mismatch.
package protocodec

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// Options selects the per-caller behaviour of ArgsToMessage. The zero
// value is gat's behaviour.
type Options struct {
	// SkipArg, when non-nil, is asked about every present arg value
	// before it is written; true leaves the field unset. gwag uses it
	// to pass unmaterialised *Upload values through untouched.
	SkipArg func(v any) bool
	// BytesFromSlice lets a bytes field take a []byte value as well as
	// a string. Without it only string is accepted.
	BytesFromSlice bool
}

// FieldInfo is a field descriptor paired with its GraphQL arg key
// (LowerCamel of the proto name).
type FieldInfo struct {
	FD      protoreflect.FieldDescriptor
	JSONKey string
}

// fieldInfoCache memoises per-MessageDescriptor field metadata. Keyed
// by descriptor identity (the interface value), not FullName: two
// gateways in the same process can register the same .proto and hold
// distinct MessageDescriptor instances with identical FullNames —
// dynamicpb.Set rejects a FieldDescriptor whose parent descriptor
// doesn't match the receiver, so cross-gateway sharing breaks
// dispatch on the second gateway.
var fieldInfoCache sync.Map // map[protoreflect.MessageDescriptor][]FieldInfo

// FieldInfos returns md's fields with their GraphQL arg keys, computed
// once per descriptor.
func FieldInfos(md protoreflect.MessageDescriptor) []FieldInfo {
	if v, ok := fieldInfoCache.Load(md); ok {
		return v.([]FieldInfo)
	}
	fields := md.Fields()
	out := make([]FieldInfo, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		out[i] = FieldInfo{FD: fd, JSONKey: LowerCamel(string(fd.Name()))}
	}
	if v, loaded := fieldInfoCache.LoadOrStore(md, out); loaded {
		return v.([]FieldInfo)
	}
	return out
}

// LowerCamel converts a snake_case proto field name to lowerCamelCase,
// the GraphQL field name the IR assigns. Same algorithm as gw/ir's
// internal helper (ir cannot import this package: protocodec imports
// ir).
func LowerCamel(s string) string {
	if s == "" {
		return s
	}
	parts := strings.Split(s, "_")
	for i := range parts {
		if i == 0 || parts[i] == "" {
			continue
		}
		r := []rune(parts[i])
		r[0] = unicode.ToUpper(r[0])
		parts[i] = string(r)
	}
	r := []rune(parts[0])
	if len(r) > 0 {
		r[0] = unicode.ToLower(r[0])
		parts[0] = string(r)
	}
	return strings.Join(parts, "")
}

// ArgsToMessage walks args (lowerCamel keys, GraphQL-canonical values)
// and writes them into msg according to its descriptor. Unknown args
// are ignored; type mismatches return errors.
func (o Options) ArgsToMessage(args map[string]any, msg *dynamicpb.Message) error {
	for _, info := range FieldInfos(msg.Descriptor()) {
		v, ok := args[info.JSONKey]
		if !ok {
			continue
		}
		if o.SkipArg != nil && o.SkipArg(v) {
			continue
		}
		if err := o.SetField(msg, info.FD, v); err != nil {
			return fmt.Errorf("field %s: %w", info.FD.Name(), err)
		}
	}
	return nil
}

// SetField writes one GraphQL-canonical value into fd on msg. A list
// field takes []any and appends to whatever the field already holds.
func (o Options) SetField(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, v any) error {
	if fd.IsList() {
		items, ok := v.([]any)
		if !ok {
			return fmt.Errorf("expected list, got %T", v)
		}
		list := msg.Mutable(fd).List()
		for _, item := range items {
			elem, err := o.toProtoScalar(fd, item)
			if err != nil {
				return err
			}
			list.Append(elem)
		}
		return nil
	}
	val, err := o.toProtoScalar(fd, v)
	if err != nil {
		return err
	}
	msg.Set(fd, val)
	return nil
}

func (o Options) toProtoScalar(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		b, ok := v.(bool)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("expected bool, got %T", v)
		}
		return protoreflect.ValueOfBool(b), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := toInt64(v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfInt32(int32(n)), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := toInt64(v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfUint32(uint32(n)), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := toInt64(v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfInt64(n), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := toInt64(v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfUint64(uint64(n)), nil
	case protoreflect.FloatKind:
		f, err := toFloat64(v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfFloat32(float32(f)), nil
	case protoreflect.DoubleKind:
		f, err := toFloat64(v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfFloat64(f), nil
	case protoreflect.StringKind:
		s, ok := v.(string)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("expected string, got %T", v)
		}
		return protoreflect.ValueOfString(s), nil
	case protoreflect.BytesKind:
		switch x := v.(type) {
		case string:
			return protoreflect.ValueOfBytes([]byte(x)), nil
		case []byte:
			if o.BytesFromSlice {
				return protoreflect.ValueOfBytes(x), nil
			}
		}
		if o.BytesFromSlice {
			return protoreflect.Value{}, fmt.Errorf("expected string or []byte for bytes, got %T", v)
		}
		return protoreflect.Value{}, fmt.Errorf("expected string for bytes, got %T", v)
	case protoreflect.EnumKind:
		switch x := v.(type) {
		case int32:
			return protoreflect.ValueOfEnum(protoreflect.EnumNumber(x)), nil
		case int:
			return protoreflect.ValueOfEnum(protoreflect.EnumNumber(x)), nil
		case string:
			// Rendered enums carry a type prefix on the descriptor; every other
			// surface speaks the unprefixed name, so resolve both spellings.
			ev := ir.EnumValueByExternalName(fd.Enum(), x)
			if ev == nil {
				return protoreflect.Value{}, fmt.Errorf("unknown enum value %q for %s", x, fd.Enum().FullName())
			}
			return protoreflect.ValueOfEnum(ev.Number()), nil
		default:
			return protoreflect.Value{}, fmt.Errorf("expected enum int|string, got %T", v)
		}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		nested, ok := v.(map[string]any)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("expected object, got %T", v)
		}
		sub := dynamicpb.NewMessage(fd.Message())
		if err := o.ArgsToMessage(nested, sub); err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfMessage(sub), nil
	}
	return protoreflect.Value{}, fmt.Errorf("unhandled kind: %v", fd.Kind())
}

func toInt64(v any) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case float64:
		return int64(x), nil
	case string:
		return strconv.ParseInt(x, 10, 64)
	default:
		return 0, fmt.Errorf("expected number, got %T", v)
	}
}

func toFloat64(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case string:
		return strconv.ParseFloat(x, 64)
	default:
		return 0, fmt.Errorf("expected number, got %T", v)
	}
}

// MessageToMap converts msg to map[string]any with lowerCamel keys for
// graphql to serialise. Unset fields are omitted; lists become []any;
// nested messages recurse. A nil msg yields an empty map.
func MessageToMap(msg *dynamicpb.Message) map[string]any {
	if msg == nil {
		return map[string]any{}
	}
	infos := FieldInfos(msg.Descriptor())
	out := make(map[string]any, len(infos))
	for _, info := range infos {
		if !msg.Has(info.FD) {
			continue
		}
		out[info.JSONKey] = ValueToAny(info.FD, msg.Get(info.FD))
	}
	return out
}

// ValueToAny renders the value of field fd (list or singular) as a
// GraphQL-canonical value.
func ValueToAny(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	if fd.IsList() {
		l := v.List()
		out := make([]any, l.Len())
		for i := 0; i < l.Len(); i++ {
			out[i] = scalarToAny(fd, l.Get(i))
		}
		return out
	}
	return scalarToAny(fd, v)
}

func scalarToAny(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return v.Bool()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return v.Int()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return v.Uint()
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		// Stringified to preserve precision (matches the Long scalar).
		return strconv.FormatInt(v.Int(), 10)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return strconv.FormatUint(v.Uint(), 10)
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return v.Float()
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.BytesKind:
		return string(v.Bytes())
	case protoreflect.EnumKind:
		// Proto enums register with graphql-go as `Value: int32(number)`.
		// graphql-go's enum Serialize matches the resolver value against
		// that config value by typed equality, so this MUST be int32 —
		// the name as a string yields null on the wire.
		return int32(v.Enum())
	case protoreflect.MessageKind, protoreflect.GroupKind:
		sub, ok := v.Message().Interface().(*dynamicpb.Message)
		if !ok {
			return nil
		}
		return MessageToMap(sub)
	}
	return nil
}
