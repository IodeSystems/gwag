package gat_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	graphql "github.com/IodeSystems/graphql-go/v2"
	"github.com/IodeSystems/graphql-go/v2/gqlerrors"
	"github.com/getkin/kin-openapi/openapi3"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/ir"
)

const byoSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "shop", "version": "1.0.0"},
  "paths": {
    "/items/{id}": {"get": {"operationId": "getItem",
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
      "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}}}}},
    "/tags": {"get": {"operationId": "listTags",
      "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "array", "items": {"type": "string"}}}}}}}},
    "/echo": {"post": {"operationId": "echoGrid",
      "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}},
      "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}}}}}
  },
  "components": {"schemas": {"Item": {"type": "object", "required": ["id"],
    "properties": {
      "id": {"type": "string"},
      "grid": {"type": "array", "items": {"type": "array", "items": {"type": "integer"}}}}}}}
}`

func byoUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/tags":
			io.WriteString(w, `["a","b"]`)
		case r.URL.Path == "/echo":
			b, _ := io.ReadAll(r.Body)
			w.Write(b)
		case strings.HasPrefix(r.URL.Path, "/items/"):
			id := strings.TrimPrefix(r.URL.Path, "/items/")
			switch id {
			case "missing":
				http.Error(w, `{"error":"no such item"}`, http.StatusNotFound)
			case "dup":
				http.Error(w, "exists", http.StatusConflict)
			case "down":
				http.Error(w, "later", http.StatusServiceUnavailable)
			default:
				io.WriteString(w, `{"id":"`+id+`","grid":[[1,2],[3]]}`)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newByoGat(t *testing.T, spec, baseURL string) (*gat.Gateway, *httptest.Server, *descriptorpb.FileDescriptorSet) {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	svc := ir.IngestOpenAPI(doc)
	svc.Namespace, svc.Version = "shop", "v1"
	ir.PopulateSchemaIDs(svc)
	g, err := gat.New(gat.ServiceRegistration{Service: svc, BaseURL: baseURL})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := gat.RegisterHTTP(mux, g, "/api"); err != nil {
		t.Fatal(err)
	}
	if err := gat.RegisterGRPC(mux, g, "/api/grpc"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.EnableHTTP2 = true
	srv.Start()
	t.Cleanup(srv.Close)
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(mustGET(t, srv.URL+"/api/schema/proto"), fds); err != nil {
		t.Fatal(err)
	}
	return g, srv, fds
}

// byoCall calls op over gRPC (connect client) with the request as
// protojson and returns the response as protojson.
func byoCall(t *testing.T, srv *httptest.Server, fds *descriptorpb.FileDescriptorSet, op, reqJSON string) (string, error) {
	t.Helper()
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	md := findMethod(t, files, op)
	procedure := "/" + string(md.Parent().(protoreflect.ServiceDescriptor).FullName()) + "/" + string(md.Name())
	client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](srv.Client(), srv.URL+"/api/grpc"+procedure,
		connect.WithSchema(md),
		connect.WithResponseInitializer(func(spec connect.Spec, msg any) error {
			*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(spec.Schema.(protoreflect.MethodDescriptor).Output())
			return nil
		}))
	in := dynamicpb.NewMessage(md.Input())
	if err := protojson.Unmarshal([]byte(reqJSON), in); err != nil {
		t.Fatalf("request %s: %v", reqJSON, err)
	}
	resp, err := client.CallUnary(context.Background(), connect.NewRequest(in))
	if err != nil {
		return "", err
	}
	out, err := protojson.Marshal(resp.Msg)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), nil
}

func TestGRPC_BYOOpenAPI(t *testing.T) {
	up := byoUpstream(t)
	_, srv, fds := newByoGat(t, byoSpec, up.URL)

	t.Run("message with nested list", func(t *testing.T) {
		out, err := byoCall(t, srv, fds, "getItem", `{"id":"x1"}`)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			ID   string
			Grid []struct{ Value []int }
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if got.ID != "x1" || len(got.Grid) != 2 || len(got.Grid[0].Value) != 2 || got.Grid[1].Value[0] != 3 {
			t.Fatalf("response = %s", out)
		}
	})

	t.Run("list response under value", func(t *testing.T) {
		out, err := byoCall(t, srv, fds, "listTags", `{}`)
		if err != nil {
			t.Fatal(err)
		}
		var got struct{ Value []string }
		if err := json.Unmarshal([]byte(out), &got); err != nil || strings.Join(got.Value, ",") != "a,b" {
			t.Fatalf("response = %s", out)
		}
	})

	t.Run("nested list in request body", func(t *testing.T) {
		out, err := byoCall(t, srv, fds, "echoGrid", `{"body":{"id":"e","grid":[{"value":[7,8]},{"value":[9]}]}}`)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			ID   string
			Grid []struct{ Value []int }
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if got.ID != "e" || len(got.Grid) != 2 || got.Grid[0].Value[1] != 8 || got.Grid[1].Value[0] != 9 {
			t.Fatalf("response = %s", out)
		}
	})

	for id, want := range map[string]connect.Code{
		"missing": connect.CodeNotFound,
		"dup":     connect.CodeAlreadyExists,
		"down":    connect.CodeUnavailable,
	} {
		t.Run("upstream "+id, func(t *testing.T) {
			_, err := byoCall(t, srv, fds, "getItem", `{"id":"`+id+`"}`)
			if err == nil {
				t.Fatal("want error")
			}
			if got := connect.CodeOf(err); got != want {
				t.Fatalf("code = %v, want %v (%v)", got, want, err)
			}
		})
	}
}

func TestBYOOpenAPI_ErrorStatus(t *testing.T) {
	up := byoUpstream(t)
	g, srv, _ := newByoGat(t, byoSpec, up.URL)

	for id, want := range map[string]struct {
		status int
		code   string
		conn   connect.Code
	}{
		"missing": {404, "not_found", connect.CodeNotFound},
		"dup":     {409, "already_exists", connect.CodeAlreadyExists},
		"down":    {503, "unavailable", connect.CodeUnavailable},
	} {
		t.Run(id, func(t *testing.T) {
			resp := mustGraphQL(t, srv.URL+"/api/graphql", `{ shop { v1 { getItem(id: "`+id+`") { id } } } }`)
			errs, _ := resp["errors"].([]any)
			if len(errs) != 1 {
				t.Fatalf("response = %v", resp)
			}
			ext := errs[0].(map[string]any)["extensions"].(map[string]any)
			if int(ext["status"].(float64)) != want.status || ext["code"] != want.code {
				t.Fatalf("extensions = %v, want %d %s", ext, want.status, want.code)
			}
		})
		t.Run(id+" classify", func(t *testing.T) {
			res := graphql.Do(graphql.Params{Schema: *g.Schema(), RequestString: `{ shop { v1 { getItem(id: "` + id + `") { id } } } }`, Context: context.Background()})
			if len(res.Errors) != 1 {
				t.Fatalf("errors = %v", res.Errors)
			}
			err := res.Errors[0].OriginalError()
			if ge, ok := err.(*gqlerrors.Error); ok {
				err = ge.OriginalError
			}
			if got := gat.ErrorCode(err); got != want.conn {
				t.Fatalf("ErrorCode = %v, want %v", got, want.conn)
			}
			if got := gat.HTTPStatus(gat.ErrorCode(err)); got != want.status {
				t.Fatalf("HTTPStatus = %d, want %d", got, want.status)
			}
		})
	}
}

func TestGRPC_BYOOpenAPIStream(t *testing.T) {
	up, _ := tickUpstream(t)
	_, srv, fds := newByoGat(t, tickStreamSpec, up.URL)
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	md := findMethod(t, files, "jsonl")
	if !md.IsStreamingServer() {
		t.Skip("jsonl is not a server stream in the rendered proto")
	}
	procedure := "/" + string(md.Parent().(protoreflect.ServiceDescriptor).FullName()) + "/jsonl"
	client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](srv.Client(), srv.URL+"/api/grpc"+procedure,
		connect.WithSchema(md),
		connect.WithResponseInitializer(func(spec connect.Spec, msg any) error {
			*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(spec.Schema.(protoreflect.MethodDescriptor).Output())
			return nil
		}))
	in := dynamicpb.NewMessage(md.Input())
	if err := protojson.Unmarshal([]byte(`{"n":3}`), in); err != nil {
		t.Fatal(err)
	}
	st, err := client.CallServerStream(context.Background(), connect.NewRequest(in))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for st.Receive() {
		b, _ := protojson.Marshal(st.Msg())
		got = append(got, string(b))
	}
	if err := st.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != `{"n":1},{"n":2},{"n":3}` {
		t.Fatalf("ticks = %v", got)
	}
}
