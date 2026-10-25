package gat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

type cachedInput struct {
	ID string `path:"id"`
}

type cachedOutput struct {
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Vary         string `header:"Vary"`
	Body         project
}

// newCachedGat serves ops whose cache headers come from the id:
// "a" and "b" are public with different max-ages and ETags, "p" is
// private, "n" sets no headers (plain op), "x" fails.
func newCachedGat(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t)
	gat.Register(api, g, huma.Operation{OperationID: "cached", Method: http.MethodGet, Path: "/c/{id}"},
		func(ctx context.Context, in *cachedInput) (*cachedOutput, error) {
			out := &cachedOutput{Body: project{ID: in.ID}}
			switch in.ID {
			case "a":
				out.CacheControl, out.ETag, out.Vary = "public, max-age=60", `"ea"`, "Accept-Language"
			case "a2":
				out.CacheControl, out.ETag = "public, max-age=60", `"ea2"`
			case "b":
				out.CacheControl, out.ETag, out.Vary = "public, max-age=30", `"eb"`, "Authorization"
			case "p":
				out.CacheControl, out.ETag = "private, max-age=120", `"ep"`
			case "x":
				return nil, errors.New("boom")
			}
			return out, nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "plain", Method: http.MethodGet, Path: "/n/{id}"},
		func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
			return &getProjectOutput{Body: project{ID: in.ID}}, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postHeaders(t *testing.T, url, query string) http.Header {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"query": query})
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.Header
}

func TestCacheHeaders_Merge(t *testing.T) {
	srv := newCachedGat(t)
	gql := srv.URL + "/api/graphql"
	for _, c := range []struct {
		name, query, cc, etag string
	}{
		{"single op passes through", `{ Demo { cached(id: "a") { id } } }`, "public, max-age=60", `"ea"`},
		{"same policy passes through", `{ Demo { x: cached(id: "a") { id } y: cached(id: "a2") { id } } }`, "public, max-age=60", "W/"},
		{"different: most restrictive", `{ Demo { x: cached(id: "a") { id } y: cached(id: "b") { id } } }`, "public, max-age=30", "W/"},
		{"private wins", `{ Demo { x: cached(id: "a") { id } y: cached(id: "p") { id } } }`, "private, max-age=60", "W/"},
		{"op without headers: no-store", `{ Demo { x: cached(id: "a") { id } y: plain(id: "1") { id } } }`, "no-store", ""},
		{"error: no-store", `{ Demo { x: cached(id: "a") { id } y: cached(id: "x") { id } } }`, "no-store", ""},
		{"no op sets headers: none", `{ Demo { plain(id: "1") { id } } }`, "", ""},
	} {
		h := postHeaders(t, gql, c.query)
		if got := h.Get("Cache-Control"); got != c.cc {
			t.Errorf("%s: Cache-Control %q, want %q", c.name, got, c.cc)
		}
		if got := h.Get("ETag"); !strings.HasPrefix(got, c.etag) || (c.etag == "" && got != "") {
			t.Errorf("%s: ETag %q, want prefix %q", c.name, got, c.etag)
		}
	}
	if v := postHeaders(t, gql, `{ Demo { x: cached(id: "a") { id } y: cached(id: "b") { id } } }`).Get("Vary"); v != "Accept-Language, Authorization" {
		t.Errorf("Vary %q, want the union", v)
	}
}

func TestCacheHeaders_CombinedETagIsStableAndQuerySpecific(t *testing.T) {
	srv := newCachedGat(t)
	gql := srv.URL + "/api/graphql"
	q1 := `{ Demo { x: cached(id: "a") { id } y: cached(id: "b") { id } } }`
	q2 := `{ Demo { y: cached(id: "b") { id } x: cached(id: "a") { id } } }`
	e1, e1again, e2 := postHeaders(t, gql, q1).Get("ETag"), postHeaders(t, gql, q1).Get("ETag"), postHeaders(t, gql, q2).Get("ETag")
	if e1 == "" || e1 != e1again {
		t.Errorf("same query, ETags %q and %q", e1, e1again)
	}
	if e1 == e2 {
		t.Errorf("different documents share ETag %q", e1)
	}
}

func TestCacheHeaders_IfNoneMatch(t *testing.T) {
	srv := newCachedGat(t)
	u := srv.URL + "/api/graphql?" + url.Values{"query": {`{ Demo { cached(id: "a") { id } } }`}}.Encode()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("If-None-Match", `"ea"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("matching If-None-Match: status %d, want 304", resp.StatusCode)
	}
	req.Header.Set("If-None-Match", `"other"`)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("stale If-None-Match: status %d, want 200", resp.StatusCode)
	}
}
