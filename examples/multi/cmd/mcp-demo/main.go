// Worked example for the gateway's MCP integration (plan §2). Drives
// the gateway's MCP Streamable HTTP transport end-to-end against the
// running examples/multi stack:
//
//  1. read the admin boot token from <admin-data-dir>/admin-token
//  2. curate the MCP allowlist via /api/admin/mcp/* (default-deny →
//     include `greeter.**` and `library.**`)
//  3. open an MCP client at /api/mcp with `Authorization: Bearer <hex>`
//  4. tools/list → schema_list → schema_search → schema_expand → query
//
// Run alongside `./run.sh` (which persists the token to
// /tmp/gwag-multi/admin-token by default). The demo prints every step
// so you can see exactly how an agent would chain the tools.
//
//	$ cd examples/multi && ./run.sh   # terminal 1
//	$ cd examples/multi && go run ./cmd/mcp-demo   # terminal 2
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

func main() {
	gatewayURL := flag.String("gateway", "http://localhost:8080", "Gateway base URL")
	tokenFile := flag.String("admin-token-file", "/tmp/gwag-multi/admin-token", "Path to the admin token (hex). run.sh writes this for you.")
	tokenHex := flag.String("admin-token", "", "Hex admin token. Overrides --admin-token-file when set.")
	flag.Parse()

	token, err := resolveToken(*tokenFile, *tokenHex)
	if err != nil {
		log.Fatalf("admin token: %v", err)
	}
	authz := "Bearer " + token
	log.Printf("using admin token from %s (%d hex chars)", strings.TrimSpace(*tokenFile), len(token))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	step(1, "curate the MCP allowlist (default-deny → include greeter.** + library.**)")
	if err := adminInclude(ctx, *gatewayURL, authz, "greeter.**"); err != nil {
		log.Fatalf("include greeter.**: %v", err)
	}
	if err := adminInclude(ctx, *gatewayURL, authz, "library.**"); err != nil {
		log.Fatalf("include library.**: %v", err)
	}
	cfg, err := adminListMCP(ctx, *gatewayURL)
	if err != nil {
		log.Fatalf("list mcp config: %v", err)
	}
	fmt.Printf("  → MCPConfig: %s\n", cfg)

	step(2, "open MCP client at "+*gatewayURL+"/api/mcp")
	client, err := mcpclient.NewStreamableHttpClient(*gatewayURL+"/api/mcp",
		transport.WithHTTPHeaders(map[string]string{"Authorization": authz}),
	)
	if err != nil {
		log.Fatalf("NewStreamableHttpClient: %v", err)
	}
	defer client.Close()
	if err := client.Start(ctx); err != nil {
		log.Fatalf("client.Start: %v", err)
	}

	initRes, err := client.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "mcp-demo", Version: "0.0.1"},
		},
	})
	if err != nil {
		log.Fatalf("Initialize: %v", err)
	}
	fmt.Printf("  → server: %s %s\n", initRes.ServerInfo.Name, initRes.ServerInfo.Version)

	step(3, "tools/list")
	tools, err := client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		log.Fatalf("ListTools: %v", err)
	}
	for _, tl := range tools.Tools {
		fmt.Printf("  → %s — %s\n", tl.Name, firstSentence(tl.Description))
	}

	step(4, "schema_list (every op the allowlist exposes)")
	listRes, err := client.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "schema_list"}})
	if err != nil {
		log.Fatalf("schema_list: %v", err)
	}
	printToolJSON(listRes)

	step(5, "schema_search regex=\"hello\" — find every op whose name/args/doc mention 'hello'")
	searchRes, err := client.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "schema_search",
		Arguments: map[string]any{
			"regex": "hello",
		},
	}})
	if err != nil {
		log.Fatalf("schema_search: %v", err)
	}
	printToolJSON(searchRes)

	step(6, "schema_expand greeter.hello — full structured contract + transitive type closure")
	expandRes, err := client.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name:      "schema_expand",
		Arguments: map[string]any{"name": "greeter.hello"},
	}})
	if err != nil {
		log.Fatalf("schema_expand: %v", err)
	}
	printToolJSON(expandRes)

	step(7, "query — execute a GraphQL operation in-process")
	qRes, err := client.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "query",
		Arguments: map[string]any{
			"query": `{ greeter { hello(name: "mcp-demo") { greeting } } library { listBooks(author: "") { books { title author year } } } }`,
		},
	}})
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	printToolJSON(qRes)
}

func step(n int, msg string) {
	fmt.Printf("\n--- step %d — %s ---\n", n, msg)
}

func resolveToken(path, literal string) (string, error) {
	if literal != "" {
		return strings.TrimSpace(literal), nil
	}
	abs, _ := filepath.Abs(path)
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read %s: %w (start ./run.sh first, or pass --admin-token)", abs, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func adminInclude(ctx context.Context, gateway, authz, path string) error {
	body, _ := json.Marshal(map[string]string{"path": path})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway+"/api/admin/mcp/include", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authz)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("status %d: %s", res.StatusCode, b)
	}
	fmt.Printf("  → POST /api/admin/mcp/include path=%s ok\n", path)
	return nil
}

func adminListMCP(ctx context.Context, gateway string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway+"/api/admin/mcp", nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		return "", fmt.Errorf("status %d: %s", res.StatusCode, b)
	}
	return string(bytes.TrimSpace(b)), nil
}

// printToolJSON writes the textual content of an MCP CallToolResult to
// stdout. mark3labs/mcp-go's NewToolResultStructured emits a Text
// payload alongside structuredContent, so we just print whichever the
// transport delivered.
func printToolJSON(res *mcp.CallToolResult) {
	if res.IsError {
		fmt.Printf("  ! tool returned IsError: %+v\n", res.Content)
		return
	}
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			indented := indentJSON(tc.Text)
			fmt.Println(indented)
			return
		}
	}
	fmt.Printf("  (no text content; got %d parts)\n", len(res.Content))
}

func indentJSON(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	out, err := json.MarshalIndent(v, "  ", "  ")
	if err != nil {
		return raw
	}
	return "  " + string(out)
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, ".\n"); i > 0 {
		return s[:i+1]
	}
	return s
}
