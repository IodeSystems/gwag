package gat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/IodeSystems/graphql-go/v2"
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
	g.trusted = &trustedDocs{
		docs:    docs,
		enforce: enforce,
		plans:   make(map[trustedKey]graphql.PlanResult, len(docs)),
	}
	return g
}

// ParseTrustedDocuments reads a persisted-documents manifest: a JSON
// object of document id → document text, as graphql-codegen's client
// preset writes it.
//
// Stability: experimental
func ParseTrustedDocuments(data []byte) (map[string]string, error) {
	var docs map[string]string
	if err := json.Unmarshal(data, &docs); err != nil {
		return nil, fmt.Errorf("gat: trusted documents manifest: %w", err)
	}
	for id, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			return nil, fmt.Errorf("gat: trusted documents manifest: document %q is empty", id)
		}
	}
	return docs, nil
}

type trustedDocs struct {
	docs    map[string]string
	enforce bool

	mu    sync.RWMutex
	plans map[trustedKey]graphql.PlanResult
}

type trustedKey struct{ id, operationName string }

// lookup returns the document for id, trying it as sent and then
// without an algorithm prefix, so "sha256:abc" and "abc" match either
// manifest spelling.
func (t *trustedDocs) lookup(id string) (string, string, bool) {
	if doc, ok := t.docs[id]; ok {
		return id, doc, true
	}
	if _, bare, ok := strings.Cut(id, ":"); ok {
		if doc, ok := t.docs[bare]; ok {
			return bare, doc, true
		}
	}
	return "", "", false
}

// plan returns the cached plan for (id, operationName), building it
// through the gateway's plan cache on first use. The cache is bounded:
// operationName comes from the client, so past a few entries per
// document new keys are planned but not kept.
func (t *trustedDocs) plan(g *Gateway, id, doc, operationName string) graphql.PlanResult {
	key := trustedKey{id, operationName}
	t.mu.RLock()
	pr, ok := t.plans[key]
	t.mu.RUnlock()
	if ok {
		return pr
	}
	pr = g.planCache.Get(g.schema, doc, operationName)
	t.mu.Lock()
	if len(t.plans) < 4*len(t.docs)+16 {
		t.plans[key] = pr
	}
	t.mu.Unlock()
	return pr
}

// persistedID is the document id a request names, from documentId or
// the Apollo APQ extension; "" when it names none.
func persistedID(documentID string, extensions map[string]any) string {
	if documentID != "" {
		return documentID
	}
	pq, _ := extensions["persistedQuery"].(map[string]any)
	if h, _ := pq["sha256Hash"].(string); h != "" {
		return "sha256:" + h
	}
	return ""
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
