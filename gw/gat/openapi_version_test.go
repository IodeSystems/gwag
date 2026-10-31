package gat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/getkin/kin-openapi/openapi3"

	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/ir"
)

type gadget struct {
	ID      string     `json:"id" example:"w1" doc:"Widget id."`
	Note    *string    `json:"note" nullable:"true"`
	Color   string     `json:"color" enum:"red,blue"`
	Size    int64      `json:"size" minimum:"0" exclusiveMinimum:"0" maximum:"100"`
	Ratio   float64    `json:"ratio" exclusiveMaximum:"1"`
	Price   cents      `json:"price"`
	Blob    []byte     `json:"blob,omitempty"`
	Made    time.Time  `json:"made"`
	Parts   []gadget   `json:"parts,omitempty"`
	Parent  *gadget    `json:"parent,omitempty"`
	Updated *time.Time `json:"updated,omitempty" nullable:"true"`
}

type gadgetInput struct {
	ID string `path:"id"`
}

type gadgetOutput struct {
	Body gadget
}

// newVersionGat serves a huma API (OpenAPI 3.1.0) with the schema
// forms 3.0 spells differently, and a RegisterSSE operation.
func newVersionGat(t *testing.T) (*gat.Gateway, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Widgets", "1.0.0"))
	g := mustNewGat(t)
	gat.Register(api, g, huma.Operation{OperationID: "getWidget", Method: http.MethodGet, Path: "/widgets/{id}"},
		func(_ context.Context, in *gadgetInput) (*gadgetOutput, error) {
			return &gadgetOutput{Body: gadget{ID: in.ID}}, nil
		})
	gat.RegisterSSE(api, g, huma.Operation{OperationID: "widgetEvents", Method: http.MethodGet, Path: "/widgets/events"},
		map[string]any{"event": SessionEvent{}, "snapshot": SessionSnapshot{}},
		func(_ context.Context, _ *struct{}, send sse.Sender) {
			_ = send.Data(SessionEvent{Seq: 1})
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return g, srv
}

func validateSpec(t *testing.T, raw []byte) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(raw)
	if err != nil {
		t.Fatalf("load: %v\n%s", err, raw)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate %s: %v\n%s", doc.OpenAPI, err, raw)
	}
	return doc
}

// The default output of a huma service is what it was before the
// version option existed: the 3.1.0 spec, as RenderOpenAPI gives it.
func TestSchemaOpenAPI_HumaDefaultUnchanged(t *testing.T) {
	g, srv := newVersionGat(t)
	got := mustGET(t, srv.URL+"/api/schema/openapi")
	doc, err := ir.RenderOpenAPI(g.Services()[0])
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	_ = json.NewEncoder(&want).Encode(doc)
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("default output changed:\n got %s\nwant %s", got, want.Bytes())
	}
	if !strings.Contains(string(got), `"openapi":"3.1.0"`) {
		t.Fatalf("huma spec is not 3.1.0: %.200s", got)
	}
}

// ?openapi=3.0 and OpenAPIVersion("3.0") give a valid 3.0 document
// of huma's spec that ingests to the same IR as the 3.1 one.
func TestSchemaOpenAPI_HumaTo30(t *testing.T) {
	g, srv := newVersionGat(t)
	// The IR gwag would ingest from the 3.1 spec. (gat's own IR knows
	// widgetEvents is a stream from RegisterSSE; the spec says so only
	// in the 3.2 itemSchema form, which huma does not write.)
	origin, _ := g.Services()[0].Origin.(*openapi3.T)
	if origin == nil {
		t.Fatal("no origin spec")
	}
	src := ir.IngestOpenAPI(origin)
	for _, viaOption := range []bool{false, true} {
		url := srv.URL + "/api/schema/openapi?openapi=3.0"
		if viaOption {
			g.OpenAPIVersion("3.0")
			url = srv.URL + "/api/schema/openapi"
		}
		raw := mustGET(t, url)
		doc := validateSpec(t, raw)
		if doc.OpenAPI != ir.OpenAPI30Version {
			t.Fatalf("openapi = %q", doc.OpenAPI)
		}
		for _, bad := range []string{`"type":["`, `"examples":[`, `"exclusiveMinimum":0`, `"exclusiveMaximum":1`} {
			if strings.Contains(string(raw), bad) {
				t.Errorf("3.0 output has %s", bad)
			}
		}
		for _, want := range []string{`"nullable":true`, `"x-gwag-scalar":"Cents"`, `"format":"byte"`, `"exclusiveMinimum":true`} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("3.0 output lacks %s", want)
			}
		}
		down := ir.IngestOpenAPI(doc)
		if a, b := opsAndFields(src), opsAndFields(down); a != b {
			t.Errorf("IR differs:\nsource\n%s\n3.0\n%s", a, b)
		}
	}
	g.OpenAPIVersion("")
}

// opsAndFields summarizes the IR of svc: operations with their kind,
// and fields with type and nullability. []byte (contentEncoding
// base64) is left out: its 3.0 form, format byte, ingests as Upload.
func opsAndFields(svc *ir.Service) string {
	var lines []string
	for _, op := range svc.FlatOperations() {
		lines = append(lines, fmt.Sprintf("%s %s %s kind=%v", op.Name, op.HTTPMethod, op.HTTPPath, op.Kind))
	}
	for name, ty := range svc.Types {
		for _, f := range ty.Fields {
			if f.Name == "blob" {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s.%s %s%s%v null=%v rep=%v",
				name, f.Name, f.Type.Named, f.Type.ScalarName, f.Type.Builtin, f.Nullable, f.Repeated))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// A 3.2 source (BYO IR) is served as declared by default, as 3.0 or
// 3.1 on request.
func TestSchemaOpenAPI_32Source(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData([]byte(gatQuerySpec))
	if err != nil {
		t.Fatal(err)
	}
	svc := ir.IngestOpenAPI(doc)
	svc.Namespace, svc.Version = "things", "v1"
	ir.PopulateSchemaIDs(svc)
	g, err := gat.New(gat.ServiceRegistration{Service: svc, BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := gat.RegisterHTTP(mux, g, "/api"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for query, want := range map[string]string{"": "3.2.0", "?openapi=3.0": ir.OpenAPI30Version, "?openapi=3.1": ir.OpenAPI31Version} {
		raw := mustGET(t, srv.URL+"/api/schema/openapi"+query)
		if query == "" {
			var got map[string]any
			_ = json.Unmarshal(raw, &got)
			if got["openapi"] != want {
				t.Errorf("default openapi = %v", got["openapi"])
			}
			continue
		}
		out := validateSpec(t, raw)
		if out.OpenAPI != want {
			t.Errorf("%s: openapi = %q, want %s", query, out.OpenAPI, want)
		}
		if !strings.Contains(string(raw), `"x-additional-operations":{"QUERY":{`) {
			t.Errorf("%s: QUERY not under x-additional-operations:\n%s", query, raw)
		}
	}
}

func TestOpenAPIVersion_Bad(t *testing.T) {
	g := mustNewGat(t)
	defer func() {
		if recover() == nil {
			t.Error("OpenAPIVersion(\"3.2\") did not panic")
		}
	}()
	g.OpenAPIVersion("3.2")
}
