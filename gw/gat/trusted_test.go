package gat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

const getDoc = `query GetProject($id: String!) { Demo { getProject(id: $id) { name } } }`

func newTrustedGat(t *testing.T, enforce bool) *httptest.Server {
	t.Helper()
	docs, err := gat.ParseTrustedDocuments([]byte(`{"abc123": ` + mustJSON(t, getDoc) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t).TrustedDocuments(docs, enforce)
	gat.Register(api, g, huma.Operation{OperationID: "getProject", Method: http.MethodGet, Path: "/projects/{id}"},
		func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
			return &getProjectOutput{Body: project{ID: in.ID, Name: "Project " + in.ID}}, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// postJSON returns the status and decoded body of a GraphQL POST.
func postJSON(t *testing.T, url string, payload map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(payload)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, decodeBody(t, resp.Body)
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, decodeBody(t, resp.Body)
}

func decodeBody(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func errorCode(resp map[string]any) any {
	errs, _ := resp["errors"].([]any)
	if len(errs) == 0 {
		return nil
	}
	return digPath(errs[0], "extensions", "code")
}

func TestTrustedDocuments_Allow(t *testing.T) {
	srv := newTrustedGat(t, false)
	gql := srv.URL + "/api/graphql"
	vars := map[string]any{"id": "p1"}

	for name, payload := range map[string]map[string]any{
		"documentId":         {"documentId": "abc123", "variables": vars},
		"prefixed id":        {"documentId": "sha256:abc123", "variables": vars},
		"apq extension":      {"extensions": map[string]any{"persistedQuery": map[string]any{"version": 1, "sha256Hash": "abc123"}}, "variables": vars},
		"plain query":        {"query": getDoc, "variables": vars},
		"apq miss + query":   {"query": getDoc, "variables": vars, "extensions": map[string]any{"persistedQuery": map[string]any{"sha256Hash": "nope"}}},
		"repeat (plan kept)": {"documentId": "abc123", "variables": vars},
	} {
		status, resp := postJSON(t, gql, payload)
		if status != http.StatusOK || digPath(resp, "data", "Demo", "getProject", "name") != "Project p1" {
			t.Errorf("%s: status %d resp %v", name, status, resp)
		}
	}

	status, resp := postJSON(t, gql, map[string]any{"documentId": "missing"})
	if status != http.StatusOK || errorCode(resp) != "PERSISTED_QUERY_NOT_FOUND" {
		t.Errorf("unknown id: status %d resp %v", status, resp)
	}
}

func TestTrustedDocuments_Enforce(t *testing.T) {
	srv := newTrustedGat(t, true)
	gql := srv.URL + "/api/graphql"

	status, resp := postJSON(t, gql, map[string]any{"query": getDoc, "variables": map[string]any{"id": "p1"}})
	if status != http.StatusBadRequest || errorCode(resp) != "TRUSTED_DOCUMENT_REQUIRED" {
		t.Errorf("raw query under enforce: status %d resp %v", status, resp)
	}
	status, resp = postJSON(t, gql, map[string]any{"query": getDoc, "extensions": map[string]any{"persistedQuery": map[string]any{"sha256Hash": "nope"}}})
	if status != http.StatusOK || errorCode(resp) != "PERSISTED_QUERY_NOT_FOUND" {
		t.Errorf("apq miss + query under enforce must not run the query: status %d resp %v", status, resp)
	}

	// GET with documentId + variables: the CDN-cacheable form.
	q := url.Values{"documentId": {"abc123"}, "variables": {`{"id":"p2"}`}}
	status, resp = getJSON(t, gql+"?"+q.Encode())
	if status != http.StatusOK || digPath(resp, "data", "Demo", "getProject", "name") != "Project p2" {
		t.Errorf("GET documentId: status %d resp %v", status, resp)
	}
}

func TestTrustedDocuments_NotConfigured(t *testing.T) {
	srv, _ := newLimitsGat(t, func(*gat.Gateway) {})
	status, resp := postJSON(t, srv.URL+"/api/graphql", map[string]any{"documentId": "abc123"})
	if status != http.StatusBadRequest || errorCode(resp) != "PERSISTED_QUERY_NOT_SUPPORTED" {
		t.Errorf("documentId without TrustedDocuments: status %d resp %v", status, resp)
	}
}

func TestGETVariables(t *testing.T) {
	srv, _ := newLimitsGat(t, func(*gat.Gateway) {})
	q := url.Values{
		"query":     {`query Q($id: String!) { Demo { opA(id: $id) { id } } }`},
		"variables": {`{"id":"v1"}`},
	}
	status, resp := getJSON(t, srv.URL+"/api/graphql?"+q.Encode())
	if status != http.StatusOK || digPath(resp, "data", "Demo", "opA", "id") != "v1" {
		t.Errorf("GET with variables: status %d resp %v", status, resp)
	}
	status, _ = getJSON(t, srv.URL+"/api/graphql?"+url.Values{"query": {"{ x }"}, "variables": {"{not json"}}.Encode())
	if status != http.StatusBadRequest {
		t.Errorf("bad variables JSON: status %d, want 400", status)
	}
}

func TestParseTrustedDocuments(t *testing.T) {
	if _, err := gat.ParseTrustedDocuments([]byte(`[]`)); err == nil {
		t.Error("array manifest: want an error")
	}
	if _, err := gat.ParseTrustedDocuments([]byte(`{"a": "  "}`)); err == nil {
		t.Error("empty document: want an error")
	}
}

// BenchmarkTrustedDocument compares naming a document by id with
// sending its text.
func BenchmarkTrustedDocument(b *testing.B) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g, _ := gat.New()
	g.TrustedDocuments(map[string]string{"abc123": getDoc}, false)
	gat.Register(api, g, huma.Operation{OperationID: "getProject", Method: http.MethodGet, Path: "/projects/{id}"},
		func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
			return &getProjectOutput{Body: project{ID: in.ID}}, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		b.Fatal(err)
	}
	bodies := map[string][]byte{}
	bodies["query"], _ = json.Marshal(map[string]any{"query": getDoc, "variables": map[string]any{"id": "p1"}})
	bodies["documentId"], _ = json.Marshal(map[string]any{"documentId": "abc123", "variables": map[string]any{"id": "p1"}})
	for _, name := range []string{"query", "documentId"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				req := httptest.NewRequest(http.MethodPost, "/api/graphql", bytes.NewReader(bodies[name]))
				req.Header.Set("Content-Type", "application/json")
				mux.ServeHTTP(httptest.NewRecorder(), req)
			}
		})
	}
}
