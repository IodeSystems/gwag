package gateway

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/iodesystems/gwag/v2/gw/ir"
	qav1 "github.com/iodesystems/gwag/v2/gw/proto/quotaauth/v1"
)

// A dispatcher wrapper that does not forward DispatchAppend silently
// demotes every request to the Dispatch + marshal path. These tests
// pin that every wrapper, alone and in the production chain order,
// keeps the capability AND reaches the inner DispatchAppend.
//
// To add a wrapper: append it to appendWrapperCases. The source guard
// (TestAppendWrappers_AllWrappersListed) fails until you do.

type appendProbe struct {
	appendCalls   atomic.Int32
	dispatchCalls atomic.Int32
}

func (p *appendProbe) Dispatch(context.Context, map[string]any) (any, error) {
	p.dispatchCalls.Add(1)
	return map[string]any{"ok": true}, nil
}

func (p *appendProbe) DispatchAppend(_ context.Context, _ map[string]any, dst []byte) ([]byte, error) {
	p.appendCalls.Add(1)
	return append(dst, `{"ok":true}`...), nil
}

// appendTestDesc builds a one-field message descriptor for the
// canonical runtime-chain wrapper.
func appendTestDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    strPtr("append_wrappers.proto"),
		Package: strPtr("appendtest"),
		Syntax:  strPtr("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: strPtr("Req"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     strPtr("id"),
				JsonName: strPtr("id"),
				Number:   int32Ptr(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			}},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	return fd.Messages().ByName("Req")
}

func strPtr(s string) *string { return &s }
func int32Ptr(i int32) *int32 { return &i }

type appendWrapperCase struct {
	name string
	wrap ir.DispatcherMiddleware
}

// appendWrapperCases lists every wrapper individually, built the way
// production builds them. Fixtures: quota delegate always grants,
// caller-id enforce accepts the public extractor, backpressure has a
// real semaphore, the runtime chain is a passthrough.
func appendWrapperCases(t *testing.T) []appendWrapperCase {
	t.Helper()
	g := New(
		WithoutMetrics(), WithoutBackpressure(),
		WithQuota(QuotaOptions{BlockSize: 1000}),
		WithCallerIDPublic(), WithCallerIDEnforce(),
	)
	t.Cleanup(g.Close)
	g.quotaAuth.callDelegateFn = func(_ context.Context, _, _, _ string) (*qav1.AcquireBlockResponse, error) {
		return &qav1.AcquireBlockResponse{Code: qav1.QuotaAuthCode_QUOTA_AUTH_CODE_OK, GrantedPermits: 1000}, nil
	}
	bp := backpressureConfig{
		Sem:      make(chan struct{}, 4),
		Queueing: new(atomic.Int32),
		Metrics:  noopMetrics{},
		Label:    "test", Kind: "unary",
	}
	desc := appendTestDesc(t)
	passthrough := Middleware(func(next Handler) Handler { return next })
	canonical := func(next ir.Dispatcher) ir.Dispatcher {
		return wrapCanonicalDispatcherWithChain(next, passthrough, desc, "ns", "v1", "op")
	}
	return []appendWrapperCase{
		{"backpressureMiddleware", backpressureMiddleware(bp)},
		{"quotaMiddleware", g.quotaMiddleware("ns", "v1")},
		{"callerIDEnforceMiddleware", g.callerIDEnforceMiddleware()},
		{"wrapCanonicalDispatcherWithChain", canonical},
	}
}

func assertAppendReaches(t *testing.T, label string, d ir.Dispatcher, p *appendProbe) {
	t.Helper()
	ad, ok := d.(ir.AppendDispatcher)
	if !ok {
		t.Fatalf("%s: wrapped dispatcher %T lost ir.AppendDispatcher (requests fall back to the map path)", label, d)
	}
	r, _ := newRequestWithHeader(PublicCallerIDHeader, "alice")
	ctx := withHTTPRequest(context.Background(), r)
	out, err := ad.DispatchAppend(ctx, map[string]any{"id": "x"}, []byte("pre:"))
	if err != nil {
		t.Fatalf("%s: DispatchAppend: %v", label, err)
	}
	if got := p.appendCalls.Load(); got != 1 {
		t.Fatalf("%s: inner DispatchAppend reached %d times, want 1 (wrapper %T did not forward)", label, got, d)
	}
	if got := p.dispatchCalls.Load(); got != 0 {
		t.Fatalf("%s: inner Dispatch reached %d times during DispatchAppend, want 0", label, got)
	}
	if string(out) != `pre:{"ok":true}` {
		t.Fatalf("%s: output %q, want dst preserved and inner bytes appended", label, out)
	}
}

func TestAppendWrappers_EachForwardsDispatchAppend(t *testing.T) {
	for _, c := range appendWrapperCases(t) {
		t.Run(c.name, func(t *testing.T) {
			p := &appendProbe{}
			assertAppendReaches(t, c.name, c.wrap(p), p)
		})
	}
}

// TestAppendWrappers_ProductionChains mirrors the wrapper order in
// proto_register / openapi_register / graphql_register /
// mcp_register. Keep the orders in sync with those files.
func TestAppendWrappers_ProductionChains(t *testing.T) {
	by := map[string]ir.DispatcherMiddleware{}
	for _, c := range appendWrapperCases(t) {
		by[c.name] = c.wrap
	}
	bp, canon, quota, enforce := by["backpressureMiddleware"], by["wrapCanonicalDispatcherWithChain"], by["quotaMiddleware"], by["callerIDEnforceMiddleware"]
	chains := []struct {
		name string
		mws  []ir.DispatcherMiddleware // innermost first
	}{
		{"proto", []ir.DispatcherMiddleware{bp, quota, enforce}},
		{"openapi", []ir.DispatcherMiddleware{bp, canon, quota, enforce}},
		{"graphql", []ir.DispatcherMiddleware{bp, canon, quota, enforce}},
		{"graphql-group", []ir.DispatcherMiddleware{bp, quota, enforce}},
		{"internal-proto", []ir.DispatcherMiddleware{quota, enforce}},
	}
	for _, c := range chains {
		t.Run(c.name, func(t *testing.T) {
			p := &appendProbe{}
			var d ir.Dispatcher = p
			for _, mw := range c.mws {
				d = mw(d)
			}
			assertAppendReaches(t, c.name+" chain", d, p)
		})
	}
}

// TestAppendWrappers_AllWrappersListed parses non-test sources in
// gw/ and fails when a function that wraps an ir.Dispatcher is not
// in appendWrapperCases. Matches: funcs returning
// ir.DispatcherMiddleware, and funcs taking and returning
// ir.Dispatcher.
func TestAppendWrappers_AllWrappersListed(t *testing.T) {
	listed := map[string]bool{}
	for _, c := range appendWrapperCases(t) {
		listed[c.name] = true
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	isIR := func(e ast.Expr, name string) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		x, _ := sel.X.(*ast.Ident)
		return x != nil && x.Name == "ir" && sel.Sel.Name == name
	}
	var found []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range af.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Type.Results == nil {
				continue
			}
			retMW, retD := false, false
			for _, r := range fn.Type.Results.List {
				retMW = retMW || isIR(r.Type, "DispatcherMiddleware")
				retD = retD || isIR(r.Type, "Dispatcher")
			}
			takesD := false
			if fn.Type.Params != nil {
				for _, p := range fn.Type.Params.List {
					takesD = takesD || isIR(p.Type, "Dispatcher")
				}
			}
			if retMW || (retD && takesD) {
				found = append(found, fn.Name.Name)
			}
		}
	}
	sort.Strings(found)
	for _, n := range found {
		if !listed[n] {
			t.Errorf("%s wraps ir.Dispatcher but is not in appendWrapperCases: add it so the DispatchAppend fast path stays pinned", n)
		}
	}
	for n := range listed {
		i := sort.SearchStrings(found, n)
		if i == len(found) || found[i] != n {
			t.Errorf("%s is in appendWrapperCases but no longer matches a wrapper in source: remove or rename", n)
		}
	}
}
