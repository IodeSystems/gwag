package gat_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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

type spanKey struct{}

type dispatchLog struct {
	mu    sync.Mutex
	calls []string // "observe:<op>:<err>" and "done:<op>:<err>"
}

func (o *dispatchLog) add(s string) {
	o.mu.Lock()
	o.calls = append(o.calls, s)
	o.mu.Unlock()
}

func (o *dispatchLog) take() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := o.calls
	o.calls = nil
	return out
}

func errLabel(err error) string {
	if err != nil {
		return "err"
	}
	return "ok"
}

// newObservedGat serves getProject (fails for id "bad") over GraphQL
// and gRPC with both an observer and a hook. The handler echoes the
// span the hook put on ctx, so the hook's context is shown to reach it.
func newObservedGat(t *testing.T) (*httptest.Server, *dispatchLog) {
	t.Helper()
	obs := &dispatchLog{}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t).
		ObserveDispatch(func(op string, dur time.Duration, err error) {
			obs.add("observe:" + op + ":" + errLabel(err))
		}).
		HookDispatch(func(ctx context.Context, op string) (context.Context, func(error)) {
			return context.WithValue(ctx, spanKey{}, "span-"+op), func(err error) {
				obs.add("done:" + op + ":" + errLabel(err))
			}
		})
	gat.Register(api, g, huma.Operation{OperationID: "getProject", Method: http.MethodGet, Path: "/projects/{id}"},
		func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
			if in.ID == "bad" {
				return nil, errors.New("boom")
			}
			span, _ := ctx.Value(spanKey{}).(string)
			return &getProjectOutput{Body: project{ID: in.ID, Name: span}}, nil
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
	t.Cleanup(srv.Close)
	return srv, obs
}

func wantCalls(t *testing.T, surface string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: calls = %v, want %v", surface, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: calls = %v, want %v", surface, got, want)
			return
		}
	}
}

func TestDispatchObserverAndHook_GraphQL(t *testing.T) {
	srv, obs := newObservedGat(t)
	resp := mustGraphQL(t, srv.URL+"/api/graphql", `query { Demo { getProject(id: "p1") { name } } }`)
	if got := digPath(resp, "data", "Demo", "getProject", "name"); got != "span-getProject" {
		t.Errorf("handler saw span %v, want span-getProject", got)
	}
	wantCalls(t, "graphql ok", obs.take(), "observe:getProject:ok", "done:getProject:ok")

	mustGraphQL(t, srv.URL+"/api/graphql", `query { Demo { getProject(id: "bad") { name } } }`)
	wantCalls(t, "graphql err", obs.take(), "observe:getProject:err", "done:getProject:err")
}

func TestDispatchObserverAndHook_GRPC(t *testing.T) {
	srv, obs := newObservedGat(t)
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(mustGET(t, srv.URL+"/api/schema/proto"), fds); err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	md := findMethod(t, files, "getProject")
	procedure := "/" + string(md.Parent().(protoreflect.ServiceDescriptor).FullName()) + "/" + string(md.Name())
	client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](
		srv.Client(), srv.URL+"/api/grpc"+procedure,
		connect.WithSchema(md),
		connect.WithResponseInitializer(func(spec connect.Spec, msg any) error {
			*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(spec.Schema.(protoreflect.MethodDescriptor).Output())
			return nil
		}),
	)
	in := dynamicpb.NewMessage(md.Input())
	in.Set(md.Input().Fields().ByName("id"), protoreflect.ValueOfString("p1"))
	resp, err := client.CallUnary(context.Background(), connect.NewRequest(in))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := dynamicAsJSON(resp.Msg)["name"]; got != "span-getProject" {
		t.Errorf("handler saw span %v, want span-getProject", got)
	}
	// Before this, gRPC calls bypassed the observer entirely.
	wantCalls(t, "grpc", obs.take(), "observe:getProject:ok", "done:getProject:ok")
}
