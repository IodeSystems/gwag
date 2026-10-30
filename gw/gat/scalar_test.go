package gat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

type cents int64

func (cents) Schema(huma.Registry) *huma.Schema {
	return gat.IntegerScalar("Cents", "Money in integer cents.")
}

type chargeInput struct {
	Body struct {
		Amount cents   `json:"amount"`
		Splits []cents `json:"splits,omitempty"`
	}
}

type chargeOutput struct {
	Body struct {
		Amount cents   `json:"amount"`
		Splits []cents `json:"splits"`
		Fee    *cents  `json:"fee,omitempty"`
	}
}

type balanceInput struct {
	Min cents `query:"min"`
}

type balanceOutput struct {
	Body struct {
		Total cents `json:"total"`
	}
}

func newScalarGat(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t).LongAsNumber(false)
	gat.Register(api, g, huma.Operation{OperationID: "charge", Method: http.MethodPost, Path: "/charge"},
		func(_ context.Context, in *chargeInput) (*chargeOutput, error) {
			out := &chargeOutput{}
			out.Body.Amount, out.Body.Splits = in.Body.Amount, in.Body.Splits
			fee := in.Body.Amount / 100
			out.Body.Fee = &fee
			return out, nil
		})
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

// graphqlRaw posts query with variables and returns the raw body, so a
// test can tell a JSON number from a numeric string.
func graphqlRaw(t *testing.T, url, query string, vars map[string]any) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	resp, err := http.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestIntegerScalar_SDL(t *testing.T) {
	srv := newScalarGat(t)
	sdl := string(mustGET(t, srv.URL+"/api/schema/graphql"))
	for _, want := range []string{"scalar Cents", "amount: Cents!", "total: Cents!", "[Cents!]"} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL lacks %q:\n%s", want, sdl)
		}
	}
	if strings.Count(sdl, "scalar Cents") != 1 {
		t.Errorf("Cents declared more than once")
	}
}

func TestIntegerScalar_NumberOnTheWire(t *testing.T) {
	srv := newScalarGat(t)
	url := srv.URL + "/api/graphql"
	// 2_500_000_000 is past GraphQL Int (2^31-1); LongAsNumber(false)
	// would send a Long as a string.
	q := `mutation($a: Cents!) { Demo { charge(body: {amount: $a, splits: [1, "2"]}) { amount splits fee } } }`
	want := `{"data":{"Demo":{"charge":{"amount":2500000000,"splits":[1,2],"fee":25000000}}}}`
	for _, in := range []any{2500000000, "2500000000"} {
		if got := graphqlRaw(t, url, q, map[string]any{"a": in}); got != want {
			t.Errorf("input %T: got %s", in, got)
		}
	}
	if got := graphqlRaw(t, url, `{ Demo { balance(min: 9007199254740990) { total } } }`, nil); got != `{"data":{"Demo":{"balance":{"total":9007199254740991}}}}` {
		t.Errorf("literal at the edge: %s", got)
	}
}

func TestIntegerScalar_RefusesInexact(t *testing.T) {
	srv := newScalarGat(t)
	url := srv.URL + "/api/graphql"
	q := `mutation($a: Cents!) { Demo { charge(body: {amount: $a}) { amount } } }`
	for _, in := range []any{1.5, "1.5", "9007199254740992", "abc"} {
		if got := graphqlRaw(t, url, q, map[string]any{"a": in}); !strings.Contains(got, `"errors"`) || strings.Contains(got, `"amount"`) {
			t.Errorf("input %v accepted: %s", in, got)
		}
	}
	// Output past 2^53-1 is not rounded: the field errors.
	got := graphqlRaw(t, url, `{ Demo { balance(min: 9007199254740991) { total } } }`, nil)
	if !strings.Contains(got, `"errors"`) || strings.Contains(got, "9007199254740992") {
		t.Errorf("output past range: %s", got)
	}
}

func TestIntegerScalar_RESTAndSchemas(t *testing.T) {
	srv := newScalarGat(t)
	// REST keeps the plain integer contract, with the range enforced.
	resp, err := http.Post(srv.URL+"/charge", "application/json", strings.NewReader(`{"amount":2500000000}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"amount":2500000000`) {
		t.Fatalf("REST: %d %s", resp.StatusCode, b)
	}
	resp, err = http.Post(srv.URL+"/charge", "application/json", strings.NewReader(`{"amount":9007199254740992}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("REST past range: %d", resp.StatusCode)
	}

	oas := string(mustGET(t, srv.URL+"/api/schema/openapi"))
	if !strings.Contains(oas, `"x-gwag-scalar":"Cents"`) || !strings.Contains(oas, `"format":"int64"`) {
		t.Errorf("OpenAPI view lacks the scalar extension or int64: %s", oas)
	}
	// Proto has no custom scalars: Cents carries int64.
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(mustGET(t, srv.URL+"/api/schema/proto"), &fds); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range fds.File {
		for _, m := range f.MessageType {
			for _, fd := range m.Field {
				if fd.GetName() == "amount" {
					found = true
					if fd.GetType() != descriptorpb.FieldDescriptorProto_TYPE_INT64 {
						t.Errorf("%s.amount is %v, want int64", m.GetName(), fd.GetType())
					}
				}
			}
		}
	}
	if !found {
		t.Error("no amount field in the proto view")
	}
}
