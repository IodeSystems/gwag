package profile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name, yaml string
		mode       Mode
		want       []string // substrings of problems; nil = valid
	}{
		{"ok push", "service: a\nprovides:\n  v1: {kind: openapi, file: a.json}\n", ModePush, nil},
		{"ok pull", "service: a\nprovides:\n  v1: {kind: openapi, url: /openapi.json}\n", ModePull, nil},
		{"no service", "provides:\n  v1: {kind: openapi, file: a.json}\n", ModePush, []string{"service: required"}},
		{"bad version", "service: a\nprovides:\n  V1: {kind: openapi, file: a.json}\n", ModePush, []string{"provides.V1"}},
		{"bad kind", "service: a\nprovides:\n  v1: {kind: soap}\n", ModePush, []string{"provides.v1.kind"}},
		{"pull needs url", "service: a\nprovides:\n  v1: {kind: proto, file: a.proto}\n", ModePull, []string{"provides.v1.url"}},
		{"graphql endpoint", "service: a\nprovides:\n  v1: {kind: graphql}\n", ModePush, []string{"provides.v1.endpoint"}},
		{"bad ref", "service: a\nhealth: healthz\nprovides:\n  v1: {kind: openapi, file: a.json}\n", ModePush, []string{"health"}},
		{"self dep", "service: a\nprovides:\n  v1: {kind: openapi, file: a.json}\ndepends:\n  a: [v1]\n", ModePush, []string{"depend on itself"}},
		{"bad line", "service: a\nprovides:\n  v1: {kind: openapi, file: a.json}\ndepends:\n  b: [one]\n", ModePush, []string{"line \"one\""}},
		{"bad channel", "service: a\nprovides:\n  v1: {kind: openapi, file: a.json}\nsubscribes:\n  - {channel: nodot}\n", ModePush, []string{"subscribes[0].channel"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := Parse([]byte(c.yaml))
			if err != nil {
				t.Fatal(err)
			}
			err = p.Validate(c.mode)
			if c.want == nil {
				if err != nil {
					t.Fatalf("err=%v, want valid", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("valid, want %v", c.want)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err=%v, want %q", err, w)
				}
			}
		})
	}
	if _, err := Parse([]byte("service: a\nprovidez: {}\n")); err == nil {
		t.Fatal("unknown key accepted")
	}
}

func TestVersionsSorted(t *testing.T) {
	p := &Profile{Provides: map[string]Provide{"unstable": {}, "v10": {}, "v2": {}, "v1": {}}}
	if got := strings.Join(p.Versions(), ","); got != "v1,v2,v10,unstable" {
		t.Fatalf("versions=%s", got)
	}
}

func TestCanonicalOpenAPIIgnoresDeprecatedAndOrder(t *testing.T) {
	a := `{"openapi":"3.0.3","info":{"title":"x","version":"1"},"paths":{"/a":{"get":{"operationId":"a","responses":{}}}}}`
	b := "paths:\n  /a:\n    get:\n      responses: {}\n      deprecated: true\n      operationId: a\ninfo: {version: \"1\", title: x}\nopenapi: 3.0.3\n"
	ca, _, err := Canonical(KindOpenAPI, []byte(a), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	cb, _, err := Canonical(KindOpenAPI, []byte(b), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if ca != cb {
		t.Fatalf("differ: %s", Diff(ca, cb))
	}
	c := strings.Replace(a, `"operationId":"a"`, `"operationId":"b"`, 1)
	cc, _, _ := Canonical(KindOpenAPI, []byte(c), nil, "")
	if ca == cc || Diff(ca, cc) == "" {
		t.Fatal("a renamed operation compares equal")
	}
	if _, _, err := Canonical(KindOpenAPI, []byte(`{"swaggerish":1}`), nil, ""); err == nil {
		t.Fatal("a non-OpenAPI document accepted")
	}
}

const greeterSrc = `syntax = "proto3";
package greeter.v1;
import "common/v1/name.proto";
// Greets.
service Greeter {
  rpc Hello(common.v1.Name) returns (common.v1.Name);
}
`

const nameSrc = `syntax = "proto3"; package common.v1; message Name { string n = 1; }`

func TestCanonicalProtoSourceEqualsDescriptorSet(t *testing.T) {
	imports := map[string][]byte{"common/v1/name.proto": []byte(nameSrc)}
	src, pkg, err := Canonical(KindProto, []byte(greeterSrc), imports, "")
	if err != nil {
		t.Fatal(err)
	}
	if pkg != "greeter.v1" {
		t.Fatalf("pkg=%s", pkg)
	}
	// The same file as a FileDescriptorSet, with the import first and
	// a deprecated option a server adds to older versions.
	files := map[string]string{"greeter.proto": greeterSrc, "common/v1/name.proto": nameSrc}
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
		Accessor: protocompile.SourceAccessorFromMap(files),
	})}
	out, err := c.Compile(context.Background(), "greeter.proto")
	if err != nil {
		t.Fatal(err)
	}
	fdp := protodesc.ToFileDescriptorProto(out[0])
	fdp.Service[0].Options = &descriptorpb.ServiceOptions{Deprecated: proto.Bool(true)}
	dep := protodesc.ToFileDescriptorProto(out[0].Imports().Get(0).FileDescriptor)
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp, dep}}
	raw, _ := proto.Marshal(set)
	fds, _, err := Canonical(KindProto, raw, nil, pkg)
	if err != nil {
		t.Fatal(err)
	}
	if src != fds {
		t.Fatalf("differ: %s", Diff(src, fds))
	}
	changed := strings.Replace(greeterSrc, "rpc Hello", "rpc Hi", 1)
	other, _, _ := Canonical(KindProto, []byte(changed), imports, "")
	if other == src {
		t.Fatal("a renamed method compares equal")
	}
}

func TestResolve(t *testing.T) {
	cases := []struct{ addr, ref, want, err string }{
		{"svc:50051", "/healthz", "http://svc:50051/healthz", ""},
		{"http://svc:8080/base", "/openapi.json?v=1", "http://svc:8080/openapi.json?v=1", ""},
		{"svc:50051", "http://svc:8081/healthz", "http://svc:8081/healthz", ""},
		{"svc:50051", "http://other:8081/healthz", "", "is not the service's host"},
		{"svc:50051", "file:///etc/passwd", "", "want a path or an http(s) URL"},
	}
	for _, c := range cases {
		got, err := Resolve(c.addr, c.ref)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("Resolve(%q,%q) err=%v, want %q", c.addr, c.ref, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Resolve(%q,%q)=%q,%v want %q", c.addr, c.ref, got, err, c.want)
		}
	}
}

func TestFetchRefusesRedirect(t *testing.T) {
	srv := httptest.NewServer(http.RedirectHandler("http://169.254.169.254/", http.StatusFound))
	t.Cleanup(srv.Close)
	c := &http.Client{CheckRedirect: NoRedirects}
	if _, err := Fetch(context.Background(), c, srv.URL); err == nil || !strings.Contains(err.Error(), "status 302") {
		t.Fatalf("err=%v, want the redirect refused", err)
	}
}

func TestSchemaIsJSON(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(Schema, &m); err != nil {
		t.Fatal(err)
	}
}
