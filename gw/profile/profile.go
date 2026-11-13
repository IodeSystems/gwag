// Package profile reads and checks a service profile: the file that
// says what a service provides to gwag (versions, spec kind, spec
// file, the URLs where the running service serves its spec and its
// health check), what it depends on and which event channels it
// listens on.
//
// One file is the source of truth for registration. A service with a
// registration client loads it at start (controlclient.LoadProfile,
// @gwag/registration loadProfile, gwag_registration.load_profile)
// and pushes it with Register. An external or legacy service serves
// it as JSON at WellKnownPath, and the gateway pulls it
// (`gwag ctl services add --profile-url`). `gwag ctl services init` writes it,
// `gwag ctl services check` lints it and checks a running service against
// it.
//
//	service: billing
//	owner: {team: payments, contact: "#payments"}
//	health: /healthz
//	provides:
//	  v1:
//	    kind: openapi
//	    file: api/billing-v1.json
//	    url: /api/schema/openapi?version=v1
//	depends:
//	  library: [v1, v2]
//	subscribes:
//	  - {channel: orders.v1.OrderEvents, group: billing}
//
// Stability: experimental
package profile

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// WellKnownPath is where a service serves its profile as JSON, for
// the gateway to pull.
const WellKnownPath = "/.well-known/gwag.json"

// DefaultFile is the profile file name `gwag ctl services init` writes and
// the other commands read when --profile is not given.
const DefaultFile = "gwag.yaml"

// Spec kinds.
const (
	KindProto   = "proto"
	KindOpenAPI = "openapi"
	KindGraphQL = "graphql"
)

// Schema is the JSON Schema (draft 2020-12) of the profile file.
//
//go:embed schema.json
var Schema []byte

// Profile is one service's profile.
type Profile struct {
	// SchemaRef is the optional "$schema" key, for editors.
	SchemaRef string `json:"$schema,omitempty" yaml:"$schema,omitempty"`
	// Service is the gateway namespace.
	Service string `json:"service" yaml:"service"`
	Owner   *Owner `json:"owner,omitempty" yaml:"owner,omitempty"`
	// Public shows the service to admin readers without a token.
	Public bool `json:"public,omitempty" yaml:"public,omitempty"`
	// Address is where the gateway sends calls: host:port for proto,
	// the base URL for openapi. Pull mode only; a registration client
	// passes its own address. Empty in pull mode = the origin of the
	// profile URL.
	Address string `json:"address,omitempty" yaml:"address,omitempty"`
	// Health is the default health URL of every provided version.
	Health string `json:"health,omitempty" yaml:"health,omitempty"`
	// Provides maps a version ("v1", "unstable") to its spec.
	Provides map[string]Provide `json:"provides" yaml:"provides"`
	// Depends maps a provider service to the lines this service
	// ships clients for.
	Depends    map[string][]string `json:"depends,omitempty" yaml:"depends,omitempty"`
	Subscribes []Subscription      `json:"subscribes,omitempty" yaml:"subscribes,omitempty"`
}

// Owner is the team that owns the service.
type Owner struct {
	Team    string `json:"team,omitempty" yaml:"team,omitempty"`
	Contact string `json:"contact,omitempty" yaml:"contact,omitempty"`
}

// Subscription is one event channel the service listens on.
type Subscription struct {
	Channel string `json:"channel" yaml:"channel"`
	Group   string `json:"group,omitempty" yaml:"group,omitempty"`
}

// Provide is one provided version.
type Provide struct {
	// Kind is proto, openapi or graphql.
	Kind string `json:"kind" yaml:"kind"`
	// File is the spec file, relative to the profile file: the
	// entry .proto, or the OpenAPI document. Read by registration
	// clients and `service check`; not used in pull mode.
	File string `json:"file,omitempty" yaml:"file,omitempty"`
	// Imports are proto import directories, relative to the profile
	// file. Default: the directory of File.
	Imports []string `json:"imports,omitempty" yaml:"imports,omitempty"`
	// URL is where the running service serves this version's spec
	// (a path, or an absolute URL on the service's host). The gateway
	// fetches it to verify each instance; in pull mode it is also
	// the spec the gateway registers.
	URL string `json:"url,omitempty" yaml:"url,omitempty"`
	// Endpoint is the GraphQL endpoint (graphql only): path or
	// absolute URL.
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	// Health overrides Profile.Health for this version.
	Health string `json:"health,omitempty" yaml:"health,omitempty"`
	// Snapshot is the committed snapshot of this version's spec
	// (`gwag ctl schema snapshot update`), relative to the profile file.
	// `gwag ctl services check` compares File against it: a breaking
	// change fails the check. Optional.
	Snapshot string `json:"snapshot,omitempty" yaml:"snapshot,omitempty"`
}

// HealthURL is the health URL of version v: its own, else the
// profile default.
func (p *Profile) HealthURL(v string) string {
	if h := p.Provides[v].Health; h != "" {
		return h
	}
	return p.Health
}

// Versions returns the provided versions, sorted ("unstable" last).
func (p *Profile) Versions() []string {
	out := make([]string, 0, len(p.Provides))
	for v := range p.Provides {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[i], out[j]) })
	return out
}

// DependsSorted returns Depends as a list sorted by service.
func (p *Profile) DependsSorted() []Dependency {
	out := make([]Dependency, 0, len(p.Depends))
	for s, l := range p.Depends {
		out = append(out, Dependency{Service: s, Lines: append([]string(nil), l...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// Dependency is one entry of Depends.
type Dependency struct {
	Service string
	Lines   []string
}

func versionLess(a, b string) bool {
	na, nb := versionN(a), versionN(b)
	if na != nb {
		return na < nb
	}
	return a < b
}

// versionN orders vN by N; anything else sorts after.
func versionN(v string) int {
	if m := versionRe.FindStringSubmatch(v); m != nil && m[1] != "" {
		n := 0
		fmt.Sscanf(m[1], "%d", &n)
		return n
	}
	return 1 << 30
}

var (
	versionRe   = regexp.MustCompile(`^(?:v([1-9][0-9]*)|unstable)$`)
	namespaceRe = regexp.MustCompile(`^[^.*>/$\s]+$`)
)

// Parse reads a profile from YAML or JSON (JSON is YAML). Unknown
// keys are an error, so a typo does not pass silently.
func Parse(data []byte) (*Profile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	return &p, nil
}

// Load reads and parses the profile file at path. It does not
// validate; call Validate.
func Load(path string) (*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// JSON renders the profile as indented JSON: the body to serve at
// WellKnownPath.
func (p *Profile) JSON() ([]byte, error) {
	return json.MarshalIndent(p, "", "  ")
}

// YAML renders the profile as YAML.
func (p *Profile) YAML() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Problem is one validation finding.
type Problem struct {
	Path    string // e.g. "provides.v1.kind"
	Message string
}

func (p Problem) String() string { return p.Path + ": " + p.Message }

// Problems is the error Validate returns.
type Problems []Problem

func (ps Problems) Error() string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.String()
	}
	return strings.Join(parts, "; ")
}

// Mode says what a profile is validated for.
type Mode int

const (
	// ModePush: loaded by a registration client or `service check`.
	// Each proto / openapi version needs File.
	ModePush Mode = iota
	// ModePull: served at WellKnownPath for the gateway to pull.
	// Each proto / openapi version needs URL; File is ignored.
	ModePull
)

// Validate checks the profile's structure: names, versions, kinds,
// the files or URLs each kind needs, depends and subscribes. It
// returns Problems (nil when valid).
func (p *Profile) Validate(mode Mode) error {
	var ps Problems
	add := func(path, format string, args ...any) {
		ps = append(ps, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	if p.Service == "" {
		add("service", "required")
	} else if !namespaceRe.MatchString(p.Service) {
		add("service", "%q: must not contain '.', '*', '>', '/', '$' or spaces", p.Service)
	}
	if len(p.Provides) == 0 {
		add("provides", "at least one version required")
	}
	for _, v := range p.Versions() {
		pv := p.Provides[v]
		path := "provides." + v
		if !versionRe.MatchString(v) {
			add(path, "version %q: want vN (N >= 1) or unstable", v)
		}
		switch pv.Kind {
		case KindProto, KindOpenAPI:
			if mode == ModePush && pv.File == "" {
				add(path+".file", "required for kind %s", pv.Kind)
			}
			if mode == ModePull && pv.URL == "" {
				add(path+".url", "required for kind %s in a served profile", pv.Kind)
			}
			if pv.Endpoint != "" {
				add(path+".endpoint", "only for kind graphql")
			}
		case KindGraphQL:
			if pv.Endpoint == "" {
				add(path+".endpoint", "required for kind graphql")
			}
			if pv.File != "" || pv.URL != "" {
				add(path, "kind graphql takes endpoint, not file or url")
			}
		case "":
			add(path+".kind", "required: proto, openapi or graphql")
		default:
			add(path+".kind", "%q: want proto, openapi or graphql", pv.Kind)
		}
		if len(pv.Imports) > 0 && pv.Kind != KindProto {
			add(path+".imports", "only for kind proto")
		}
		for _, u := range []struct{ name, v string }{{"url", pv.URL}, {"endpoint", pv.Endpoint}, {"health", pv.Health}} {
			if u.v != "" {
				if err := checkRef(u.v); err != nil {
					add(path+"."+u.name, "%v", err)
				}
			}
		}
	}
	if p.Health != "" {
		if err := checkRef(p.Health); err != nil {
			add("health", "%v", err)
		}
	}
	for _, d := range p.DependsSorted() {
		path := "depends." + d.Service
		if !namespaceRe.MatchString(d.Service) {
			add(path, "service %q: not a valid namespace", d.Service)
		}
		if d.Service == p.Service {
			add(path, "a service cannot depend on itself")
		}
		if len(d.Lines) == 0 {
			add(path, "at least one line required")
		}
		for _, l := range d.Lines {
			if !versionRe.MatchString(l) {
				add(path, "line %q: want vN (N >= 1) or unstable", l)
			}
		}
	}
	seen := map[Subscription]bool{}
	for i, s := range p.Subscribes {
		path := fmt.Sprintf("subscribes[%d]", i)
		if s.Channel == "" || !strings.Contains(s.Channel, ".") {
			add(path+".channel", "%q: want <proto package>.<channel name>", s.Channel)
		}
		if seen[s] {
			add(path, "listed twice")
		}
		seen[s] = true
	}
	if p.Owner != nil && p.Owner.Team == "" && p.Owner.Contact == "" {
		add("owner", "empty; set team or contact, or drop owner")
	}
	if len(ps) == 0 {
		return nil
	}
	return ps
}

// checkRef accepts a path ("/healthz") or an absolute http(s) URL.
func checkRef(s string) error {
	if strings.HasPrefix(s, "/") {
		return nil
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return nil
	}
	return fmt.Errorf("%q: want a path starting with / or an http(s) URL", s)
}

// ReadSpec reads version v's spec file, relative to dir (the
// profile file's directory). Proto returns the entry source and the
// other .proto files under the import directories, keyed by import
// path.
func (p *Profile) ReadSpec(dir, v string) (entry []byte, imports map[string][]byte, err error) {
	pv, ok := p.Provides[v]
	if !ok {
		return nil, nil, fmt.Errorf("profile: no version %q", v)
	}
	if pv.File == "" {
		return nil, nil, fmt.Errorf("profile: provides.%s: no file", v)
	}
	file := pv.File
	if !filepath.IsAbs(file) {
		file = filepath.Join(dir, file)
	}
	entry, err = os.ReadFile(file)
	if err != nil {
		return nil, nil, err
	}
	if pv.Kind != KindProto {
		return entry, nil, nil
	}
	dirs := pv.Imports
	if len(dirs) == 0 {
		dirs = []string{filepath.Dir(pv.File)}
	}
	imports = map[string][]byte{}
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			d = filepath.Join(dir, d)
		}
		err := filepath.WalkDir(d, func(path string, de os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() || filepath.Ext(path) != ".proto" {
				return nil
			}
			rel, err := filepath.Rel(d, path)
			if err != nil {
				return err
			}
			if same(path, file) {
				return nil
			}
			key := filepath.ToSlash(rel)
			if _, dup := imports[key]; dup {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			imports[key] = b
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
	}
	return entry, imports, nil
}

func same(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && aa == bb
}
