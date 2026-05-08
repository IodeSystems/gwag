package gateway

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	authv1 "github.com/iodesystems/go-api-gateway/examples/auth/gen/auth/v1"
	userv1 "github.com/iodesystems/go-api-gateway/examples/auth/gen/user/v1"
)

// TestInjectType_PointerProtoTRewrite confirms that the canonical
// proto-pointer T (e.g. *authv1.Context) produces a HideTypeRewrite
// keyed on the proto FullName and a non-nil Runtime.
func TestInjectType_PointerProtoTRewrite(t *testing.T) {
	tx := InjectType[*authv1.Context](func(_ context.Context, _ **authv1.Context) (*authv1.Context, error) {
		return &authv1.Context{}, nil
	})

	if got, want := len(tx.Schema), 1; got != want {
		t.Fatalf("len(Schema)=%d, want %d", got, want)
	}
	h, ok := tx.Schema[0].(HideTypeRewrite)
	if !ok {
		t.Fatalf("Schema[0] is %T, want HideTypeRewrite", tx.Schema[0])
	}
	if h.Name != "auth.v1.Context" {
		t.Fatalf("HideTypeRewrite.Name=%q, want %q", h.Name, "auth.v1.Context")
	}
	if tx.Runtime == nil {
		t.Fatalf("Runtime=nil; want non-nil for proto-message T")
	}
}

// TestInjectType_ValueProtoTRewrite confirms the value-shape T
// (authv1.Context, no pointer) resolves to the same proto FullName.
func TestInjectType_ValueProtoTRewrite(t *testing.T) {
	tx := InjectType[authv1.Context](func(_ context.Context, _ *authv1.Context) (authv1.Context, error) {
		return authv1.Context{}, nil
	})

	if got, want := len(tx.Schema), 1; got != want {
		t.Fatalf("len(Schema)=%d, want %d", got, want)
	}
	h := tx.Schema[0].(HideTypeRewrite)
	if h.Name != "auth.v1.Context" {
		t.Fatalf("HideTypeRewrite.Name=%q, want %q", h.Name, "auth.v1.Context")
	}
}

// TestInjectType_HideFalseSkipsSchemaRewrite confirms Hide(false)
// keeps the arg in the schema (no HideTypeRewrite emitted) but still
// wires runtime injection so the resolver runs and can inspect what
// the caller sent.
func TestInjectType_HideFalseSkipsSchemaRewrite(t *testing.T) {
	tx := InjectType[*authv1.Context](func(_ context.Context, _ **authv1.Context) (*authv1.Context, error) {
		return &authv1.Context{}, nil
	}, Hide(false))

	if got := len(tx.Schema); got != 0 {
		t.Fatalf("len(Schema)=%d, want 0 under Hide(false)", got)
	}
	if tx.Runtime == nil {
		t.Fatalf("Runtime=nil; want non-nil for proto-message T")
	}
}

// TestInjectType_NonProtoTSkipsRuntime confirms a Go-native T that
// isn't a proto message produces a schema rewrite (so the field is
// stripped from the SDL if it ever existed) and a nil Runtime
// (runtime injection is proto-shaped today).
func TestInjectType_NonProtoTSkipsRuntime(t *testing.T) {
	tx := InjectType[string](func(_ context.Context, _ *string) (string, error) {
		return "filled", nil
	})

	if got := len(tx.Schema); got != 1 {
		t.Fatalf("len(Schema)=%d, want 1", got)
	}
	if tx.Runtime != nil {
		t.Fatalf("Runtime non-nil; want nil for non-proto T")
	}
}

// TestInjectType_RuntimeFillsHiddenField runs the InjectType runtime
// middleware against a synthesized GetMeRequest dynamic message and
// confirms the auth field gets populated by the resolver.
func TestInjectType_RuntimeFillsHiddenField(t *testing.T) {
	tx := InjectType[*authv1.Context](func(_ context.Context, _ **authv1.Context) (*authv1.Context, error) {
		return &authv1.Context{UserId: "u_resolved", TenantId: "t_resolved"}, nil
	})
	if tx.Runtime == nil {
		t.Fatal("Runtime is nil")
	}

	getMeDesc := (&userv1.GetMeRequest{}).ProtoReflect().Descriptor()
	dyn := dynamicpb.NewMessage(getMeDesc)

	ctx := withInjectCache(context.Background())

	called := false
	chain := tx.Runtime(func(_ context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
		called = true
		return req, nil
	})
	out, err := chain(ctx, dyn)
	if err != nil {
		t.Fatalf("middleware: %v", err)
	}
	if !called {
		t.Fatal("next handler not called")
	}
	outDyn := out.(*dynamicpb.Message)

	authFD := getMeDesc.Fields().ByName("auth")
	if authFD == nil {
		t.Fatal("auth field not found on GetMeRequest descriptor")
	}
	if !outDyn.Has(authFD) {
		t.Fatal("auth field not set after middleware ran")
	}

	subDyn := outDyn.Get(authFD).Message().Interface()
	b, err := proto.Marshal(subDyn)
	if err != nil {
		t.Fatalf("marshal sub: %v", err)
	}
	var got authv1.Context
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal sub: %v", err)
	}
	if got.GetUserId() != "u_resolved" {
		t.Fatalf("UserId=%q, want %q", got.GetUserId(), "u_resolved")
	}
	if got.GetTenantId() != "t_resolved" {
		t.Fatalf("TenantId=%q, want %q", got.GetTenantId(), "t_resolved")
	}
}
