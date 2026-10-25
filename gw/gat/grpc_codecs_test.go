package gat_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

type widgetInput struct {
	Dry   bool  `query:"dry"`
	Limit int64 `query:"limit"`
	Body  struct {
		Name  string   `json:"name"`
		Tags  []string `json:"tags"`
		Count int      `json:"count"`
	}
}

type widgetOutput struct {
	Body struct {
		Echo string `json:"echo"`
	}
}

// Pins: the binary proto codec decodes requests straight into the
// input struct; JSON goes through dynamicpb. Every protocol and codec
// connect serves has to hand the handler the same input.
func TestGRPCIngress_RequestAcrossCodecs(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Codecs", "1.0.0"))
	g := mustNewGat(t)
	gat.Register(api, g, huma.Operation{
		OperationID: "createWidget",
		Method:      http.MethodPost,
		Path:        "/widgets",
	}, func(ctx context.Context, in *widgetInput) (*widgetOutput, error) {
		out := &widgetOutput{}
		out.Body.Echo = fmt.Sprintf("%v|%d|%s|%v|%d", in.Dry, in.Limit, in.Body.Name, in.Body.Tags, in.Body.Count)
		return out, nil
	})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	if err := gat.RegisterGRPC(mux, g, "/api/grpc"); err != nil {
		t.Fatalf("RegisterGRPC: %v", err)
	}
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.EnableHTTP2 = true
	srv.Start()
	defer srv.Close()

	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(mustGET(t, srv.URL+"/api/schema/proto"), fds); err != nil {
		t.Fatalf("unmarshal FDS: %v", err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	md := findMethod(t, files, "createWidget")
	url := srv.URL + "/api/grpc/" + string(md.Parent().(protoreflect.ServiceDescriptor).FullName()) + "/" + string(md.Name())

	req := dynamicpb.NewMessage(md.Input())
	fields := md.Input().Fields()
	var bodyFd protoreflect.FieldDescriptor
	for i := 0; i < fields.Len(); i++ {
		if fields.Get(i).Kind() == protoreflect.MessageKind {
			bodyFd = fields.Get(i)
		}
	}
	if bodyFd == nil || fields.ByName("dry") == nil || fields.ByName("limit") == nil {
		t.Fatalf("unexpected input shape: %s", listFields(md.Input()))
	}
	req.Set(fields.ByName("dry"), protoreflect.ValueOfBool(true))
	req.Set(fields.ByName("limit"), protoreflect.ValueOfInt64(7))
	b := req.Mutable(bodyFd).Message()
	bfs := bodyFd.Message().Fields()
	b.Set(bfs.ByName("name"), protoreflect.ValueOfString("w"))
	tags := b.Mutable(bfs.ByName("tags")).List()
	tags.Append(protoreflect.ValueOfString("a"))
	tags.Append(protoreflect.ValueOfString("b"))
	b.Set(bfs.ByName("count"), protoreflect.ValueOfInt64(3))
	want := "true|7|w|[a b]|3"

	for name, opts := range map[string][]connect.ClientOption{
		"connect proto": nil,
		"connect json":  {connect.WithProtoJSON()},
		"grpc-web":      {connect.WithGRPCWeb()},
		"grpc":          {connect.WithGRPC()},
	} {
		t.Run(name, func(t *testing.T) {
			opts = append(opts,
				connect.WithSchema(md),
				connect.WithResponseInitializer(func(spec connect.Spec, msg any) error {
					*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(md.Output())
					return nil
				}))
			client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](srv.Client(), url, opts...)
			resp, err := client.CallUnary(context.Background(), connect.NewRequest(req))
			if err != nil {
				t.Fatalf("CallUnary: %v", err)
			}
			got := resp.Msg.Get(md.Output().Fields().ByName("echo")).String()
			if got != want {
				t.Errorf("echo = %q; want %q", got, want)
			}
		})
	}

	// Malformed wire bytes are the client's fault, on either path.
	t.Run("malformed", func(t *testing.T) {
		resp, err := http.Post(url, "application/proto", bytes.NewReader([]byte{0x08, 0x80}))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "invalid_argument") {
			t.Errorf("status %d body %s; want 400 invalid_argument", resp.StatusCode, raw)
		}
	})
}
