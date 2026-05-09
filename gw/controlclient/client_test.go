package controlclient

import (
	"context"
	"strings"
	"testing"

	greeterv1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/greeter/v1"
)

// BuildTag forcing function (plan §4): a release-tagged binary must
// not claim trunk's `unstable` slot. SelfRegister is the lint site
// because every controlclient caller goes through it; failing here
// short-circuits before any gRPC dial.

func TestSelfRegister_BuildTag_RejectsUnstable(t *testing.T) {
	_, err := SelfRegister(context.Background(), Options{
		GatewayAddr: "ignored:0",
		ServiceAddr: "ignored:0",
		BuildTag:    "v1.2.3",
		Services: []Service{{
			Namespace:      "greeter",
			Version:        "unstable",
			FileDescriptor: greeterv1.File_greeter_proto,
		}},
	})
	if err == nil {
		t.Fatalf("expected reject when BuildTag is set + Version=unstable; got nil")
	}
	if !strings.Contains(err.Error(), "BuildTag") {
		t.Errorf("error %q should name the BuildTag forcing function", err.Error())
	}
	if !strings.Contains(err.Error(), "unstable") {
		t.Errorf("error %q should name the rejected version", err.Error())
	}
	if !strings.Contains(err.Error(), "vN") {
		t.Errorf("error %q should hint to use vN", err.Error())
	}
}

// Empty BuildTag means "trunk binary" — the lint is off and unstable
// is fine. Ensures the gate is opt-in, not always-on.
func TestSelfRegister_NoBuildTag_AllowsUnstable(t *testing.T) {
	// Use an unreachable address so the gRPC dial fails fast; we just
	// need to verify the BuildTag check passed (i.e. the dial got
	// reached, not short-circuited by the lint). grpc.NewClient is
	// lazy, so the failure surfaces when the Register RPC is attempted.
	_, err := SelfRegister(context.Background(), Options{
		GatewayAddr: "127.0.0.1:1",
		ServiceAddr: "ignored:0",
		Services: []Service{{
			Namespace:      "greeter",
			Version:        "unstable",
			FileDescriptor: greeterv1.File_greeter_proto,
		}},
	})
	if err == nil {
		// We expect a dial-time error, not a lint error. The exact
		// shape varies by environment; we just want to be SURE the
		// error isn't the BuildTag lint.
		t.Fatalf("expected dial-time error, got nil")
	}
	if strings.Contains(err.Error(), "BuildTag") {
		t.Errorf("BuildTag lint fired with empty tag: %v", err)
	}
}

// BuildTag set but Version is a numbered cut — the lint should be
// happy and only the dial-time error should surface.
func TestSelfRegister_BuildTag_AllowsVN(t *testing.T) {
	_, err := SelfRegister(context.Background(), Options{
		GatewayAddr: "127.0.0.1:1",
		ServiceAddr: "ignored:0",
		BuildTag:    "v1.2.3",
		Services: []Service{{
			Namespace:      "greeter",
			Version:        "v3",
			FileDescriptor: greeterv1.File_greeter_proto,
		}},
	})
	if err == nil {
		t.Fatalf("expected dial-time error, got nil")
	}
	if strings.Contains(err.Error(), "BuildTag") {
		t.Errorf("BuildTag lint fired for vN: %v", err)
	}
}
