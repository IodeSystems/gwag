package gat

// The value gat hands connect as a Connect/gRPC response.
//
// connect's response type parameter is unconstrained, which is what
// makes the direct wire emitter reachable: instead of handing over a
// built dynamicpb message, the handler hands over the Go value plus
// the plan for encoding it, and the binary codec writes wire bytes
// from that without the message ever existing.
//
// Every other consumer still needs a real proto message — protojson
// for the JSON codec, connect's own error and debug paths — so
// wireResult implements proto.Message and materialises a dynamicpb
// message on demand. The JSON path therefore costs exactly what it did
// before; only the binary path gets shorter.
//
// It also carries an already-built message, for operations whose
// encode plan declined to form. One response type keeps the handler
// signature single-valued regardless of which path an operation took.

import (
	"reflect"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

type wireResult struct {
	md protoreflect.MessageDescriptor

	// Fast path: encode straight from the Go value.
	plan *encodePlan
	val  reflect.Value

	// Fallback: an already-populated message (no plan for this op).
	// Also memoises the fast path's materialised message — consumers
	// call ProtoReflect more than once per response, and rebuilding it
	// each time cost the JSON codec a second full encode.
	built *dynamicpb.Message
}

// newPlannedResult carries the handler's value for direct encoding.
func newPlannedResult(md protoreflect.MessageDescriptor, plan *encodePlan, body any) *wireResult {
	return &wireResult{md: md, plan: plan, val: reflect.ValueOf(body)}
}

// newBuiltResult carries a message that has already been populated.
func newBuiltResult(md protoreflect.MessageDescriptor, msg *dynamicpb.Message) *wireResult {
	return &wireResult{md: md, built: msg}
}

// appendProto writes the response's wire encoding, or reports false
// when this result has no plan and the caller should fall back to
// proto.Marshal over the materialised message.
func (r *wireResult) appendProto(dst []byte) ([]byte, bool, error) {
	if r.plan == nil {
		return dst, false, nil
	}
	out, err := appendMessage(dst, r.plan, r.val)
	if err != nil {
		return dst, true, err
	}
	return out, true, nil
}

// ProtoReflect satisfies proto.Message. On the fast path this
// materialises the message the emitter exists to avoid, so it is only
// reached by consumers that genuinely need one — protojson, and
// connect's error formatting.
func (r *wireResult) ProtoReflect() protoreflect.Message {
	if r.built != nil {
		return r.built
	}
	msg := dynamicpb.NewMessage(r.md)
	if r.plan != nil && r.val.IsValid() {
		// An error here would already have surfaced on the binary path;
		// protojson will render whatever was written before it failed,
		// which beats returning a nil message to the codec.
		_ = r.plan.encode(r.val, msg)
	}
	// Safe to cache without a lock: a wireResult belongs to one
	// in-flight response and never outlives it.
	r.built = msg
	return msg
}
