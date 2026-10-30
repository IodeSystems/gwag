package gat_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

func newVersionedGat(t *testing.T, versionedOnly bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t).VersionedOnly(versionedOnly)
	gat.Register(api, g, huma.Operation{OperationID: "balance", Method: http.MethodGet, Path: "/balance"},
		func(_ context.Context, in *balanceInput) (*balanceOutput, error) {
			out := &balanceOutput{}
			out.Body.Total = in.Min + 1
			return out, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestVersionedOnly(t *testing.T) {
	const flat = `{ Demo { balance(min: 1) { total } } }`
	const versioned = `{ Demo { v1 { balance(min: 1) { total } } } }`
	const want = `{"data":{"Demo":{"v1":{"balance":{"total":2}}}}}`

	// Default: both copies.
	srv := newVersionedGat(t, false)
	url := srv.URL + "/api/graphql"
	if got := graphqlRaw(t, url, flat, nil); got != `{"data":{"Demo":{"balance":{"total":2}}}}` {
		t.Errorf("default, flat: %s", got)
	}
	if got := graphqlRaw(t, url, versioned, nil); got != want {
		t.Errorf("default, versioned: %s", got)
	}

	// VersionedOnly: the versioned copy only.
	srv = newVersionedGat(t, true)
	url = srv.URL + "/api/graphql"
	if got := graphqlRaw(t, url, versioned, nil); got != want {
		t.Errorf("versioned: %s", got)
	}
	if got := graphqlRaw(t, url, flat, nil); !strings.Contains(got, `"errors"`) || strings.Contains(got, `"total"`) {
		t.Errorf("flat copy still served: %s", got)
	}
	sdl := string(mustGET(t, srv.URL+"/api/schema/graphql"))
	if strings.Count(sdl, "balance(") != 1 {
		t.Errorf("want one balance field:\n%s", sdl)
	}
	// REST is unaffected.
	if b := string(mustGET(t, srv.URL+"/balance?min=1")); !strings.Contains(b, `"total":2`) {
		t.Errorf("REST: %s", b)
	}
}
