package ir

import (
	"context"
	"errors"
	"testing"
)

func TestDispatchRegistry_SetGetDelete(t *testing.T) {
	r := NewDispatchRegistry()
	if got := r.Get("missing"); got != nil {
		t.Fatalf("Get on empty registry: want nil, got %v", got)
	}

	d := DispatcherFunc(func(_ context.Context, _ map[string]any) (any, error) {
		return "ok", nil
	})
	r.Set("greeter/v1/hello", d)
	if r.Len() != 1 {
		t.Fatalf("Len after Set: want 1, got %d", r.Len())
	}

	got := r.Get("greeter/v1/hello")
	if got == nil {
		t.Fatal("Get after Set: want dispatcher, got nil")
	}
	out, err := got.Dispatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Dispatch result: want %q, got %v", "ok", out)
	}

	r.Delete("greeter/v1/hello")
	if r.Len() != 0 {
		t.Fatalf("Len after Delete: want 0, got %d", r.Len())
	}
}

func TestDispatchRegistry_Replace(t *testing.T) {
	r := NewDispatchRegistry()
	r.Set("k", DispatcherFunc(func(_ context.Context, _ map[string]any) (any, error) {
		return 1, nil
	}))
	r.Set("k", DispatcherFunc(func(_ context.Context, _ map[string]any) (any, error) {
		return 2, nil
	}))
	out, _ := r.Get("k").Dispatch(context.Background(), nil)
	if out != 2 {
		t.Fatalf("Set replaces: want 2, got %v", out)
	}
}

// DispatcherMiddleware composes around a Dispatcher. The runtime
// extraction work in step 3+ wraps each format's Dispatcher with a
// BackpressureMiddleware; this test pins the composition shape.
func TestDispatcherMiddleware_Composes(t *testing.T) {
	calls := []string{}
	wrapInner := DispatcherMiddleware(func(next Dispatcher) Dispatcher {
		return DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
			calls = append(calls, "inner:before")
			out, err := next.Dispatch(ctx, args)
			calls = append(calls, "inner:after")
			return out, err
		})
	})
	wrapOuter := DispatcherMiddleware(func(next Dispatcher) Dispatcher {
		return DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
			calls = append(calls, "outer:before")
			out, err := next.Dispatch(ctx, args)
			calls = append(calls, "outer:after")
			return out, err
		})
	})
	core := DispatcherFunc(func(_ context.Context, _ map[string]any) (any, error) {
		calls = append(calls, "core")
		return nil, errors.New("core err")
	})
	d := wrapOuter(wrapInner(core))
	if _, err := d.Dispatch(context.Background(), nil); err == nil || err.Error() != "core err" {
		t.Fatalf("error propagation: want %q, got %v", "core err", err)
	}
	want := []string{"outer:before", "inner:before", "core", "inner:after", "outer:after"}
	if len(calls) != len(want) {
		t.Fatalf("call order: want %v, got %v", want, calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call[%d]: want %q, got %q", i, want[i], calls[i])
		}
	}
}

func TestPopulateSchemaIDs_FlatAndNested(t *testing.T) {
	svc := &Service{
		Namespace: "greeter",
		Version:   "v1",
		Operations: []*Operation{
			{Name: "hello"},
		},
		Groups: []*OperationGroup{
			{
				Name:       "admin",
				Operations: []*Operation{{Name: "listPeers"}},
				Groups: []*OperationGroup{
					{
						Name:       "v2",
						Operations: []*Operation{{Name: "forgetPeer"}},
					},
				},
			},
		},
	}

	PopulateSchemaIDs(svc)

	cases := []struct {
		got, want SchemaID
	}{
		{svc.Operations[0].SchemaID, "greeter/v1/hello"},
		{svc.Groups[0].Operations[0].SchemaID, "greeter/v1/admin_listPeers"},
		{svc.Groups[0].Groups[0].Operations[0].SchemaID, "greeter/v1/admin_v2_forgetPeer"},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Fatalf("case %d: want %q, got %q", i, c.want, c.got)
		}
	}
}

// FlatOperations clones Operations from Groups; SchemaID rides
// along on the clone — the runtime renderer should never need to
// recompute it.
func TestFlatOperations_PreservesSchemaID(t *testing.T) {
	svc := &Service{
		Namespace:  "n",
		Version:    "v1",
		Operations: []*Operation{{Name: "top"}},
		Groups: []*OperationGroup{{
			Name:       "g",
			Operations: []*Operation{{Name: "inner"}},
		}},
	}
	PopulateSchemaIDs(svc)

	flat := svc.FlatOperations()
	if len(flat) != 2 {
		t.Fatalf("flat count: want 2, got %d", len(flat))
	}
	want := map[string]SchemaID{
		"top":     "n/v1/top",
		"g_inner": "n/v1/g_inner",
	}
	for _, op := range flat {
		if got := op.SchemaID; got != want[op.Name] {
			t.Fatalf("op %q SchemaID: want %q, got %q", op.Name, want[op.Name], got)
		}
	}
}
