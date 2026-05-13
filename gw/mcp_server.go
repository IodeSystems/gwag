package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// MCPHandler returns an http.Handler that speaks the MCP Streamable
// HTTP transport, exposing the four MCP tools (schema_list /
// schema_search / schema_expand / query) wired to the gateway's
// in-process MCPSchemaList / MCPSchemaSearch / MCPSchemaExpand /
// MCPQuery methods. Stateless: every request creates a fresh session
// — the surface is queryable side-effect-free, so the operator
// doesn't need session pinning.
//
// Mount with whatever path makes sense — the example gateway uses
// `/api/mcp` (see plan §2). Operators should gate the mount behind
// the admin bearer (or whichever auth posture they prefer); the
// underlying gateway methods don't authenticate.
func (g *Gateway) MCPHandler() http.Handler {
	srv := server.NewMCPServer(
		"go-api-gateway",
		"1.0.0",
		server.WithToolCapabilities(false),
	)
	g.registerMCPTools(srv)
	return server.NewStreamableHTTPServer(srv,
		server.WithStateLess(true),
	)
}

// registerMCPTools attaches the four tool adapters to the MCP server.
// Each adapter pulls args off the CallToolRequest, calls the
// matching gateway method, and wraps the result in
// NewToolResultStructured so MCP-aware clients see JSON and
// text-only clients see the JSON-stringified fallback.
func (g *Gateway) registerMCPTools(srv *server.MCPServer) {
	srv.AddTool(mcp.NewTool("schema_list",
		mcp.WithDescription("List every operation exposed via the MCP surface, grouped by Query / Mutation / Subscription. Returns entries with the dot-segmented path, kind, namespace, version, and a short description. Use this for orientation; pair with schema_search and schema_expand to drill in."),
	), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		entries := g.mcpSchemaList()
		return mcpResultJSON(entries)
	})

	srv.AddTool(mcp.NewTool("schema_search",
		mcp.WithDescription("Filter the MCP-allowed operation surface. pathGlob is dot-segmented ('admin.*', '*.list*', 'things.**'); regex is matched against op name, every arg name, and the description body. Both filters AND-combine; either may be empty (an empty search returns every allowed op)."),
		mcp.WithString("pathGlob",
			mcp.Description("Dot-segmented glob over the qualified path. '*' matches one segment; '**' matches zero or more segments."),
		),
		mcp.WithString("regex",
			mcp.Description("Regular expression matched against op name, every arg name, and the description body. Invalid regex surfaces as a tool error."),
		),
	), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		entries, err := g.mcpSchemaSearch(schemaSearchInput{
			PathGlob: req.GetString("pathGlob", ""),
			Regex:    req.GetString("regex", ""),
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcpResultJSON(entries)
	})

	srv.AddTool(mcp.NewTool("schema_expand",
		mcp.WithDescription("Return the structured definition of one op path ('things.getThing') or type name ('Thing'), plus every type transitively reachable from it (args + return types for an op; fields + variants for a type). Op-by-name is resolved first; type-by-name is the fallback."),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Either a dot-segmented op path ('things.getThing') or a type name ('Thing')."),
		),
	), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, err := req.RequireString("name")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		res, err := g.mcpSchemaExpand(name)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcpResultJSON(res)
	})

	srv.AddTool(mcp.NewTool("query",
		mcp.WithDescription("Execute a GraphQL operation against the gateway in-process. The result is wrapped in ResponseWithEvents { response, events: {level, channels[]} } — events.level is always 'none' in v1; v2 subscription stitching will layer in additively. Variables passes through to the executor; operationName picks one operation when the document contains multiple. The MCP allowlist gates discovery, not execution — agents that bypass schema_list with hand-written queries still execute through the same channel."),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("The GraphQL query / mutation / subscription document."),
		),
		mcp.WithObject("variables",
			mcp.Description("Optional variable values keyed by GraphQL variable name."),
		),
		mcp.WithString("operationName",
			mcp.Description("Operation name to dispatch when the query document contains multiple operations."),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		args := req.GetArguments()
		var variables map[string]any
		if v, ok := args["variables"].(map[string]any); ok {
			variables = v
		}
		res, err := g.mcpQuery(ctx, mcpQueryInput{
			Query:         query,
			Variables:     variables,
			OperationName: req.GetString("operationName", ""),
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcpResultJSON(res)
	})
}

// mcpResultJSON wraps a structured value as a CallToolResult that
// MCP-aware clients render as JSON and text-only clients render
// from the marshaled string fallback.
func mcpResultJSON(v any) (*mcp.CallToolResult, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal result: %w", err)
	}
	return mcp.NewToolResultStructured(v, string(raw)), nil
}
