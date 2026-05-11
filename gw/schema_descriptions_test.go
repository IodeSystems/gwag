package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSchemaSDL_PathBasedAddProtoSurfacesDescriptions covers the
// end-to-end MCP-search-corpus story for proto-source services:
// path-based AddProto preserves comments through ingest → IR →
// runtime schema → SDL, AND the subscription auto-doc gets appended
// at bake time. So the rendered SDL carries both the adopter's
// hand-written comment and the gateway's HMAC-channel-auth contract
// on every server-streaming op.
//
// Why this test specifically: the multi example (and most adopter
// services) registers via SelfRegister with protoc-gen-go-stripped
// descriptors, which loses comments unconditionally — so the only
// way to demonstrate the doc-corpus path is via AddProto(path).
func TestSchemaSDL_PathBasedAddProtoSurfacesDescriptions(t *testing.T) {
	gw := newSchemaTestGateway(t)
	if err := gw.AddProto(
		"../examples/multi/protos/greeter.proto",
		To(nopGRPCConn{}),
		As("greeter"),
	); err != nil {
		t.Fatalf("AddProto: %v", err)
	}

	srv := httptest.NewServer(gw.SchemaHandler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "?service=greeter")
	if err != nil {
		t.Fatalf("GET schema: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	sdl := string(body)

	// Adopter-written comment from greeter.proto on Greetings rpc.
	if !strings.Contains(sdl, "Server-streaming") {
		t.Errorf("SDL missing original Greetings comment\n--- SDL ---\n%s", sdl)
	}
	// Gateway's auto-injected HMAC contract on the same subscription.
	if !strings.Contains(sdl, "HMAC channel token") {
		t.Errorf("SDL missing subscription auto-doc\n--- SDL ---\n%s", sdl)
	}
	// Adopter's field-level comment from GreetingsFilter.name.
	if !strings.Contains(sdl, "Filter by recipient") {
		t.Errorf("SDL missing GreetingsFilter.name comment\n--- SDL ---\n%s", sdl)
	}
}
