package gatbench_test

// Resolving gat's Connect procedure path and input descriptor.
//
// gat derives proto service and method names from the huma operation
// during ingest. Rather than hardcode that naming rule here (and go
// stale when it changes), pull the FileDescriptorSet gat publishes at
// /api/schema/proto and find the method by operation id — the same
// thing a `buf`-based client would do.
//
// The descriptor comes back too, so the binary-codec benchmarks can
// build a real request message without a generated Go type.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// gatMethod returns the mounted procedure path and the method
// descriptor for an operation id.
func gatMethod(tb testing.TB, h http.Handler, operationID string) (string, protoreflect.MethodDescriptor) {
	tb.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/schema/proto", nil))
	if rec.Code != http.StatusOK {
		tb.Fatalf("GET /api/schema/proto: %d %s", rec.Code, rec.Body.String())
	}
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(rec.Body.Bytes(), fds); err != nil {
		tb.Fatalf("unmarshal FDS: %v", err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		tb.Fatalf("NewFiles: %v", err)
	}
	var (
		procedure string
		method    protoreflect.MethodDescriptor
	)
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			svc := svcs.Get(i)
			methods := svc.Methods()
			for j := 0; j < methods.Len(); j++ {
				m := methods.Get(j)
				if strings.EqualFold(string(m.Name()), operationID) {
					procedure = "/api/grpc/" + string(svc.FullName()) + "/" + string(m.Name())
					method = m
					return false
				}
			}
		}
		return true
	})
	if procedure == "" {
		tb.Fatalf("no proto method for operation %q", operationID)
	}
	return procedure, method
}

func gatConnectProcedure(tb testing.TB, h http.Handler, operationID string) string {
	tb.Helper()
	p, _ := gatMethod(tb, h, operationID)
	return p
}

// gatRequestBytes builds the wire-encoded request message for an
// operation, setting each named field to the supplied value. Only the
// scalar kinds this benchmark's inputs use are handled — a wider
// switch would be dead code.
func gatRequestBytes(tb testing.TB, md protoreflect.MethodDescriptor, fields map[string]any) []byte {
	tb.Helper()
	msg := dynamicpb.NewMessage(md.Input())
	for name, v := range fields {
		fd := md.Input().Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			tb.Fatalf("input %s has no field %q; has %s", md.Input().FullName(), name, fieldNames(md.Input()))
		}
		// Dispatch on the descriptor, not the Go value: gat picks the
		// proto integer width from the IR, so hardcoding int32 here
		// panics the moment an operation uses a 64-bit field.
		msg.Set(fd, protoValueFor(tb, fd, v))
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		tb.Fatalf("marshal request: %v", err)
	}
	return b
}

func fieldNames(md protoreflect.MessageDescriptor) string {
	var out []string
	for i := 0; i < md.Fields().Len(); i++ {
		out = append(out, string(md.Fields().Get(i).Name()))
	}
	return strings.Join(out, ", ")
}

// protoValueFor converts a Go test value to the protoreflect.Value the
// field's declared kind requires.
func protoValueFor(tb testing.TB, fd protoreflect.FieldDescriptor, v any) protoreflect.Value {
	tb.Helper()
	switch fd.Kind() {
	case protoreflect.StringKind:
		s, ok := v.(string)
		if !ok {
			tb.Fatalf("field %s is string-kinded, got %T", fd.Name(), v)
		}
		return protoreflect.ValueOfString(s)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(int32(asInt(tb, fd, v)))
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(asInt(tb, fd, v))
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(uint32(asInt(tb, fd, v)))
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(uint64(asInt(tb, fd, v)))
	case protoreflect.BoolKind:
		b, ok := v.(bool)
		if !ok {
			tb.Fatalf("field %s is bool-kinded, got %T", fd.Name(), v)
		}
		return protoreflect.ValueOfBool(b)
	}
	tb.Fatalf("protoValueFor: unhandled kind %s for field %s", fd.Kind(), fd.Name())
	return protoreflect.Value{}
}

func asInt(tb testing.TB, fd protoreflect.FieldDescriptor, v any) int64 {
	tb.Helper()
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	}
	tb.Fatalf("field %s is numeric, got %T", fd.Name(), v)
	return 0
}
