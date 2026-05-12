package gateway

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/gwag/gw/ir"
)

// channelBindingExtensionFullName is the proto full name of the
// `(gwag.ps.binding)` extension defined in
// `gw/proto/ps/v1/options.proto`. We compare by name rather than by
// the generated `psv1.E_Binding` extension type because protocompile
// resolves options against its own descriptor pool — the option's
// value comes back as `*dynamicpb.Message`, not the concrete
// `*psv1.ChannelBinding`, so `proto.GetExtension` would panic on the
// type assertion. The protoreflect Range over MessageOptions sees the
// same field regardless of which pool the descriptor came from.
const channelBindingExtensionFullName = "gwag.ps.v1.binding"

// channelBindingPatternFieldName is the field on `ChannelBinding`
// whose value we extract.
const channelBindingPatternFieldName = "pattern"

// extractChannelBindings walks every message reachable from `fd`
// (top-level + nested, but not imported files — bindings travel with
// their declaring file's slot) and returns the `(gwag.ps.binding)`
// extensions stamped on them. Pattern is the literal NATS-style
// pattern the option declared; MessageFQN is the message's proto
// full name.
//
// Messages without the option, and proto files that don't import
// `gw/proto/ps/v1/options.proto`, yield no entries. Idempotent across
// rebakes — the IR slice is rebuilt from scratch each call.
func extractChannelBindings(fd protoreflect.FileDescriptor) []ir.ChannelBinding {
	if fd == nil {
		return nil
	}
	var out []ir.ChannelBinding
	walkMessagesForBindings(fd.Messages(), &out)
	return out
}

func walkMessagesForBindings(ms protoreflect.MessageDescriptors, out *[]ir.ChannelBinding) {
	for i := 0; i < ms.Len(); i++ {
		md := ms.Get(i)
		if pattern := bindingPatternOnMessage(md); pattern != "" {
			*out = append(*out, ir.ChannelBinding{
				Pattern:    pattern,
				MessageFQN: string(md.FullName()),
			})
		}
		walkMessagesForBindings(md.Messages(), out)
	}
}

// bindingPatternOnMessage returns the pattern string on the
// `(gwag.ps.binding)` extension if it's set on md's MessageOptions,
// or "" if no binding is declared. Works against both dynamicpb
// option values (protocompile) and the generated concrete type
// (boot-time Go-registered extension).
func bindingPatternOnMessage(md protoreflect.MessageDescriptor) string {
	opts := md.Options()
	if opts == nil {
		return ""
	}
	var pattern string
	opts.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if !fd.IsExtension() || string(fd.FullName()) != channelBindingExtensionFullName {
			return true
		}
		bindingMsg := v.Message()
		patternFD := bindingMsg.Descriptor().Fields().ByName(channelBindingPatternFieldName)
		if patternFD == nil {
			return true
		}
		pattern = bindingMsg.Get(patternFD).String()
		return false
	})
	return pattern
}
