package gateway

import (
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/internal/protocodec"
)

// protoCodec is gwag's GraphQL-canonical <-> dynamicpb conversion.
// The walker is shared with gat (gw/internal/protocodec); these
// options are where gwag differs:
//
// Unmaterialised *Upload values on bytes fields are silently skipped:
// the proto dispatcher's materializeUploadArgs replaces them with
// []byte before argsToMessage runs; the canonical-chain wrapper
// (synth-message path used by openapi/graphql ingest with runtime
// middleware) has no upload store and shouldn't fail when an Upload
// arg flows through — preserving the *Upload in the args map lets the
// inner dispatcher consume it. The materialised []byte is why bytes
// fields take []byte as well as string.
var protoCodec = protocodec.Options{
	SkipArg: func(v any) bool {
		_, isUpload := v.(*Upload)
		return isUpload
	},
	BytesFromSlice: true,
}

// fieldInfosFor returns md's fields with their cached GraphQL arg keys.
func fieldInfosFor(md protoreflect.MessageDescriptor) []protocodec.FieldInfo {
	return protocodec.FieldInfos(md)
}

// argsToMessage walks `args` (graphql-decoded JSON-ish: map[string]any,
// []any, primitives) and writes them into `msg` according to its
// descriptor. Unknown args are ignored; type mismatches return errors.
func argsToMessage(args map[string]any, msg *dynamicpb.Message) error {
	return protoCodec.ArgsToMessage(args, msg)
}

func setField(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, v any) error {
	return protoCodec.SetField(msg, fd, v)
}

// messageToMap converts a dynamicpb.Message to map[string]any for
// graphql to serialise. Unset fields are omitted; lists become []any;
// nested messages recurse.
func messageToMap(msg *dynamicpb.Message) map[string]any {
	return protocodec.MessageToMap(msg)
}

func protoToAny(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	return protocodec.ValueToAny(fd, v)
}
