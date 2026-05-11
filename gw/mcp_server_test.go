package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// TestMCPHandler_RoundTrip drives the Streamable HTTP transport end-
// to-end via the mark3labs/mcp-go client: initialize → tools/list →
// tools/call(schema_list) → tools/call(query). Pins:
//   - all four tools register and show up in list
//   - schema_list result deserializes back into []SchemaListEntry
//   - query tool runs in-process and returns the v1 events-none bundle
func TestMCPHandler_RoundTrip(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(be.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("things")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	gw.mu.Lock()
	if err := gw.assembleLocked(); err != nil {
		gw.mu.Unlock()
		t.Fatalf("assembleLocked: %v", err)
	}
	gw.mu.Unlock()
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{Include: []string{"**"}}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", gw.MCPHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := mcpclient.NewStreamableHttpClient(srv.URL + "/mcp")
	if err != nil {
		t.Fatalf("NewStreamableHttpClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}

	initRes, err := client.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "test", Version: "0.0.0"},
		},
	})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if initRes.ServerInfo.Name != "go-api-gateway" {
		t.Errorf("server name=%q, want go-api-gateway", initRes.ServerInfo.Name)
	}

	tools, err := client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"schema_list", "schema_search", "schema_expand", "query"} {
		if !names[want] {
			t.Errorf("missing tool %q in list: %v", want, tools.Tools)
		}
	}

	// schema_list: empty args, structured result decodes back into
	// []SchemaListEntry via the JSON fallback (every Streamable HTTP
	// transport delivers content[].text alongside structuredContent).
	listRes, err := client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "schema_list"},
	})
	if err != nil {
		t.Fatalf("CallTool(schema_list): %v", err)
	}
	if listRes.IsError {
		t.Fatalf("schema_list returned IsError: %+v", listRes.Content)
	}
	var listEntries []SchemaListEntry
	if err := decodeToolJSON(listRes, &listEntries); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listEntries) != 2 {
		t.Errorf("schema_list entries=%d, want 2: %+v", len(listEntries), listEntries)
	}

	// query: in-process GraphQL exec. Verify the v1 events-none bundle.
	qRes, err := client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "query",
			Arguments: map[string]any{"query": "{ __typename }"},
		},
	})
	if err != nil {
		t.Fatalf("CallTool(query): %v", err)
	}
	if qRes.IsError {
		t.Fatalf("query returned IsError: %+v", qRes.Content)
	}
	var wrapped MCPResponseWithEvents
	if err := decodeToolJSON(qRes, &wrapped); err != nil {
		t.Fatalf("decode query: %v", err)
	}
	if wrapped.Events.Level != "none" {
		t.Errorf("events.level=%q want none", wrapped.Events.Level)
	}
	if wrapped.Events.Channels == nil || len(wrapped.Events.Channels) != 0 {
		t.Errorf("events.channels=%+v want empty slice", wrapped.Events.Channels)
	}
}

// decodeToolJSON pulls the text content off a CallToolResult and
// unmarshals it into `v`. mcp-go's NewToolResultStructured produces
// both structured + text fallback content; we read the text path so
// the assertion is transport-agnostic.
func decodeToolJSON(res *mcp.CallToolResult, v any) error {
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return json.Unmarshal([]byte(tc.Text), v)
		}
	}
	return jsonNoTextError(res)
}

func jsonNoTextError(res *mcp.CallToolResult) error {
	parts := []string{}
	for _, c := range res.Content {
		parts = append(parts, "(" + describeContent(c) + ")")
	}
	return &mcpError{msg: "no TextContent in tool result; got " + strings.Join(parts, ", ")}
}

type mcpError struct{ msg string }

func (e *mcpError) Error() string { return e.msg }

func describeContent(c mcp.Content) string {
	switch v := c.(type) {
	case mcp.TextContent:
		return "text"
	case mcp.ImageContent:
		return "image"
	default:
		_ = v
		return "unknown"
	}
}
