package gateway

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// InternalProtoHandler is the in-process dispatch entry point for one
// method on an internal-proto service. The dispatcher hands the
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

	// handlers map RPC method names (matching FileDescriptor.Services()
	// walk: "Pub", "Sub", ...) to their in-process Go callbacks. Keyed
	// by wire-level PascalCase method name so the slot's IR-ingested
	// op names line up with the lookup site.
	handlers map[string]InternalProtoHandler
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
func (g *Gateway) addInternalProtoSlotLocked(ns, ver string, fd protoreflect.FileDescriptor, rawSource []byte, handlers map[string]InternalProtoHandler) error {
	if err := validateNS(ns); err != nil {
		return fmt.Errorf("internalproto: %w", err)
	}
	if fd == nil {
		return fmt.Errorf("internalproto: %s/%s: file descriptor is nil", ns, ver)
	}
	if len(handlers) == 0 {
		return fmt.Errorf("internalproto: %s/%s: handlers map is empty", ns, ver)
	}
	canonicalVer, verN, err := parseVersion(ver)
	if err != nil {
		return fmt.Errorf("internalproto: %w", err)
	}
	hash, err := hashFromFileDescriptor(fd)
	if err != nil {
		return fmt.Errorf("internalproto: hash %s/%s: %w", ns, canonicalVer, err)
	}
	key := poolKey{namespace: ns, version: canonicalVer}
	existed, err := g.registerSlotLocked(slotKindInternalProto, key, hash, 0, 0)
	if err != nil {
		return fmt.Errorf("internalproto: %w", err)
	}
	if existed {
		return nil
	}
	s := g.slots[key]
	src := &internalProtoSource{
		namespace: ns,
		version:   canonicalVer,
		versionN:  verN,
		file:      fd,
		hash:      hash,
		rawSource: append([]byte(nil), rawSource...),
		handlers:  copyInternalProtoHandlers(handlers),
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
