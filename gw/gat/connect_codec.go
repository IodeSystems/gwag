package gat

// The binary proto codec gat mounts its Connect handlers with.
//
// connect's stock codec calls proto.Marshal with default options,
// which runs checkInitialized over the whole message to find unset
// required fields. proto3 has no required fields, and every message
// gat serves is proto3 — synthesized from IR, or compiled from a
// proto3 source. On a dynamicpb message that check has no generated
// fast path, so it walks the tree reflectively and, on a 25-row
// response, accounts for roughly an eighth of the request's
// allocations while being unable to find anything.
//
// AllowPartial skips it. Everything else is the stock behaviour.

import (
	"fmt"

	"google.golang.org/protobuf/proto"
)

// protoCodecName is the content subtype connect maps to this codec:
// application/proto, application/grpc+proto, application/grpc-web+proto.
// Registering under the stock name replaces the stock codec.
const protoCodecName = "proto"

// fastProtoCodec implements connect.Codec. It routes a planned
// response through the direct wire emitter and falls back to
// proto.Marshal for everything else.
type fastProtoCodec struct{}

func (fastProtoCodec) Name() string { return protoCodecName }

func (fastProtoCodec) Marshal(msg any) ([]byte, error) {
	// Fast path: a planned response encodes straight to wire bytes, so
	// no dynamicpb message is ever built for it.
	if r, ok := msg.(*wireResult); ok {
		if out, handled, err := r.appendProto(nil); handled {
			return out, err
		}
	}
	pm, ok := msg.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("gat: %T does not implement proto.Message", msg)
	}
	// AllowPartial: proto3 has no required fields, so the initialization
	// check can only ever succeed. Skipping it avoids a reflective walk
	// of the entire message on every response.
	return proto.MarshalOptions{AllowPartial: true}.Marshal(pm)
}

func (fastProtoCodec) Unmarshal(data []byte, msg any) error {
	pm, ok := msg.(proto.Message)
	if !ok {
		return fmt.Errorf("gat: %T does not implement proto.Message", msg)
	}
	// Merge=false matches the stock codec: connect hands over a freshly
	// initialized message per request, and reusing one across requests
	// would leak state between callers.
	return proto.UnmarshalOptions{AllowPartial: true, Merge: false}.Unmarshal(data, pm)
}
