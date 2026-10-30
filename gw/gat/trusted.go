package gat

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/iodesystems/gwag/v2/gw/internal/gqlguard"
)

// TrustedDocuments registers the GraphQL documents this gateway's
// clients were built with, keyed by document id — the manifest
// graphql-codegen's client preset writes with `persistedDocuments`
// (see ParseTrustedDocuments). A request then names a document instead
// of sending it:
//
//	{"documentId": "<id>", "variables": {...}}
//	{"extensions": {"persistedQuery": {"sha256Hash": "<id>"}}, ...}
//	GET {prefix}/graphql?documentId=<id>&variables=<json>
//
// A known id skips reading, parsing and validating a query string: its
// plan is kept per (id, operationName). Ids may carry an algorithm
// prefix ("sha256:<hash>") on either side.
//
// enforce=false accepts both ids and full queries. enforce=true accepts
// ids only: a request carrying query text is refused with 400, which
// closes the endpoint to arbitrary queries. Unknown ids answer
// PERSISTED_QUERY_NOT_FOUND either way.
//
// Call before serving. Returns g for chaining.
//
// Stability: experimental
func (g *Gateway) TrustedDocuments(docs map[string]string, enforce bool) *Gateway {
	g.trusted = gqlguard.NewTrustedDocs(docs, enforce)
	return g
}

// ParseTrustedDocuments reads a persisted-documents manifest: a JSON
// object of document id → document text, as graphql-codegen's client
// preset writes it.
//
// Stability: experimental
func ParseTrustedDocuments(data []byte) (map[string]string, error) {
	docs, err := gqlguard.ParseManifest(data)
	if err != nil {
		return nil, fmt.Errorf("gat: %w", err)
	}
	return docs, nil
}

// writeCodedError writes a GraphQL errors envelope carrying
// extensions.code, the shape clients key persisted-query handling on.
func writeCodedError(w http.ResponseWriter, msg, code string, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]any{{"message": msg, "extensions": map[string]any{"code": code}}},
	})
}
