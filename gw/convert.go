package gateway

import (
	"fmt"
	"strconv"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// argsToMessage walks `args` (graphql-decoded JSON-ish: map[string]any,
// []any, primitives) and writes them into `msg` according to its
// descriptor. Unknown args are ignored; type mismatches return errors.
func argsToMessage(args map[string]any, msg *dynamicpb.Message) error {
	md := msg.Descriptor()
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		key := lowerCamel(string(fd.Name()))
		v, ok := args[key]
		if !ok {
			continue
		}
		if err := setField(msg, fd, v); err != nil {
			return fmt.Errorf("field %s: %w", fd.Name(), err)
		}
	}
	return nil
}

func setField(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, v any) error {
	if fd.IsList() {
		items, ok := v.([]any)
		if !ok {
			return fmt.Errorf("expected list, got %T", v)
		}
		list := msg.Mutable(fd).List()
		for _, item := range items {
			elem, err := toProtoScalar(fd, item)
			if err != nil {
				return err
			}
			list.Append(elem)
		}
		return nil
	}
	val, err := toProtoScalar(fd, v)
	if err != nil {
		return err
	}
	msg.Set(fd, val)
	return nil
}

func toProtoScalar(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, error) {
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
		s, ok := v.(string)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("expected string for bytes, got %T", v)
		}
		return protoreflect.ValueOfBytes([]byte(s)), nil
	case protoreflect.EnumKind:
		switch x := v.(type) {
		case int32:
			return protoreflect.ValueOfEnum(protoreflect.EnumNumber(x)), nil
		case int:
			return protoreflect.ValueOfEnum(protoreflect.EnumNumber(x)), nil
		case string:
			ev := fd.Enum().Values().ByName(protoreflect.Name(x))
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
		if err := argsToMessage(nested, sub); err != nil {
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
		n, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return 0, err
		}
		return n, nil
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

// messageToMap converts a dynamicpb.Message to map[string]any for
// graphql to serialise. Unset fields are omitted; lists become []any;
// nested messages recurse.
func messageToMap(msg *dynamicpb.Message) map[string]any {
	out := map[string]any{}
	if msg == nil {
		return out
	}
	md := msg.Descriptor()
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		if !msg.Has(fd) {
			continue
		}
		key := lowerCamel(string(fd.Name()))
		v := msg.Get(fd)
		out[key] = protoToAny(fd, v)
	}
	return out
}

func protoToAny(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
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
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		// Stringified to preserve precision (matches schema mapping above).
		return strconv.FormatInt(v.Int(), 10)
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return v.Float()
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.BytesKind:
		return string(v.Bytes())
	case protoreflect.EnumKind:
		// Proto enums register with graphql-go as `Value: int32(number)`
		// (see types.go:enumFromDescriptor). graphql-go's enum
		// Serialize matches the resolver value against that config
		// value by typed equality, so we MUST return int32 here —
		// returning the name as a string yields null on the wire.
		return int32(v.Enum())
	case protoreflect.MessageKind, protoreflect.GroupKind:
		sub, ok := v.Message().Interface().(*dynamicpb.Message)
		if !ok {
			return nil
		}
		return messageToMap(sub)
	}
	return nil
}
