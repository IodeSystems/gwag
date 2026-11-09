package gat

// Several versions of one namespace in one process. Each version is
// its own huma API (its own OpenAPI document); RegisterHumaVersion
// ingests one as the IR service (namespace, vN), and RegisterHuma or
// RegisterHTTP then builds one schema from all of them. The IR
// renderer gwag uses does the folding: the latest version flat under
// the namespace, every version under `ns.vN`, older ones
// @deprecated, type names prefixed `ns_vN_` for the older ones.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// humaVersion is one RegisterHumaVersion call.
type humaVersion struct {
	api huma.API
	svc *ir.Service
}

// VersionOption configures one RegisterHumaVersion call.
//
// Stability: experimental
type VersionOption func(*versionConfig)

type versionConfig struct {
	namespace  string
	deprecated string
}

// VersionNamespace sets the namespace instead of deriving it from the
// API's OpenAPI title. Every version of a gateway must end up with the
// same namespace.
//
// Stability: experimental
func VersionNamespace(ns string) VersionOption {
	return func(c *versionConfig) { c.namespace = ns }
}

// VersionDeprecated marks the version deprecated with reason. Without
// it, only versions older than the latest are deprecated, with the
// reason "<latest> is current" (gwag's). It sets the reason on the
// latest version too, ahead of a newer one.
//
// Stability: experimental
func VersionDeprecated(reason string) VersionOption {
	return func(c *versionConfig) { c.deprecated = reason }
}

// RegisterHumaVersion ingests the operations registered on api (with
// Register or RegisterSSE) as version of the gateway's namespace. Call
// it once per version, each with its own huma API, after that API's
// Register calls; then finalize with RegisterHuma (on any of the APIs,
// or another one with no gat operations) or RegisterHTTP. It builds
// nothing and mounts nothing itself.
//
// version is "v" and a number ("v1", "v2"). The namespace comes from
// the API's OpenAPI title, as in RegisterHuma, unless VersionNamespace
// sets it; all versions must share it. One schema serves them all:
//
//	{ ns { op } }        the latest version
//	{ ns { v1 { op } } } each version; older ones @deprecated
//
// Types of the latest version are named ns_T, those of older ones
// ns_vN_T, so two versions can both have an Order. Over gRPC each
// version is the proto package ns.vN, so the paths are
// {prefix}/ns.vN.Service/{Method}.
//
// Operations registered on an API that no RegisterHumaVersion call
// covers make the finalize call fail.
//
// Stability: experimental
func RegisterHumaVersion(api huma.API, g *Gateway, version string, opts ...VersionOption) error {
	if g.built {
		return fmt.Errorf("gat: gateway already finalized")
	}
	if !protoPackageVersionRE.MatchString(version) {
		return fmt.Errorf("gat: RegisterHumaVersion: version %q: want v and a number, e.g. v1", version)
	}
	var cfg versionConfig
	for _, o := range opts {
		o(&cfg)
	}
	ns := cfg.namespace
	if ns == "" {
		ns = humaNamespace(api)
	} else if ir.SanitizeNamespace(ns) != ns {
		return fmt.Errorf("gat: RegisterHumaVersion: namespace %q: use letters, digits and _", ns)
	}
	doc := api.OpenAPI()
	for _, v := range g.versions {
		switch {
		case v.svc.Namespace != ns:
			return fmt.Errorf("gat: RegisterHumaVersion %s: namespace %q differs from %s's %q; set VersionNamespace",
				version, ns, v.svc.Version, v.svc.Namespace)
		case v.svc.Version == version:
			return fmt.Errorf("gat: RegisterHumaVersion: %s registered twice", version)
		case v.api.OpenAPI() == doc:
			return fmt.Errorf("gat: RegisterHumaVersion %s: this huma API is already %s", version, v.svc.Version)
		}
	}

	var captured []*capturedOp
	for _, c := range g.captured {
		if c.doc == doc {
			captured = append(captured, c)
		}
	}
	var subs []*streamOp
	for _, s := range g.subs {
		if s.doc != doc {
			continue
		}
		if strings.HasPrefix(s.op.Path, subscriptionPathPrefix) && doc != g.subOwner {
			return fmt.Errorf("gat: RegisterHumaVersion %s: Subscribe %s: Subscribe works on one huma API only; use RegisterSSE",
				version, s.op.OperationID)
		}
		subs = append(subs, s)
	}
	if len(captured) == 0 && len(subs) == 0 {
		return fmt.Errorf("gat: RegisterHumaVersion %s: no operations registered on this huma API with gat.Register or gat.RegisterSSE", version)
	}
	if err := g.preflight(captured); err != nil {
		return err
	}
	src := doc
	if g.subAPI != nil && g.subOwner == doc {
		src = g.mergedOpenAPI(api)
	}
	svc, err := g.ingestHumaService(src, api, captured, subs, ns, version)
	if err != nil {
		return err
	}
	svc.Deprecated = cfg.deprecated
	g.services = append(g.services, svc)
	g.versions = append(g.versions, &humaVersion{api: api, svc: svc})
	return nil
}

// buildVersions builds the schema of a RegisterHumaVersion gateway,
// refusing gat operations no version covers.
func (g *Gateway) buildVersions() error {
	for _, c := range g.captured {
		if !c.claimed {
			return fmt.Errorf("gat: %s is registered on a huma API that no RegisterHumaVersion call covers", c.op.OperationID)
		}
	}
	for _, s := range g.subs {
		if s.irOp == nil {
			return fmt.Errorf("gat: stream %s is registered on a huma API that no RegisterHumaVersion call covers", s.op.OperationID)
		}
	}
	return g.build()
}

// OpenAPISpec returns the OpenAPI document (JSON) of the huma API
// registered as version, as huma built it: the spec to hand to gwag's
// controlclient as that version's Service.OpenAPISpec. It carries no
// deprecation marks (gwag deprecates older versions itself, and the
// spec of a frozen version then does not change when a newer one is
// added); {prefix}/schema/openapi?version= adds them.
//
// After RegisterHuma without versions, the API is "v1". Gateways
// built from ServiceRegistration have no huma API: an error.
//
// Stability: experimental
func (g *Gateway) OpenAPISpec(version string) ([]byte, error) {
	for _, v := range g.versions {
		if v.svc.Version == version {
			return json.Marshal(v.api.OpenAPI())
		}
	}
	if g.humaAPI != nil && version == "v1" {
		return json.Marshal(g.humaAPI.OpenAPI())
	}
	return nil, fmt.Errorf("gat: no huma API registered as %q", version)
}

// deprecations returns each deprecated service's @deprecated reason,
// as the GraphQL renderer sets it: the manual reason (Service.
// Deprecated), else "<latest> is current" for a version older than its
// namespace's latest.
func deprecations(services []*ir.Service) map[*ir.Service]string {
	byNS := map[string][]*ir.Service{}
	for _, svc := range services {
		byNS[svc.Namespace] = append(byNS[svc.Namespace], svc)
	}
	out := map[*ir.Service]string{}
	for _, svcs := range byNS {
		// Same order and pick as ir.RenderGraphQLRuntimeFields.
		sorted := append([]*ir.Service(nil), svcs...)
		sort.SliceStable(sorted, func(i, j int) bool {
			return ir.ParseRuntimeVersionN(sorted[i].Version) < ir.ParseRuntimeVersionN(sorted[j].Version)
		})
		latest := sorted[len(sorted)-1]
		for _, svc := range sorted {
			auto := ""
			if svc != latest {
				auto = latest.Version + " is current"
			}
			if r := ir.CombineDepReason(svc.Deprecated, auto); r != "" {
				out[svc] = r
			}
		}
	}
	return out
}

// serviceFilter selects services for the schema views:
// `?service=ns[:vN][,ns[:vN]...]` (gwag's selector) and gat's
// `?version=vN`. Both given: a service must match both.
type serviceFilter struct {
	sels    []nsVersion
	version string
}

type nsVersion struct{ namespace, version string }

func parseServiceFilter(q url.Values) (serviceFilter, error) {
	f := serviceFilter{version: strings.TrimSpace(q.Get("version"))}
	for _, p := range strings.Split(q.Get("service"), ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ns, ver, _ := strings.Cut(p, ":")
		ns, ver = strings.TrimSpace(ns), strings.TrimSpace(ver)
		if ns == "" {
			return f, fmt.Errorf("service selector %q: empty namespace", p)
		}
		f.sels = append(f.sels, nsVersion{ns, ver})
	}
	return f, nil
}

func (f serviceFilter) set() bool { return f.version != "" || len(f.sels) > 0 }

func (f serviceFilter) match(svc *ir.Service) bool {
	if f.version != "" && svc.Version != f.version {
		return false
	}
	if len(f.sels) == 0 {
		return true
	}
	for _, s := range f.sels {
		if s.namespace == svc.Namespace && (s.version == "" || s.version == svc.Version) {
			return true
		}
	}
	return false
}

func (f serviceFilter) apply(services []*ir.Service) []*ir.Service {
	if !f.set() {
		return services
	}
	var out []*ir.Service
	for _, svc := range services {
		if f.match(svc) {
			out = append(out, svc)
		}
	}
	return out
}

// openAPIMethods are the operation keys of an OpenAPI path item.
var openAPIMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace", "query"}

// deprecateOpenAPI returns doc as a JSON object with `deprecated: true`
// on every operation. doc is not modified (it may be the service's
// source spec).
func deprecateOpenAPI(doc any) (map[string]any, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	paths, _ := m["paths"].(map[string]any)
	for _, item := range paths {
		pi, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, method := range openAPIMethods {
			if op, ok := pi[method].(map[string]any); ok {
				op["deprecated"] = true
			}
		}
		if extra, ok := pi["additionalOperations"].(map[string]any); ok {
			for _, v := range extra {
				if op, ok := v.(map[string]any); ok {
					op["deprecated"] = true
				}
			}
		}
	}
	return m, nil
}

// deprecateProtoFile returns a copy of fp with the deprecated option
// on the file, its services and their methods. Proto options carry no
// reason.
func deprecateProtoFile(fp *descriptorpb.FileDescriptorProto) *descriptorpb.FileDescriptorProto {
	fp = proto.Clone(fp).(*descriptorpb.FileDescriptorProto)
	if fp.Options == nil {
		fp.Options = &descriptorpb.FileOptions{}
	}
	fp.Options.Deprecated = proto.Bool(true)
	for _, sd := range fp.Service {
		if sd.Options == nil {
			sd.Options = &descriptorpb.ServiceOptions{}
		}
		sd.Options.Deprecated = proto.Bool(true)
		for _, md := range sd.Method {
			if md.Options == nil {
				md.Options = &descriptorpb.MethodOptions{}
			}
			md.Options.Deprecated = proto.Bool(true)
		}
	}
	return fp
}

// renderProto renders services as a FileDescriptorSet, deprecated
// ones (deps) with the deprecated option. Same files, order and
// dedupe as ir.RenderProtoFiles(services).
func renderProto(services []*ir.Service, deps map[*ir.Service]string) (*descriptorpb.FileDescriptorSet, error) {
	if len(deps) == 0 {
		return ir.RenderProtoFiles(services)
	}
	fds := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	for _, svc := range services {
		one, err := ir.RenderProtoFiles([]*ir.Service{svc})
		if err != nil {
			return nil, err
		}
		for _, fp := range one.File {
			if seen[fp.GetName()] {
				continue
			}
			seen[fp.GetName()] = true
			if deps[svc] != "" {
				fp = deprecateProtoFile(fp)
			}
			fds.File = append(fds.File, fp)
		}
	}
	return fds, nil
}
