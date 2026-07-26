package gat_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/gw/gat"
)

// huma does not read request parameters declared on an anonymous embedded
// struct — not into the OpenAPI document, and not into its runtime binder. The
// request silently binds zero values and the schema ships with holes, so gat
// refuses to mount rather than let that reach production.

type embeddedScope struct {
	Dir string `query:"dir"`
	Now string `query:"now"`
}

type embeddedParamInput struct {
	embeddedScope
	Spec string `query:"spec"`
}

type flatParamInput struct {
	Dir  string `query:"dir"`
	Now  string `query:"now"`
	Spec string `query:"spec"`
}

type deepEmbedOuter struct{ embeddedScope }

type deepEmbedInput struct {
	deepEmbedOuter
	Spec string `query:"spec"`
}

type headerEmbed struct {
	Trace string `header:"X-Trace"`
}

type headerEmbedInput struct {
	headerEmbed
}

// bodyOnlyEmbed carries no parameter tags, so embedding it is fine — the check
// must not fire on ordinary struct reuse.
type bodyOnlyEmbed struct {
	Ignored string `json:"ignored"`
}

type bodyOnlyEmbedInput struct {
	bodyOnlyEmbed
	Spec string `query:"spec"`
}

type okOutput struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

func mountWith[I any](t *testing.T, tweak func(*gat.Gateway)) error {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t)
	if tweak != nil {
		tweak(g)
	}
	gat.Register(api, g, huma.Operation{
		OperationID: "probe", Method: http.MethodGet, Path: "/probe",
	}, func(ctx context.Context, _ *I) (*okOutput, error) { return &okOutput{}, nil })
	return gat.RegisterHuma(api, g, "/api")
}

func TestEmbeddedParamsRefuseToMount(t *testing.T) {
	err := mountWith[embeddedParamInput](t, nil)
	if err == nil {
		t.Fatal("an embedded query parameter must not mount silently")
	}
	for _, want := range []string{"embeddedScope", "dir", "now", "Declare these fields directly"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), `"spec"`) {
		t.Errorf("spec is declared directly and must not be reported:\n%s", err)
	}
}

func TestFlatParamsMountCleanly(t *testing.T) {
	if err := mountWith[flatParamInput](t, nil); err != nil {
		t.Fatalf("directly-declared params must mount: %v", err)
	}
}

// An embed inside an embed is just as invisible to huma.
func TestNestedEmbeddedParamsAreFound(t *testing.T) {
	err := mountWith[deepEmbedInput](t, nil)
	if err == nil {
		t.Fatal("a parameter two embeds deep must still be reported")
	}
	if !strings.Contains(err.Error(), "deepEmbedOuter.embeddedScope") {
		t.Errorf("error should name the embed path:\n%s", err)
	}
}

func TestEmbeddedHeaderParamIsFound(t *testing.T) {
	err := mountWith[headerEmbedInput](t, nil)
	if err == nil || !strings.Contains(err.Error(), "X-Trace") {
		t.Fatalf("header params count too, got %v", err)
	}
}

// Embedding a struct that carries no parameter tags is ordinary Go reuse and
// must not trip the check.
func TestEmbeddedStructWithoutParamTagsIsFine(t *testing.T) {
	if err := mountWith[bodyOnlyEmbedInput](t, nil); err != nil {
		t.Fatalf("an embed with no param tags must mount: %v", err)
	}
}

func TestAllowEmbeddedParamsDowngradesToWarning(t *testing.T) {
	err := mountWith[embeddedParamInput](t, func(g *gat.Gateway) { g.AllowEmbeddedParams(true) })
	if err != nil {
		t.Fatalf("AllowEmbeddedParams(true) must mount: %v", err)
	}
}

// The check is a diagnostic for a real defect, so prove the defect: huma neither
// documents nor binds the embedded parameter. If huma ever fixes this, this test
// fails and the check can be dropped.
func TestHumaItselfDropsEmbeddedParams(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	var got embeddedParamInput
	huma.Register(api, huma.Operation{
		OperationID: "probe", Method: http.MethodGet, Path: "/probe",
	}, func(ctx context.Context, in *embeddedParamInput) (*okOutput, error) {
		got = *in
		return &okOutput{}, nil
	})

	doc, err := api.OpenAPI().MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), `"name":"dir"`) {
		t.Fatal("huma now documents embedded params — the gat check can be removed")
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe?dir=D&now=N&spec=S", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("probe: %d", rec.Code)
	}
	if got.Spec != "S" {
		t.Fatalf("directly-declared param must bind, got %q", got.Spec)
	}
	if got.Dir != "" || got.Now != "" {
		t.Fatalf("huma now binds embedded params (dir=%q now=%q) — the gat check can be removed",
			got.Dir, got.Now)
	}
}
