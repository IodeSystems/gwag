package gat

import (
	"fmt"
	"strings"
)

// RegisterHTTP mounts gat's HTTP endpoints on a plain mux under
// prefix. It is the standalone counterpart to RegisterHuma /
// RegisterGRPC — for adopters who already have a plain *http.ServeMux
// (or any mux satisfying HandleMux) and don't want huma in the
// picture.
//
// Endpoints:
//
//	POST {prefix}/graphql           GraphQL queries + mutations
//	GET  {prefix}/schema/graphql    SDL (or ?format=json introspection)
//	GET  {prefix}/schema/proto      FileDescriptorSet (binary)
//	GET  {prefix}/schema/openapi    Re-emitted OpenAPI document
//	GET  {prefix}/_gat/subscribe    SSE pubsub stream (?channel=PATTERN)
//	POST {prefix}/_gat/publish      peer-mesh receive (only when EnablePeerMesh)
//
// Must be called after the gateway is built — either New(regs...) or
// RegisterHuma. Call EnablePeerMesh before RegisterHTTP for the
// peer-publish endpoint to be mounted. Returns an error if the schema
// isn't ready.
//
// Stability: experimental
func RegisterHTTP(mux HandleMux, g *Gateway, prefix string) error {
	if !g.built {
		return fmt.Errorf("gat: RegisterHTTP requires a built gateway (call New(regs...) or RegisterHuma first)")
	}
	prefix = strings.TrimRight(prefix, "/")
	mux.Handle(prefix+"/graphql", g.Handler())
	mux.Handle(prefix+"/schema/graphql", schemaGraphQLHandler(g))
	mux.Handle(prefix+"/schema/proto", schemaProtoHandler(g))
	mux.Handle(prefix+"/schema/openapi", schemaOpenAPIHandler(g))
	mux.Handle(prefix+SubscribePath, g.subscribeSSEHandler())
	if g.mesh != nil {
		mux.Handle(prefix+PeerPublishPath, g.peerPublishHandler())
	}
	return nil
}
