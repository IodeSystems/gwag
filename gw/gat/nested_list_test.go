package gat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

type nestedRegion struct {
	Name   string       `json:"name"`
	Points [][2]float64 `json:"points"`
}

type nestedRegionIn struct{ Body nestedRegion }
type nestedRegionOut struct{ Body nestedRegion }

// A Go [][2]float64 is an OpenAPI array of array. It must render as a
// nested GraphQL list and round-trip through a mutation; it used to
// flatten to [Float]!, which refused [[1,2]] at validation.
func TestNestedListRoundTrip(t *testing.T) {
	mux := http.NewServeMux()
	api := huma.NewAPI(huma.DefaultConfig("Demo", "1.0.0"), humago.NewAdapter(mux, ""))
	g := mustNewGat(t)
	gat.Register(api, g, huma.Operation{OperationID: "putRegion", Method: http.MethodPost, Path: "/regions"},
		func(ctx context.Context, in *nestedRegionIn) (*nestedRegionOut, error) {
			return &nestedRegionOut{Body: in.Body}, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sdl := string(mustGET(t, srv.URL+"/api/schema/graphql"))
	// Output: required lists coerce nil → []; huma marks the slices
	// nullable (["array","null"]), so the input side stays nullable.
	for _, want := range []string{"points: [[Float!]]!", "points: [[Float!]]"} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL missing %q:\n%s", want, sdl)
		}
	}
	resp := mustGraphQL(t, srv.URL+"/api/graphql",
		`mutation { Demo { putRegion(body: {name: "a", points: [[1, 2], [3.5, 4]]}) { name points } } }`)
	got := digPath(resp, "data", "Demo", "putRegion", "points")
	rows, ok := got.([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("points = %v: %v", got, resp)
	}
	if r, ok := rows[1].([]any); !ok || len(r) != 2 || r[0] != 3.5 || r[1] != 4.0 {
		t.Errorf("points[1] = %v", rows[1])
	}
}

// Over gRPC, each inner list travels as a PointsRow box
// ({value: [...]}); the huma handler still sees [][2]float64.
func TestNestedListGRPC(t *testing.T) {
	mux := http.NewServeMux()
	api := huma.NewAPI(huma.DefaultConfig("Demo", "1.0.0"), humago.NewAdapter(mux, ""))
	g := mustNewGat(t)
	var seen [][2]float64
	gat.Register(api, g, huma.Operation{OperationID: "putRegion", Method: http.MethodPost, Path: "/regions"},
		func(ctx context.Context, in *nestedRegionIn) (*nestedRegionOut, error) {
			seen = in.Body.Points
			out := in.Body
			out.Points = append(out.Points, [2]float64{9, 9})
			return &nestedRegionOut{Body: out}, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatal(err)
	}
	if err := gat.RegisterGRPC(mux, g, "/api/grpc"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.EnableHTTP2 = true
	srv.Start()
	defer srv.Close()
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(mustGET(t, srv.URL+"/api/schema/proto"), fds); err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	md := findMethod(t, files, "putRegion")
	procedure := "/" + string(md.Parent().(protoreflect.ServiceDescriptor).FullName()) + "/" + string(md.Name())
	client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](srv.Client(), srv.URL+"/api/grpc"+procedure,
		connect.WithSchema(md),
		connect.WithResponseInitializer(func(spec connect.Spec, msg any) error {
			*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(spec.Schema.(protoreflect.MethodDescriptor).Output())
			return nil
		}))
	in := dynamicpb.NewMessage(md.Input())
	if err := protojson.Unmarshal([]byte(`{"body":{"name":"a","points":[{"value":[1,2]},{"value":[3.5,4]}]}}`), in); err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.CallUnary(context.Background(), connect.NewRequest(in))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if want := [][2]float64{{1, 2}, {3.5, 4}}; !reflect.DeepEqual(seen, want) {
		t.Errorf("handler saw %v, want %v", seen, want)
	}
	b, _ := protojson.Marshal(resp.Msg)
	var got, exp any
	_ = json.Unmarshal(b, &got)
	_ = json.Unmarshal([]byte(`{"name":"a","points":[{"value":[1,2]},{"value":[3.5,4]},{"value":[9,9]}]}`), &exp)
	if !reflect.DeepEqual(got, exp) {
		t.Errorf("response %s", b)
	}
}

type nestedGrid struct {
	Name  string                    `json:"name"`
	Cells map[string]map[string]int `json:"cells"`
}

type nestedGridIn struct{ Body nestedGrid }
type nestedGridOut struct{ Body nestedGrid }

// Over gRPC, each inner map of a map of maps travels as a CellsValue
// box ({value: {...}}); the huma handler still sees
// map[string]map[string]int (int64 values, sent by protojson as
// strings). GraphQL renders the field as a JSON
// scalar and is unaffected.
func TestNestedMapGRPC(t *testing.T) {
	mux := http.NewServeMux()
	api := huma.NewAPI(huma.DefaultConfig("Demo", "1.0.0"), humago.NewAdapter(mux, ""))
	g := mustNewGat(t)
	var seen map[string]map[string]int
	gat.Register(api, g, huma.Operation{OperationID: "putGrid", Method: http.MethodPost, Path: "/grids"},
		func(ctx context.Context, in *nestedGridIn) (*nestedGridOut, error) {
			seen = in.Body.Cells
			out := in.Body
			out.Cells = map[string]map[string]int{"a": {"x": 1, "y": 2}, "b": {"z": 3}}
			return &nestedGridOut{Body: out}, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatal(err)
	}
	if err := gat.RegisterGRPC(mux, g, "/api/grpc"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.EnableHTTP2 = true
	srv.Start()
	defer srv.Close()

	// GraphQL side: JSON scalar, plain nested object.
	payload, _ := json.Marshal(map[string]any{
		"query":     `mutation($c: JSON!) { Demo { putGrid(body: {name: "q", cells: $c}) { cells } } }`,
		"variables": map[string]any{"c": map[string]any{"a": map[string]any{"k": 7}}},
	})
	hr, err := http.Post(srv.URL+"/api/graphql", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(hr.Body)
	hr.Body.Close()
	if !strings.Contains(string(rb), `"cells":{"a":{"x":1,"y":2},"b":{"z":3}}`) {
		t.Errorf("graphql response = %s", rb)
	}
	if want := map[string]map[string]int{"a": {"k": 7}}; !reflect.DeepEqual(seen, want) {
		t.Errorf("graphql handler saw %v, want %v", seen, want)
	}
	seen = nil

	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(mustGET(t, srv.URL+"/api/schema/proto"), fds); err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	md := findMethod(t, files, "putGrid")
	procedure := "/" + string(md.Parent().(protoreflect.ServiceDescriptor).FullName()) + "/" + string(md.Name())
	client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](srv.Client(), srv.URL+"/api/grpc"+procedure,
		connect.WithSchema(md),
		connect.WithResponseInitializer(func(spec connect.Spec, msg any) error {
			*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(spec.Schema.(protoreflect.MethodDescriptor).Output())
			return nil
		}))
	in := dynamicpb.NewMessage(md.Input())
	if err := protojson.Unmarshal([]byte(`{"body":{"name":"a","cells":{"r":{"value":{"p":1,"q":"9007199254740993"}}}}}`), in); err != nil {
		t.Fatalf("build request: %v", err)
	}
	out, err := client.CallUnary(context.Background(), connect.NewRequest(in))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if want := map[string]map[string]int{"r": {"p": 1, "q": 9007199254740993}}; !reflect.DeepEqual(seen, want) {
		t.Errorf("handler saw %v, want %v", seen, want)
	}
	b, _ := protojson.Marshal(out.Msg)
	var got, exp any
	_ = json.Unmarshal(b, &got)
	_ = json.Unmarshal([]byte(`{"name":"a","cells":{"a":{"value":{"x":"1","y":"2"}},"b":{"value":{"z":"3"}}}}`), &exp)
	if !reflect.DeepEqual(got, exp) {
		t.Errorf("response %s", b)
	}
}
