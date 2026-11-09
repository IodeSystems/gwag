package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/bufbuild/protocompile"
	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Canonical returns the canonical text of a spec, for an equality
// test between what a service registered and what it serves:
//
//   - openapi: the document (JSON or YAML) as JSON with sorted keys,
//     without `deprecated` on operations (gwag and gat add those to
//     older versions themselves).
//   - proto: data is .proto source (compiled with imports), a binary
//     FileDescriptorSet, or one in protojson. The file compared is
//     the entry (source), else the file whose package is pkg, else
//     the last file. Its name, source info and deprecated options on
//     the file, services and methods are dropped.
//   - graphql: introspection JSON with sorted keys.
//
// For proto it also returns the package, so a served
// FileDescriptorSet can be matched against a registered source.
func Canonical(kind string, data []byte, imports map[string][]byte, pkg string) (canon, protoPkg string, err error) {
	switch kind {
	case KindOpenAPI:
		c, err := canonOpenAPI(data)
		return c, "", err
	case KindGraphQL:
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return "", "", fmt.Errorf("introspection: %w", err)
		}
		c, err := canonJSON(v)
		return c, "", err
	case KindProto:
		return canonProto(data, imports, pkg)
	}
	return "", "", fmt.Errorf("unknown kind %q", kind)
}

// openAPIMethods are the operation keys of an OpenAPI path item.
var openAPIMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace", "query"}

func canonOpenAPI(b []byte) (string, error) {
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return "", fmt.Errorf("openapi: %w", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "", errors.New("openapi: not an object")
	}
	if _, ok := m["openapi"]; !ok {
		if _, ok := m["swagger"]; !ok {
			return "", errors.New("openapi: no \"openapi\" key; not an OpenAPI document")
		}
	}
	paths, _ := m["paths"].(map[string]any)
	strip := func(op any) {
		if o, ok := op.(map[string]any); ok {
			delete(o, "deprecated")
		}
	}
	for _, item := range paths {
		pi, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, method := range openAPIMethods {
			strip(pi[method])
		}
		if extra, ok := pi["additionalOperations"].(map[string]any); ok {
			for _, op := range extra {
				strip(op)
			}
		}
	}
	return canonJSON(m)
}

func canonJSON(v any) (string, error) {
	// yaml.v3 decodes maps as map[string]any; json.Marshal sorts
	// map keys, which is the canonical order.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

const entryName = "entry.proto"

func canonProto(data []byte, imports map[string][]byte, pkg string) (string, string, error) {
	var fdp *descriptorpb.FileDescriptorProto
	t := bytes.TrimSpace(data)
	set := &descriptorpb.FileDescriptorSet{}
	switch {
	case len(t) > 0 && t[0] == '{':
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(t, set); err != nil || len(set.File) == 0 {
			one := &descriptorpb.FileDescriptorProto{}
			if err2 := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(t, one); err2 != nil {
				return "", "", fmt.Errorf("proto: protojson: %v", err)
			}
			set.File = []*descriptorpb.FileDescriptorProto{one}
		}
	case looksLikeProtoSource(t):
		f, err := compileSource(data, imports)
		if err != nil {
			return "", "", err
		}
		set.File = []*descriptorpb.FileDescriptorProto{f}
	default:
		if err := proto.Unmarshal(data, set); err != nil {
			return "", "", fmt.Errorf("proto: not .proto source, FileDescriptorSet or protojson: %w", err)
		}
	}
	if len(set.File) == 0 {
		return "", "", errors.New("proto: empty FileDescriptorSet")
	}
	fdp = set.File[len(set.File)-1]
	if pkg != "" {
		for _, f := range set.File {
			if f.GetPackage() == pkg {
				fdp = f
				break
			}
		}
	}
	fdp = proto.Clone(fdp).(*descriptorpb.FileDescriptorProto)
	fdp.Name = nil
	fdp.SourceCodeInfo = nil
	if o := fdp.GetOptions(); o != nil {
		o.Deprecated = nil
	}
	for _, s := range fdp.GetService() {
		if o := s.GetOptions(); o != nil {
			o.Deprecated = nil
		}
		for _, m := range s.GetMethod() {
			if o := m.GetOptions(); o != nil {
				o.Deprecated = nil
			}
		}
	}
	// Options left empty by the strip marshal as {}; drop them so a
	// file that never had options compares equal.
	if o := fdp.GetOptions(); o != nil && proto.Size(o) == 0 {
		fdp.Options = nil
	}
	for _, s := range fdp.GetService() {
		if o := s.GetOptions(); o != nil && proto.Size(o) == 0 {
			s.Options = nil
		}
		for _, m := range s.GetMethod() {
			if o := m.GetOptions(); o != nil && proto.Size(o) == 0 {
				m.Options = nil
			}
		}
	}
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(fdp)
	if err != nil {
		return "", "", err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", "", err
	}
	c, err := canonJSON(v)
	return c, fdp.GetPackage(), err
}

// looksLikeProtoSource reports whether b is .proto text: it starts
// (after comments and blank lines) with syntax, edition, package,
// import, option, message, service or enum.
func looksLikeProtoSource(b []byte) bool {
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "//") || strings.HasPrefix(l, "/*") || strings.HasPrefix(l, "*") {
			continue
		}
		for _, kw := range []string{"syntax", "edition", "package", "import", "option", "message", "service", "enum"} {
			if strings.HasPrefix(l, kw) {
				return true
			}
		}
		return false
	}
	return false
}

func compileSource(src []byte, imports map[string][]byte) (*descriptorpb.FileDescriptorProto, error) {
	files := map[string][]byte{entryName: src}
	for k, v := range imports {
		files[k] = v
	}
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			Accessor: func(p string) (io.ReadCloser, error) {
				if b, ok := files[p]; ok {
					return io.NopCloser(bytes.NewReader(b)), nil
				}
				return nil, fmt.Errorf("proto import not found: %s", p)
			},
		}),
	}
	out, err := c.Compile(context.Background(), entryName)
	if err != nil {
		return nil, fmt.Errorf("proto: compile: %w", err)
	}
	return protodesc.ToFileDescriptorProto(out[0]), nil
}

// Diff returns a short description of the first lines where two
// canonical texts differ, for a reason line. "" when equal.
func Diff(want, got string) string {
	if want == got {
		return ""
	}
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return fmt.Sprintf("line %d: want %q, got %q", i+1, strings.TrimSpace(x), strings.TrimSpace(y))
		}
	}
	return "differs"
}

// BaseURL returns the http base URL of a registered address:
// "host:port" → "http://host:port"; a URL is returned without path,
// query or fragment.
func BaseURL(addr string) (*url.URL, error) {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, fmt.Errorf("address %q has no host", addr)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// Resolve turns ref (a path, or an absolute http(s) URL) into a URL
// on the host of addr. An absolute URL must name the same host as
// addr (any port); anything else is refused, so a probe goes only
// to the registered service.
func Resolve(addr, ref string) (string, error) {
	base, err := BaseURL(addr)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(ref, "/") {
		r, err := url.Parse(ref)
		if err != nil {
			return "", err
		}
		return base.ResolveReference(r).String(), nil
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%q: want a path or an http(s) URL", ref)
	}
	if !strings.EqualFold(u.Hostname(), base.Hostname()) {
		return "", fmt.Errorf("%q: host %s is not the service's host %s", ref, u.Hostname(), base.Hostname())
	}
	return u.String(), nil
}

// DialAddr is the host:port to dial for addr (a host:port or URL).
func DialAddr(addr string) (string, error) {
	base, err := BaseURL(addr)
	if err != nil {
		return "", err
	}
	if base.Port() != "" {
		return base.Host, nil
	}
	port := "80"
	if base.Scheme == "https" {
		port = "443"
	}
	return net.JoinHostPort(base.Hostname(), port), nil
}
