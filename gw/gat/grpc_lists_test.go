package gat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// TestGRPC_ListParamsAndResponses: an array query param is a repeated
// field, and a list response comes back under the synthesized
// response message's "value".
func TestGRPC_ListParamsAndResponses(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Shop", "1.0.0"))
	g := mustNewGat(t)
	gat.Register(api, g, huma.Operation{OperationID: "listOrders", Method: http.MethodGet, Path: "/orders"},
		func(ctx context.Context, in *listOrdersInput) (*listOrdersOutput, error) {
			return &listOrdersOutput{Body: []order{{ID: "o1", CustomerID: "c1"}, {ID: "o2", CustomerID: "c2"}}}, nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "getCustomersById", Method: http.MethodGet, Path: "/customers"},
		func(ctx context.Context, in *customersInput) (*customersOutput, error) {
			out := &customersOutput{}
			for _, id := range in.IDs {
				out.Body = append(out.Body, customer{ID: id, Name: "N" + id})
			}
			return out, nil
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
	files, _ := protodesc.NewFiles(fds)
	for _, name := range []string{"listOrders", "getCustomersById"} {
		md := findMethod(t, files, name)
		procedure := "/" + string(md.Parent().(protoreflect.ServiceDescriptor).FullName()) + "/" + string(md.Name())
		client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](srv.Client(), srv.URL+"/api/grpc"+procedure,
			connect.WithSchema(md),
			connect.WithResponseInitializer(func(spec connect.Spec, msg any) error {
				*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(spec.Schema.(protoreflect.MethodDescriptor).Output())
				return nil
			}))
		in := dynamicpb.NewMessage(md.Input())
		if fd := md.Input().Fields().ByName("ids"); fd != nil {
			if fd.IsList() {
				l := in.Mutable(fd).List()
				l.Append(protoreflect.ValueOfString("c1"))
				l.Append(protoreflect.ValueOfString("c9"))
			} else {
				in.Set(fd, protoreflect.ValueOfString("c1"))
			}
		}
		resp, err := client.CallUnary(context.Background(), connect.NewRequest(in))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		b, _ := protojson.Marshal(resp.Msg)
		want := map[string]string{
			"listOrders":       `{"value":[{"customerId":"c1","id":"o1"},{"customerId":"c2","id":"o2"}]}`,
			"getCustomersById": `{"value":[{"id":"c1","name":"Nc1"},{"id":"c9","name":"Nc9"}]}`,
		}[name]
		var got, exp any
		_ = json.Unmarshal(b, &got)
		_ = json.Unmarshal([]byte(want), &exp)
		if !reflect.DeepEqual(got, exp) {
			t.Errorf("%s -> %s, want %s", name, b, want)
		}
	}
}
