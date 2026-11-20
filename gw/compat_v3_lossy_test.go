package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IodeSystems/graphql-go/v3"
)

// TestLossyInputCoercion_EntryPoints: WithLossyInputCoercion reaches
// every path that executes client variables. Int 2.5 is rejected by
// default and truncated to 2 with the option. graphql-transport-ws is
// gat's, wired from the same config (graphql_handler.go); covered by
// gw/gat TestAllowLossyInputCoercion_WS.
func TestLossyInputCoercion_EntryPoints(t *testing.T) {
	const q = `query($v: Int){ t { qry(i: $v){ id } } }`
	const wantErr = `Variable "$v" got invalid value 2.5.`
	for _, m := range compatCoercionModes {
		t.Run(m.name, func(t *testing.T) {
			f := newCompatOpenAPI(t, m.opts...)
			check := func(t *testing.T, errMsg string) {
				t.Helper()
				req, _ := f.seen()
				if m.lossy {
					if errMsg != "" || req != "GET /q?i=2" {
						t.Fatalf("lossy: error=%q upstream=%q, want GET /q?i=2", errMsg, req)
					}
					return
				}
				if !strings.HasPrefix(errMsg, wantErr) || req != "" {
					t.Fatalf("strict: error=%q upstream=%q, want %q and no call", errMsg, req, wantErr)
				}
			}

			t.Run("http", func(t *testing.T) {
				f.reset()
				_, body := compatPost(t, f.h, q, `{"v":2.5}`)
				check(t, compatErrs(body))
			})

			t.Run("batch", func(t *testing.T) {
				f.reset()
				qj, _ := json.Marshal(q)
				req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`[{"query":`+string(qj)+`,"variables":{"v":2.5}}]`))
				req.Header.Set("Content-Type", "application/json")
				rr := httptest.NewRecorder()
				f.h.ServeHTTP(rr, req)
				var out []struct {
					Errors []struct{ Message string } `json:"errors"`
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || len(out) != 1 {
					t.Fatalf("batch body %s: %v", rr.Body.String(), err)
				}
				msg := ""
				if len(out[0].Errors) > 0 {
					msg = out[0].Errors[0].Message
				}
				check(t, msg)
			})

			t.Run("mcp-query", func(t *testing.T) {
				f.reset()
				out, err := f.gw.mcpQuery(context.Background(), mcpQueryInput{Query: q, Variables: map[string]any{"v": 2.5}})
				if err != nil {
					t.Fatal(err)
				}
				res := out.Response.(*graphql.Result)
				em := ""
				if len(res.Errors) > 0 {
					em = res.Errors[0].Message
				}
				check(t, em)
			})
		})
	}
}
