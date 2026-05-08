package gateway

import (
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// messagePoolCache memoises one sync.Pool per descriptor identity. A
// dispatched RPC allocates two dynamicpb.Messages (request + response)
// today; pooling drops both back into a per-descriptor pool after
// they've been read out, so steady-state dispatch reuses the same
// underlying message header instead of re-allocating.
//
// Keyed by descriptor identity (the interface value), not FullName —
// see fieldInfoCache for the same reasoning. Two gateways in the same
// process can register the same .proto and hold distinct
// MessageDescriptor instances; dynamicpb refuses to mix them.
//
// proto.Reset is `m.ProtoReflect().Clear()` — safe to call once gRPC
// (or the JSON encoder) has finished marshaling. Callers must:
//   - put a message back EXACTLY ONCE per acquire (otherwise sync.Pool
//     hands the same pointer to two goroutines), and
//   - put back to the pool keyed on the same descriptor it was
//     acquired from (descriptor mismatch will panic on next use, since
//     dynamicpb's field accessors verify parent identity).
var messagePoolCache sync.Map // map[protoreflect.MessageDescriptor]*sync.Pool

func messagePoolFor(desc protoreflect.MessageDescriptor) *sync.Pool {
	if v, ok := messagePoolCache.Load(desc); ok {
		return v.(*sync.Pool)
	}
	p := &sync.Pool{
		New: func() any { return dynamicpb.NewMessage(desc) },
	}
	if v, loaded := messagePoolCache.LoadOrStore(desc, p); loaded {
		return v.(*sync.Pool)
	}
	return p
}

// acquireDynamicMessage returns a cleared dynamicpb.Message of the
// given descriptor. The caller must call releaseDynamicMessage
// exactly once when done.
func acquireDynamicMessage(desc protoreflect.MessageDescriptor) *dynamicpb.Message {
	return messagePoolFor(desc).Get().(*dynamicpb.Message)
}

// releaseDynamicMessage clears `m` and returns it to the per-
// descriptor pool. Pass the same descriptor that was used to acquire
// it — the pool is keyed there.
func releaseDynamicMessage(desc protoreflect.MessageDescriptor, m *dynamicpb.Message) {
	if m == nil {
		return
	}
	proto.Reset(m)
	messagePoolFor(desc).Put(m)
}
