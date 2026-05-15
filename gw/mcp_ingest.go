package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCPTransport selects how AddMCP connects to a downstream MCP server.
//
// Stability: stable
type MCPTransport string

// MCP transport constants.
//
// Stability: stable
const (
	// MCPStdio spawns the target as a subprocess and speaks JSON-RPC
	// over its stdin/stdout.
	MCPStdio MCPTransport = "stdio"
	// MCPHTTP connects over the MCP Streamable HTTP transport.
	MCPHTTP MCPTransport = "http"
	// MCPSSE connects over the legacy MCP HTTP+SSE transport.
	MCPSSE MCPTransport = "sse"
)

// mcpSource is the per-slot handle for an ingested downstream MCP
// server. It owns the live client — a subprocess for stdio, an HTTP
// connection otherwise — for the gateway's lifetime; Gateway.Close()
// closes it. MCP has no replica concept, so unlike proto / openapi /
// graphql sources there is exactly one client per slot.
type mcpSource struct {
	namespace string
	version   string
	versionN  int
	transport MCPTransport
	target    string

	// client is the live mcp-go client. rawTools is the tools/list
	// response JSON captured at boot — bakeSlotIRLocked feeds it to
	// ir.IngestMCP, and it's the schema-equality material behind hash.
	client   *client.Client
	rawTools []byte
	hash     [32]byte
}

// AddMCP ingests a downstream MCP server's tools as GraphQL Mutations.
// The gateway connects to the server at boot, runs `tools/list`, and
// registers one Mutation per tool under the chosen namespace; a
// `tools/call` round-trip dispatches each call at request time.
//
//	gw.AddMCP(gateway.MCPStdio,
//	    "npx -y @modelcontextprotocol/server-filesystem /tmp",
//	    gateway.As("files"))
//	gw.AddMCP(gateway.MCPHTTP, "https://mcp.example.com",
//	    gateway.As("weather"))
//
// `transport` is one of MCPStdio / MCPHTTP / MCPSSE. For stdio,
// `target` is the command line (split on whitespace into argv); for
// http / sse it's the server URL. An unreachable server at boot is a
// registration error — same posture as AddGraphQL's introspection.
//
// Tools only: MCP resources and prompts are not ingested. Tool
// results are semi-structured, so every Mutation returns the JSON
// scalar carrying the `tools/call` result.
//
// Stability: stable
func (g *Gateway) AddMCP(transport MCPTransport, target string, opts ...ServiceOption) error {
	sc := &serviceConfig{}
	for _, o := range opts {
		o(sc)
	}
	if target == "" {
		return fmt.Errorf("gateway: AddMCP: target is required")
	}

	cl, err := newMCPClient(transport, target)
	if err != nil {
		return fmt.Errorf("gateway: AddMCP(%s): %w", target, err)
	}
	ctx := context.Background()
	if err := cl.Start(ctx); err != nil {
		_ = cl.Close()
		return fmt.Errorf("gateway: AddMCP(%s): start transport: %w", target, err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "gwag", Version: "1.0.0"}
	if _, err := cl.Initialize(ctx, initReq); err != nil {
		_ = cl.Close()
		return fmt.Errorf("gateway: AddMCP(%s): initialize: %w", target, err)
	}
	listRes, err := cl.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		_ = cl.Close()
		return fmt.Errorf("gateway: AddMCP(%s): tools/list: %w", target, err)
	}
	rawTools, err := json.Marshal(struct {
		Tools []mcp.Tool `json:"tools"`
	}{Tools: listRes.Tools})
	if err != nil {
		_ = cl.Close()
		return fmt.Errorf("gateway: AddMCP(%s): encode tools: %w", target, err)
	}

	ns := sc.namespace
	if ns == "" {
		ns = "mcp"
	}
	if err := validateNS(ns); err != nil {
		_ = cl.Close()
		return fmt.Errorf("gateway: AddMCP: %w", err)
	}
	ver, verN, err := parseVersion(sc.version)
	if err != nil {
		_ = cl.Close()
		return fmt.Errorf("gateway: AddMCP: %w", err)
	}
	hash := sha256.Sum256(rawTools)

	g.mu.Lock()
	defer g.mu.Unlock()
	if sc.internal {
		g.internal[ns] = true
	}
	key := poolKey{namespace: ns, version: ver}
	existed, err := g.registerSlotLocked(slotKindMCP, key, hash, 0, 0, nil)
	if err != nil {
		_ = cl.Close()
		return fmt.Errorf("gateway: AddMCP: %w", err)
	}
	if existed {
		// Identical re-registration (same tools → same hash). MCP has
		// no replica fan-out, so the slot's existing client stands;
		// close the duplicate we just opened.
		_ = cl.Close()
		return nil
	}
	s := g.slots[key]
	s.mcp = &mcpSource{
		namespace: ns,
		version:   ver,
		versionN:  verN,
		transport: transport,
		target:    target,
		client:    cl,
		rawTools:  rawTools,
		hash:      hash,
	}
	g.bakeSlotIRLocked(s)
	g.advanceStableLocked(ns, verN)
	if g.schema.Load() != nil {
		return g.assembleLocked()
	}
	return nil
}

// newMCPClient constructs an mcp-go client for the given transport.
// stdio splits `target` on whitespace into command + argv; http / sse
// treat it as the server URL. The returned client is not yet started.
func newMCPClient(transport MCPTransport, target string) (*client.Client, error) {
	switch transport {
	case MCPStdio:
		fields := strings.Fields(target)
		if len(fields) == 0 {
			return nil, fmt.Errorf("stdio transport: empty command")
		}
		return client.NewStdioMCPClient(fields[0], nil, fields[1:]...)
	case MCPHTTP:
		return client.NewStreamableHttpClient(target)
	case MCPSSE:
		return client.NewSSEMCPClient(target)
	default:
		return nil, fmt.Errorf("unknown MCP transport %q (want stdio, http, or sse)", transport)
	}
}

// closeMCPClientsLocked closes every ingested MCP server's client.
// Caller holds g.mu; the actual Close calls happen after unlock to
// avoid holding the lock across a subprocess teardown.
func (g *Gateway) closeMCPClientsLocked() []*client.Client {
	var out []*client.Client
	for _, s := range g.slots {
		if s.kind == slotKindMCP && s.mcp != nil && s.mcp.client != nil {
			out = append(out, s.mcp.client)
		}
	}
	return out
}
