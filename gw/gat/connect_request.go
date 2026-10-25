package gat

// The value connect decodes a Connect/gRPC request into.
//
// The mirror of wireResult. connect's request type parameter is
// unconstrained, so the binary codec can be handed something other
// than a proto message: wireRequest carries the operation's decode
// plan, and fastProtoCodec.Unmarshal decodes the wire bytes straight
// into the handler's input struct (connect_decode.go). No dynamicpb
// message is built.
//
// Every other codec still needs a real proto message — protojson for
// the JSON codec — so wireRequest implements proto.Message and hands
// out a dynamicpb message on demand, which the handler then binds the
// way it always did. So does an operation whose decode plan declined
// to form, or one whose input checks need the request as args.

import (
	"fmt"
	"reflect"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// requestDecoder is one operation's wire fast path.
type requestDecoder struct {
	inputType reflect.Type
	plan      *decodePlan
}

// newRequestDecoder plans the wire fast path for an operation, or
// returns nil when it keeps the dynamicpb path. It rides on the bind
// plan, so it forms only where that does, and declines further shapes
// of its own (see connect_decode.go).
func newRequestDecoder(inputType reflect.Type, irOp *ir.Operation, md protoreflect.MessageDescriptor) *requestDecoder {
	bp := buildBindPlan(inputType, irOp, bodyArgOf(irOp), md)
	plan := buildDecodePlan(inputType, bp, md)
	if plan == nil {
		return nil
	}
	return &requestDecoder{inputType: inputType, plan: plan}
}

// decode parses data into a fresh *I.
func (d *requestDecoder) decode(data []byte) (any, error) {
	inPtr := reflect.New(d.inputType)
	if err := d.plan.decode(data, inPtr.Elem(), protowire.DefaultRecursionLimit); err != nil {
		return nil, err
	}
	return inPtr.Interface(), nil
}

type wireRequest struct {
	md  protoreflect.MessageDescriptor
	dec *requestDecoder // nil: no fast path for this operation

	// in is the decoded *I, set when the fast path ran.
	in any

	// msg is the dynamicpb form, built only when something asks for a
	// proto message: a non-binary codec, or an operation without a
	// decode plan.
	msg *dynamicpb.Message
}

// newRequestInitializer returns the connect.WithRequestInitializer
// that gives each request its descriptor and the operation's decoder.
// dec may be nil.
func newRequestInitializer(dec *requestDecoder) func(connect.Spec, any) error {
	return func(spec connect.Spec, msg any) error {
		r, ok := msg.(*wireRequest)
		if !ok {
			return nil
		}
		md, ok := spec.Schema.(protoreflect.MethodDescriptor)
		if !ok {
			return fmt.Errorf("gat: spec.Schema is %T, want protoreflect.MethodDescriptor", spec.Schema)
		}
		r.md = md.Input()
		r.dec = dec
		return nil
	}
}

// unmarshalProto is the binary codec's entry point.
func (r *wireRequest) unmarshalProto(data []byte) error {
	if r.dec != nil {
		in, err := r.dec.decode(data)
		if err != nil {
			return err
		}
		r.in = in
		return nil
	}
	// Merge=false matches the stock codec, as in fastProtoCodec.
	return proto.UnmarshalOptions{AllowPartial: true, Merge: false}.Unmarshal(data, r.message())
}

// message returns the request as a dynamicpb message. After the fast
// path ran it is empty — the bytes went into r.in and are gone — so
// callers check r.in first.
func (r *wireRequest) message() *dynamicpb.Message {
	if r.msg == nil {
		r.msg = dynamicpb.NewMessage(r.md)
	}
	return r.msg
}

// ProtoReflect satisfies proto.Message, for protojson and any other
// codec that decodes into a real message.
func (r *wireRequest) ProtoReflect() protoreflect.Message {
	return r.message()
}

// bindRequest returns the handler input for r: the fast path's result
// when it ran, otherwise bind over the dynamicpb message.
func bindRequest(r *wireRequest, bind func(*dynamicpb.Message) (any, error)) (any, error) {
	if r.in != nil {
		return r.in, nil
	}
	return bind(r.message())
}
