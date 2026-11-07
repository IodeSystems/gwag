//go:build e2e

package harness

import (
	"context"
	"net/http"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCP returns an initialized MCP client for the gateway's /mcp
// (Streamable HTTP, admin bearer). It is closed when the test ends.
func (g *Gateway) MCP(t testing.TB) *mcpclient.Client {
	t.Helper()
	ctx := Ctx(t, defaultCallTimeout)
	c, err := mcpclient.NewStreamableHttpClient(g.BaseURL()+"/mcp",
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + g.Token()}))
	if err != nil {
		t.Fatalf("mcp client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Start(ctx); err != nil {
		t.Fatalf("mcp start: %v", err)
	}
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "gwag-e2e", Version: "0"},
	}}); err != nil {
		t.Fatalf("mcp initialize: %v", err)
	}
	return c
}

// MCPInclude adds a path glob (e.g. "orders.**") to the gateway's MCP
// allowlist.
func (a *Admin) MCPInclude(ctx context.Context, glob string) error {
	return a.Do(ctx, http.MethodPost, "/admin/mcp/include", map[string]string{"path": glob}, nil)
}
