package gateway

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// InternalProtoHandler is the in-process dispatch entry point for one
// unary method on an internal-proto service. The dispatcher hands the
// handler a *dynamicpb.Message populated from canonical args (or from
// the gRPC ingress' wire-decoded request) and expects a
// protoreflect.ProtoMessage back of the method's declared output type
// — concrete generated type or *dynamicpb.Message either works; the
// dispatcher adapts both.
//
// Plain Go function, no I/O assumed. The gateway runs the user
// runtime middleware chain around it but no backpressure (there's no
// upstream replica to gate against).
type InternalProtoHandler func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error)

// InternalProtoSubscriptionHandler is the in-process dispatch entry
// point for one server-streaming method on an internal-proto service.
// The handler receives the GraphQL-canonical args map (same shape the
// unary side would see post-argsToMessage round-trip would produce on
// the canonical names) and returns a `chan any` — graphql-go's
// Subscribe field receives that channel and pumps each frame as
// rp.Source. For ps.sub the handler joins the gateway's subscription
// broker; future internal-proto subscription primitives reuse this
// shape.
//
// The handler owns its release lifetime: typically a goroutine
// listening on ctx.Done() that closes the broker handle.
type InternalProtoSubscriptionHandler func(ctx context.Context, args map[string]any) (any, error)

// internalProtoSource is the per-slot dispatch handle for the
// internal-proto kind — the in-process analogue of `*pool`. Shape
// mirrors `*pool` for IR / SDL / MCP / admin-listing parity (same
// FileDescriptor, same hash, same raw source bytes for `/schema/proto`
// re-emit) minus the replica + sem machinery, since dispatch is a
// direct Go call.
type internalProtoSource struct {
	namespace string
	version   string
	versionN  int

	file      protoreflect.FileDescriptor
	hash      [32]byte
	rawSource []byte

	// handlers map unary RPC method names (matching FileDescriptor.
	// Services() walk: "Pub", ...) to their in-process Go callbacks.
	// Keyed by wire-level PascalCase method name so the slot's IR-
	// ingested op names line up with the lookup site.
	handlers map[string]InternalProtoHandler

	// subscriptionHandlers map server-streaming RPC method names
	// (e.g. "Sub") to their subscription-flavored Go callbacks. A
	// streaming method without an entry here registers no Subscription
	// dispatcher and resolves to "no dispatcher for ..." at request
	// time — same posture as a missing unary handler.
	subscriptionHandlers map[string]InternalProtoSubscriptionHandler
}

// addInternalProtoSlotLocked installs the in-process proto service at
// (ns, ver) with the supplied handlers. Boot-time only — internal-
// proto slots are gateway-defined; there is no control-plane or
// cluster propagation path (the gateway's own bindings live in every
// process).
//
// Tier policy (unstable swap, vN immutability, cross-kind reject) is
// centralized in `registerSlotLocked`. maxConcurrency caps don't
// apply (no upstream); passes 0/0 so the slot index treats every
// internal-proto add as cap-compatible.
//
// Caller holds g.mu.
func (g *Gateway) addInternalProtoSlotLocked(ns, ver string, fd protoreflect.FileDescriptor, rawSource []byte, handlers map[string]InternalProtoHandler, subscriptionHandlers map[string]InternalProtoSubscriptionHandler) error {
	if err := validateNS(ns); err != nil {
		return fmt.Errorf("internalproto: %w", err)
	}
	if fd == nil {
		return fmt.Errorf("internalproto: %s/%s: file descriptor is nil", ns, ver)
	}
	if len(handlers) == 0 && len(subscriptionHandlers) == 0 {
		return fmt.Errorf("internalproto: %s/%s: handlers and subscriptionHandlers maps are both empty", ns, ver)
	}
	canonicalVer, verN, err := parseVersion(ver)
	if err != nil {
		return fmt.Errorf("internalproto: %w", err)
	}
	hash, err := hashFromFileDescriptor(fd)
	if err != nil {
		return fmt.Errorf("internalproto: hash %s/%s: %w", ns, canonicalVer, err)
	}
	// Internal-proto slots are gateway-bundled code; bypass --allow-tier
	// policy (it gates user registrations only).
	key := poolKey{namespace: ns, version: canonicalVer}
	bindings := extractChannelBindings(fd)
	existed, err := g.registerSlotLockedSkipTierCheck(slotKindInternalProto, key, hash, 0, 0, bindings)
	if err != nil {
		return fmt.Errorf("internalproto: %w", err)
	}
	if existed {
		return nil
	}
	s := g.slots[key]
	src := &internalProtoSource{
		namespace:            ns,
		version:              canonicalVer,
		versionN:             verN,
		file:                 fd,
		hash:                 hash,
		rawSource:            append([]byte(nil), rawSource...),
		handlers:             copyInternalProtoHandlers(handlers),
		subscriptionHandlers: copyInternalProtoSubscriptionHandlers(subscriptionHandlers),
	}
	s.internalProto = src
	g.bakeSlotIRLocked(s)
	g.advanceStableLocked(ns, verN)
	if g.schema.Load() != nil {
		return g.assembleLocked()
	}
	return nil
}

func copyInternalProtoHandlers(in map[string]InternalProtoHandler) map[string]InternalProtoHandler {
	out := make(map[string]InternalProtoHandler, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyInternalProtoSubscriptionHandlers(in map[string]InternalProtoSubscriptionHandler) map[string]InternalProtoSubscriptionHandler {
	out := make(map[string]InternalProtoSubscriptionHandler, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
