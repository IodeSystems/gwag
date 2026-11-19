// Command mcpstdio is a minimal MCP server speaking JSON-RPC over
// stdin/stdout. It exposes the same two tools (echo, greet) as the
// in-process test server used in mcp_ingest_test.go, so the stdio
// transport can be exercised with a real subprocess.
//
// It is invoked by tests via `go run ./testdata/mcpstdio`.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func main() {
	s := mcpserver.NewMCPServer("test-mcp-stdio", "1.0.0",
		mcpserver.WithToolCapabilities(false),
	)
	s.AddTool(
		mcp.NewTool("echo",
			mcp.WithDescription("Echo the message back"),
			mcp.WithString("message", mcp.Required(), mcp.Description("Text to echo")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("echo: " + req.GetArguments()["message"].(string)), nil
		},
	)
	s.AddTool(
		mcp.NewTool("greet",
			mcp.WithDescription("Greet someone in a tone"),
			mcp.WithString("name", mcp.Required()),
			mcp.WithString("tone", mcp.Enum("formal", "casual")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			tone, _ := args["tone"].(string)
			return mcp.NewToolResultText(fmt.Sprintf("greet(%v, tone=%q)", args["name"], tone)), nil
		},
	)
	srv := mcpserver.NewStdioServer(s)
	if err := srv.Listen(context.Background(), os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}
