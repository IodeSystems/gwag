package protocodec

// Nested lists and maps over gRPC. proto has no list of lists and no
// map of maps, so ir.RenderProtoFiles boxes each inner list in a
// `<Field>Row { repeated T value = 1; }` message (ir.IsListRow) and
// each inner map in a `<Field>Value { map<K,V> value = 1; }` message
// (ir.IsMapValueBox). The handlers behind a gRPC route speak plain
// JSON-shaped values, where an inner list or map is itself. Box and
// Unbox convert between the two, guided by the message descriptor.

import (
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

var hasBoxesCache sync.Map // protoreflect.MessageDescriptor → bool

// HasBoxes reports whether md, or any message reachable from it,
// has a list-row or map-value box field. Cached per descriptor, so callers can check
// it on every request and skip the walks when it is false.
func HasBoxes(md protoreflect.MessageDescriptor) bool {
	if md == nil {
		return false
	}
	if v, ok := hasBoxesCache.Load(md); ok {
		return v.(bool)
	}
	has := hasBoxes(md, map[protoreflect.FullName]bool{})
	hasBoxesCache.Store(md, has)
	return has
}

func hasBoxes(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) bool {
	if seen[md.FullName()] {
		return false
	}
	seen[md.FullName()] = true
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		sub := fieldMessage(fields.Get(i))
		if sub == nil {
			continue
		}
		if isBox(sub) || hasBoxes(sub, seen) {
			return true
		}
	}
	return false
}

func isBox(md protoreflect.MessageDescriptor) bool {
	return ir.IsListRow(md) || ir.IsMapValueBox(md)
}

// fieldMessage is the message a field holds: its own message type, or
// a map field's value type. Nil for scalars.
func fieldMessage(fd protoreflect.FieldDescriptor) protoreflect.MessageDescriptor {
	if fd.IsMap() {
		return fd.MapValue().Message()
	}
	return fd.Message()
}

// Box rewrites a JSON-shaped object for message md: each inner list of
// a nested list becomes `{"value": [...]}` and each inner map of a map
// of maps becomes `{"value": {...}}`, so protojson or ArgsToMessage
// can decode it. v is changed in place where it is a
// map; the result is returned for the top-level call.
func Box(md protoreflect.MessageDescriptor, v any) any {
	return rewriteObject(md, v, true)
}

// Unbox is the reverse of Box: each `{"value": ...}` box becomes its
// plain list or map.
func Unbox(md protoreflect.MessageDescriptor, v any) any {
	return rewriteObject(md, v, false)
}

func rewriteObject(md protoreflect.MessageDescriptor, v any, box bool) any {
	m, ok := v.(map[string]any)
	if !ok || md == nil {
		return v
	}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fieldMessage(fd) == nil {
			continue
		}
		// The key may be the proto name or the JSON name, depending on
		// who built the map.
		for _, key := range []string{string(fd.Name()), fd.JSONName()} {
			if fv, ok := m[key]; ok {
				m[key] = rewriteField(fd, fv, box)
				break
			}
		}
	}
	return m
}

func rewriteField(fd protoreflect.FieldDescriptor, v any, box bool) any {
	switch {
	case fd.IsMap():
		if m, ok := v.(map[string]any); ok {
			for k, e := range m {
				m[k] = rewriteValue(fd.MapValue().Message(), e, box)
			}
		}
		return v
	case fd.IsList():
		if l, ok := v.([]any); ok {
			for i, e := range l {
				l[i] = rewriteValue(fd.Message(), e, box)
			}
		}
		return v
	}
	return rewriteValue(fd.Message(), v, box)
}

// rewriteValue converts one value of message type md.
func rewriteValue(md protoreflect.MessageDescriptor, v any, box bool) any {
	if md == nil || !isBox(md) {
		return rewriteObject(md, v, box)
	}
	inner := md.Fields().Get(0)
	if box {
		// Only the shape the box holds is wrapped; anything else is
		// left for the decoder to reject.
		if _, isMap := v.(map[string]any); isMap == inner.IsMap() {
			if _, isList := v.([]any); isList || isMap {
				return map[string]any{"value": rewriteField(inner, v, box)}
			}
		}
		return v
	}
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	w, ok := m["value"]
	if !ok || w == nil {
		if inner.IsMap() {
			return map[string]any{}
		}
		return []any{}
	}
	return rewriteField(inner, w, box)
}
