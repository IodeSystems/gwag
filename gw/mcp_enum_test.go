package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mark3labs/mcp-go/server/servertest"
)

// An MCP string-enum argument sends the enum value, not its index.
func TestEnumRename_MCP(t *testing.T) {
	f := newCompatMCPEnum(t, []string{"a-b", "ok"})
	_, body := compatPost(t, f.h, `mutation { m { probe(c: a_b) } }`, "")
	if strings.Contains(body, `"errors"`) {
		t.Fatalf("probe: %s", body)
	}
	f.mu.Lock()
	args := f.args
	f.mu.Unlock()
	if args != `{"c":"a-b"}` {
		t.Errorf("tool args %s, want {\"c\":\"a-b\"}", args)
	}
}

func newCompatMCPEnum(t *testing.T, values []string) *compatMCP {
	t.Helper()
	f := &compatMCP{}
	s := mcpserver.NewMCPServer("enum-mcp", "1.0.0", mcpserver.WithToolCapabilities(false))
	s.AddTool(
		mcp.NewTool("probe", mcp.WithString("c", mcp.Enum(values...))),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			b, _ := json.Marshal(req.GetArguments())
			f.mu.Lock()
			f.args = string(b)
			f.mu.Unlock()
			return mcp.NewToolResultText("ok"), nil
		},
	)
	srv := servertest.NewTestStreamableHTTPServer(s)
	t.Cleanup(srv.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)
	if err := gw.AddMCP(MCPHTTP, srv.URL, As("m")); err != nil {
		t.Fatalf("AddMCP: %v", err)
	}
	f.gw = gw
	f.h = gw.Handler()
	return f
}
