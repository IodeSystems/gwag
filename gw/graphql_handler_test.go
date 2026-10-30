package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func serveGraphQL(h http.Handler, method, target, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// The GraphQL handler reads every body format it read before moving
// onto gat.
func TestGraphQLHandler_BodyFormats(t *testing.T) {
	h, calls := limitsGateway(t)
	const q = `{ test { getThing(id:"x") { id } } }`
	want := `{"data":{"test":{"getThing":{"id":"abc"}}}}`
	cases := []struct{ name, contentType, body string }{
		{"json", "application/json", `{"query":` + strconvQuote(q) + `}`},
		{"string variables", "application/json", `{"query":"query($id: String!) { test { getThing(id: $id) { id } } }","variables":"{\"id\":\"x\"}"}`},
		{"application/graphql", "application/graphql", q},
		{"form", "application/x-www-form-urlencoded", url.Values{"query": {q}}.Encode()},
	}
	for _, c := range cases {
		if rr := serveGraphQL(h, http.MethodPost, "/graphql", c.contentType, c.body); rr.Code != http.StatusOK || rr.Body.String() != want {
			t.Errorf("%s: %d %s", c.name, rr.Code, rr.Body)
		}
	}
	if rr := serveGraphQL(h, http.MethodGet, "/graphql?query="+url.QueryEscape(q), "", ""); rr.Body.String() != want {
		t.Errorf("GET: %d %s", rr.Code, rr.Body)
	}
	if n := calls.Load(); n != int32(len(cases)+1) {
		t.Errorf("backend calls = %d, want %d", n, len(cases)+1)
	}
}

// Changed in the move onto gat: a body that is not JSON, or carries no
// query, is refused with 400 (was 200 with a GraphQL error).
func TestGraphQLHandler_BadRequests(t *testing.T) {
	h, calls := limitsGateway(t)
	for name, body := range map[string]string{
		"malformed JSON":  `{"query":`,
		"empty query":     `{"query":""}`,
		"no query":        `{}`,
		"bad variables":   `{"query":"{ __typename }","variables":"{"}`,
		"array variables": `{"query":"{ __typename }","variables":[1]}`,
	} {
		if rr := serveGraphQL(h, http.MethodPost, "/graphql", "application/json", body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rr.Code, rr.Body)
		}
	}
	if rr := serveGraphQL(h, http.MethodGet, "/graphql?query={__typename}&variables={", "", ""); rr.Code != http.StatusBadRequest {
		t.Errorf("GET with bad variables: %d %s", rr.Code, rr.Body)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("refused requests dispatched %d calls", n)
	}
}

// Added in the move onto gat: a JSON array body is a batch.
func TestGraphQLHandler_Batch(t *testing.T) {
	h, calls := limitsGateway(t, WithMaxOperations(2))
	rr := serveGraphQL(h, http.MethodPost, "/graphql", "application/json",
		`[{"query":"{ test { getThing(id:\"x\") { id } } }"},{"query":"{ __typename }"}]`)
	var out []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || len(out) != 2 || out[0]["errors"] != nil || out[1]["errors"] != nil {
		t.Fatalf("batch: %d %s", rr.Code, rr.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("backend calls = %d, want 1", calls.Load())
	}
	// WithMaxOperations also caps a batch's length.
	rr = serveGraphQL(h, http.MethodPost, "/graphql", "application/json",
		`[{"query":"{ __typename }"},{"query":"{ __typename }"},{"query":"{ __typename }"}]`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("batch over the cap: %d %s", rr.Code, rr.Body)
	}
}

// WithMaxOperations caps a batch's operations summed over its
// documents: over the cap, the whole batch is refused and none runs.
func TestGraphQLHandler_BatchOperationCap(t *testing.T) {
	h, calls := limitsGateway(t, WithMaxOperations(3))
	two := `{"query":"{ test { a: getThing(id:\"x\") { id } b: getThing(id:\"y\") { id } } }"}`
	rr := serveGraphQL(h, http.MethodPost, "/graphql", "application/json", "["+two+","+two+"]")
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "over the limit of 3") {
		t.Fatalf("batch of 4 operations: %d %s", rr.Code, rr.Body)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("refused batch dispatched %d calls", n)
	}
	rr = serveGraphQL(h, http.MethodPost, "/graphql", "application/json", "["+two+`,{"query":"{ __typename }"}]`)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "errors") {
		t.Fatalf("batch at the cap: %d %s", rr.Code, rr.Body)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("backend calls = %d, want 2", n)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
