package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/iodesystems/gwag/v2/gw/profile"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// gwag service: the integration pattern for one service.
//
//	gwag service init   write the profile (gwag.yaml) from local specs
//	gwag service check  lint the profile; check a running service
//	                    against it; check depends on a gateway
//	gwag service wait   block until the gateway reports it green

const serviceUsage = `Usage:
  gwag service init  --spec [VERSION=]FILE ... [--service NS] [--url PATH] [--health PATH] [flags]
  gwag service check [--profile gwag.yaml] [--url http://svc:port] [--grpc-addr host:port] [--gateway HOST:PORT] [--json]
  gwag service wait  --service NS[:VERSION] [--instance ID] [--green] [--timeout 2m] [--gateway HOST:PORT] [--json]
  gwag service schema   print the JSON Schema of the profile file`

func serviceCmd(args []string) int {
	return runService(args, os.Stdout, os.Stderr)
}

func runService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, serviceUsage)
		return 2
	}
	switch args[0] {
	case "init":
		return serviceInit(args[1:], stdout, stderr)
	case "check":
		return serviceCheck(args[1:], stdout, stderr)
	case "wait":
		return serviceWait(args[1:], stdout, stderr)
	case "schema":
		_, _ = stdout.Write(profile.Schema)
		return 0
	}
	fmt.Fprintln(stderr, serviceUsage)
	return 2
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// ---------------------------------------------------------------------
// init
// ---------------------------------------------------------------------

func serviceInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("service init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var specs, depends, subs multiFlag
	fs.Var(&specs, "spec", "[VERSION=]FILE: a .proto entry file or an OpenAPI document (repeatable; VERSION defaults to v1, then v2, ...)")
	service := fs.String("service", "", "Namespace (default: the proto file stem or the OpenAPI info.title)")
	url := fs.String("url", "", "Where the running service serves each version's spec: a path or URL; {version} is replaced (e.g. /api/schema/openapi?version={version})")
	health := fs.String("health", "", "Health URL of the service (path or URL), e.g. /healthz")
	graphql := fs.String("graphql", "", "Provide a GraphQL endpoint instead of a spec file: [VERSION=]PATH|URL")
	fs.Var(&depends, "depends", "SERVICE:LINE[,LINE...] the service calls through the gateway (repeatable), e.g. library:v1,v2")
	fs.Var(&subs, "subscribe", "CHANNEL[@GROUP] the service listens on (repeatable), e.g. orders.v1.OrderEvents@billing")
	team := fs.String("owner-team", "", "Owner team")
	contact := fs.String("owner-contact", "", "Owner contact (email, chat channel, URL)")
	public := fs.Bool("public", false, "Show the service to admin readers without a token")
	address := fs.String("address", "", "Pull mode: where the gateway sends calls (default: the profile URL's origin)")
	imports := fs.String("I", "", "proto: comma-separated import directories (default: the directory of the spec)")
	out := fs.String("o", profile.DefaultFile, "Output file; .json writes JSON (for /.well-known/gwag.json), anything else YAML; - for stdout")
	force := fs.Bool("force", false, "Overwrite the output file")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gwag service init --spec [VERSION=]FILE ... [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if len(specs) == 0 && *graphql == "" {
		fmt.Fprintln(stderr, "service init: at least one --spec or --graphql is required")
		fs.Usage()
		return 2
	}
	base := "."
	if *out != "-" {
		base = filepath.Dir(*out)
	}
	p := &profile.Profile{
		Service:  *service,
		Public:   *public,
		Address:  *address,
		Health:   *health,
		Provides: map[string]profile.Provide{},
	}
	if *team != "" || *contact != "" {
		p.Owner = &profile.Owner{Team: *team, Contact: *contact}
	}
	next := 1
	nextVersion := func() string {
		for {
			v := fmt.Sprintf("v%d", next)
			next++
			if _, used := p.Provides[v]; !used {
				return v
			}
		}
	}
	for _, s := range specs {
		ver, file := "", s
		if i := strings.Index(s, "="); i >= 0 {
			ver, file = s[:i], s[i+1:]
		}
		if ver == "" {
			ver = nextVersion()
		}
		if _, dup := p.Provides[ver]; dup {
			fmt.Fprintf(stderr, "service init: version %s given twice\n", ver)
			return 2
		}
		kind, ns, err := inspectSpec(file)
		if err != nil {
			fmt.Fprintf(stderr, "service init: %s: %v\n", file, err)
			return 1
		}
		if p.Service == "" {
			p.Service = ns
		}
		rel := relTo(base, file)
		pv := profile.Provide{Kind: kind, File: filepath.ToSlash(rel)}
		if kind == profile.KindProto && *imports != "" {
			for _, d := range strings.Split(*imports, ",") {
				pv.Imports = append(pv.Imports, filepath.ToSlash(relTo(base, d)))
			}
		}
		if *url != "" {
			pv.URL = strings.ReplaceAll(*url, "{version}", ver)
		}
		p.Provides[ver] = pv
	}
	if *graphql != "" {
		ver, ep := "", *graphql
		if i := strings.Index(ep, "="); i >= 0 && !strings.Contains(ep[:i], "/") {
			ver, ep = ep[:i], ep[i+1:]
		}
		if ver == "" {
			ver = nextVersion()
		}
		p.Provides[ver] = profile.Provide{Kind: profile.KindGraphQL, Endpoint: ep}
	}
	for _, d := range depends {
		svc, lines, ok := strings.Cut(d, ":")
		if !ok || svc == "" || lines == "" {
			fmt.Fprintf(stderr, "service init: --depends %q: want SERVICE:LINE[,LINE...]\n", d)
			return 2
		}
		if p.Depends == nil {
			p.Depends = map[string][]string{}
		}
		p.Depends[svc] = append(p.Depends[svc], strings.Split(lines, ",")...)
	}
	for _, s := range subs {
		ch, group, _ := strings.Cut(s, "@")
		p.Subscribes = append(p.Subscribes, profile.Subscription{Channel: ch, Group: group})
	}
	if err := p.Validate(profile.ModePush); err != nil {
		fmt.Fprintf(stderr, "service init: the profile would be invalid:\n")
		for _, pr := range err.(profile.Problems) {
			fmt.Fprintf(stderr, "  %s\n", pr)
		}
		return 3
	}
	var body []byte
	var err error
	if strings.HasSuffix(*out, ".json") {
		body, err = p.JSON()
		body = append(body, '\n')
	} else {
		body, err = p.YAML()
		body = append([]byte("# gwag service profile. Schema: `gwag service schema`. Check: `gwag service check`.\n"), body...)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *out == "-" {
		_, _ = stdout.Write(body)
		return 0
	}
	if _, err := os.Stat(*out); err == nil && !*force {
		fmt.Fprintf(stderr, "service init: %s exists (pass --force to overwrite)\n", *out)
		return 1
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s: service %s, versions %s\n", *out, p.Service, strings.Join(p.Versions(), ", "))
	return 0
}

// relTo returns p relative to dir when p lies under dir, else p.
func relTo(dir, p string) string {
	ad, err1 := filepath.Abs(dir)
	ap, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return p
	}
	r, err := filepath.Rel(ad, ap)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return p
	}
	return r
}

var nsSanitize = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// inspectSpec returns the kind of a spec file and the namespace it
// suggests.
func inspectSpec(file string) (kind, ns string, err error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", "", err
	}
	if strings.EqualFold(filepath.Ext(file), ".proto") {
		return profile.KindProto, strings.TrimSuffix(filepath.Base(file), filepath.Ext(file)), nil
	}
	doc, err := openapi3.NewLoader().LoadFromData(b)
	if err != nil || doc.OpenAPI == "" {
		return "", "", fmt.Errorf("not a .proto file or an OpenAPI document")
	}
	if doc.Info != nil && doc.Info.Title != "" {
		ns = strings.Trim(strings.ToLower(nsSanitize.ReplaceAllString(doc.Info.Title, "_")), "_")
	}
	return profile.KindOpenAPI, ns, nil
}

// ---------------------------------------------------------------------
// check
// ---------------------------------------------------------------------

// Exit codes of `service check`, one per failure class. With several
// failures the lowest code wins (the most basic problem first).
const (
	checkExitOK          = 0
	checkExitProfile     = 3 // profile invalid, spec file unreadable, breaking vs snapshot
	checkExitUnreachable = 4 // address, gRPC, health
	checkExitSpec        = 5 // served spec or served profile differs
	checkExitDepends     = 6 // a declared dependency has no line on the gateway
)

type checkResult struct {
	Name   string `json:"name"`
	Class  string `json:"class"` // profile, reachability, spec, depends
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// Warning: reported, does not fail the check.
	Warning bool `json:"warning,omitempty"`
}

var classExit = map[string]int{
	"profile":      checkExitProfile,
	"reachability": checkExitUnreachable,
	"spec":         checkExitSpec,
	"depends":      checkExitDepends,
}

func serviceCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("service check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("profile", profile.DefaultFile, "Profile file")
	svcURL := fs.String("url", "", "Base URL of the running service (http://host:port): check health, served specs and the served profile")
	grpcAddr := fs.String("grpc-addr", "", "gRPC address of the running service for proto versions (default: the --url host:port)")
	wellKnown := fs.Bool("well-known", false, "Require the service to serve the profile at "+profile.WellKnownPath)
	gwAddr := fs.String("gateway", "", "Gateway control-plane address: check that each declared dependency has a line there")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json, for --gateway")
	jsonOut := fs.Bool("json", false, "Print the results as JSON")
	timeout := fs.Duration("timeout", 10*time.Second, "Timeout of each network check")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gwag service check [--profile gwag.yaml] [--url http://svc:port] [--grpc-addr host:port] [--gateway HOST:PORT] [--json]")
		fmt.Fprintln(stderr, "Exit codes: 0 ok; 3 profile; 4 service unreachable; 5 spec or profile differs; 6 dependency missing on the gateway; 1 error; 2 usage. Several failures: the lowest code.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var results []checkResult
	add := func(name, class string, err error) {
		r := checkResult{Name: name, Class: class, OK: err == nil}
		if err != nil {
			r.Detail = err.Error()
		}
		results = append(results, r)
	}
	warn := func(name, class, detail string) {
		results = append(results, checkResult{Name: name, Class: class, OK: true, Warning: true, Detail: detail})
	}
	finish := func() int {
		code := checkExitOK
		for _, r := range results {
			if !r.OK {
				if c := classExit[r.Class]; code == checkExitOK || c < code {
					code = c
				}
			}
		}
		if *jsonOut {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]any{"ok": code == 0, "exitCode": code, "checks": results})
			return code
		}
		for _, r := range results {
			mark := "ok  "
			switch {
			case !r.OK:
				mark = "FAIL"
			case r.Warning:
				mark = "warn"
			}
			line := fmt.Sprintf("%s %-12s %s", mark, r.Class, r.Name)
			if r.Detail != "" {
				line += ": " + r.Detail
			}
			fmt.Fprintln(stdout, line)
		}
		if code == 0 {
			fmt.Fprintln(stdout, "service check: ok")
		} else {
			fmt.Fprintf(stdout, "service check: failed (exit %d)\n", code)
		}
		return code
	}

	// 1. The profile itself.
	p, err := profile.Load(*path)
	if err != nil {
		add("profile "+*path, "profile", err)
		return finish()
	}
	if err := p.Validate(profile.ModePush); err != nil {
		var ps profile.Problems
		if errors.As(err, &ps) {
			for _, pr := range ps {
				add(pr.Path, "profile", errors.New(pr.Message))
			}
		} else {
			add("profile", "profile", err)
		}
		return finish()
	}
	add("profile "+*path, "profile", nil)
	dir := filepath.Dir(*path)

	// 2. Each spec file parses; snapshots, when configured.
	locals := map[string]localSpec{}
	for _, v := range p.Versions() {
		pv := p.Provides[v]
		if pv.Kind == profile.KindGraphQL {
			continue
		}
		data, imps, err := p.ReadSpec(dir, v)
		if err == nil {
			_, _, err = profile.Canonical(pv.Kind, data, imps, "")
		}
		add(fmt.Sprintf("provides.%s spec %s", v, pv.File), "profile", err)
		if err != nil {
			continue
		}
		locals[v] = localSpec{pv.Kind, data, imps}
		if pv.Snapshot != "" {
			res, detail := checkSnapshot(p, dir, v)
			switch res {
			case "equal":
				add(fmt.Sprintf("provides.%s snapshot %s", v, pv.Snapshot), "profile", nil)
			case "additive":
				warn(fmt.Sprintf("provides.%s snapshot %s", v, pv.Snapshot), "profile", "additive change; run `gwag schema snapshot update`: "+detail)
			default:
				add(fmt.Sprintf("provides.%s snapshot %s", v, pv.Snapshot), "profile", errors.New(detail))
			}
		}
	}

	ctx := context.Background()
	hc := &http.Client{Timeout: *timeout, CheckRedirect: profile.NoRedirects}

	// 3. The running service.
	if *svcURL != "" {
		if _, err := profile.Origin(*svcURL); err != nil {
			add("--url", "reachability", err)
			return finish()
		}
		cctx, cancel := context.WithTimeout(ctx, *timeout)
		err := profile.Reachable(cctx, *svcURL)
		cancel()
		add("address "+*svcURL, "reachability", err)
		if err == nil {
			checkRunning(ctx, p, locals, *svcURL, *grpcAddr, *wellKnown, *timeout, hc, add, warn)
		}
	}

	// 4. Depends on a gateway.
	if *gwAddr != "" || *ctxName != "" {
		checkDepends(p, *gwAddr, *ctxName, *timeout, add)
	}
	return finish()
}

// localSpec is one version's spec file, read.
type localSpec struct {
	kind    string
	data    []byte
	imports map[string][]byte
}

func checkRunning(ctx context.Context, p *profile.Profile, locals map[string]localSpec, svcURL, grpcAddr string, wellKnown bool, timeout time.Duration, hc *http.Client,
	add func(string, string, error), warn func(string, string, string)) {
	// Served profile.
	wk, err := profile.Resolve(svcURL, profile.WellKnownPath)
	if err == nil {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		body, ferr := profile.Fetch(cctx, hc, wk)
		cancel()
		switch {
		case ferr != nil && !wellKnown:
			warn("profile at "+profile.WellKnownPath, "spec", "not served ("+ferr.Error()+"); needed only when the gateway pulls the profile")
		case ferr != nil:
			add("profile at "+profile.WellKnownPath, "spec", ferr)
		default:
			add("profile at "+profile.WellKnownPath, "spec", sameProfile(p, body))
		}
	}
	// Health, once per distinct URL.
	seen := map[string]bool{}
	for _, v := range p.Versions() {
		h := p.HealthURL(v)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		u, err := profile.Resolve(svcURL, h)
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, timeout)
			err = profile.Health(cctx, hc, u)
			cancel()
		}
		add("health "+h, "reachability", err)
	}
	if len(seen) == 0 {
		warn("health", "reachability", "no health URL declared; the gateway checks only that the address accepts connections")
	}
	// gRPC for proto versions.
	hasProto := false
	for _, v := range p.Versions() {
		hasProto = hasProto || p.Provides[v].Kind == profile.KindProto
	}
	if hasProto {
		target := grpcAddr
		if target == "" {
			target, _ = profile.DialAddr(svcURL)
		}
		add("grpc "+target, "reachability", grpcReachable(target, timeout))
	}
	// Served specs.
	for _, v := range p.Versions() {
		pv := p.Provides[v]
		switch {
		case pv.Kind == profile.KindGraphQL:
			ep, err := profile.Resolve(svcURL, pv.Endpoint)
			if err == nil {
				cctx, cancel := context.WithTimeout(ctx, timeout)
				err = graphqlIntrospects(cctx, hc, ep)
				cancel()
			}
			add(fmt.Sprintf("provides.%s endpoint %s", v, pv.Endpoint), "spec", err)
		case pv.URL == "":
			warn(fmt.Sprintf("provides.%s url", v), "spec", "no url declared; the gateway does not compare the served spec")
		default:
			l, ok := locals[v]
			if !ok {
				continue
			}
			u, err := profile.Resolve(svcURL, pv.URL)
			if err == nil {
				cctx, cancel := context.WithTimeout(ctx, timeout)
				err = profile.SpecMatches(cctx, hc, u, l.kind, l.data, l.imports)
				cancel()
			}
			add(fmt.Sprintf("provides.%s spec at %s equals %s", v, pv.URL, pv.File), "spec", err)
		}
	}
}

// sameProfile compares a served profile with the local one, ignoring
// what only the local file can carry (file, imports, snapshot).
func sameProfile(local *profile.Profile, served []byte) error {
	sp, err := profile.Parse(served)
	if err != nil {
		return err
	}
	norm := func(p *profile.Profile) string {
		c := *p
		c.SchemaRef = ""
		c.Provides = map[string]profile.Provide{}
		for v, pv := range p.Provides {
			pv.File, pv.Imports, pv.Snapshot = "", nil, ""
			c.Provides[v] = pv
		}
		b, _ := json.Marshal(c)
		return string(b)
	}
	if a, b := norm(local), norm(sp); a != b {
		return fmt.Errorf("served profile differs from the local one (%s)", profile.Diff(prettyJSON(a), prettyJSON(b)))
	}
	return nil
}

func prettyJSON(s string) string {
	var v any
	_ = json.Unmarshal([]byte(s), &v)
	b, _ := json.MarshalIndent(v, "", " ")
	return string(b)
}

func grpcReachable(target string, timeout time.Duration) error {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn.Connect()
	for {
		st := conn.GetState()
		if st == connectivity.Ready {
			return nil
		}
		if !conn.WaitForStateChange(ctx, st) {
			return fmt.Errorf("not ready within %s (state %s)", timeout, st)
		}
	}
}

func graphqlIntrospects(ctx context.Context, hc *http.Client, endpoint string) error {
	body := strings.NewReader(`{"query":"{ __schema { queryType { name } } }"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Schema *struct{} `json:"__schema"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, profile.MaxSpecBytes)).Decode(&out); err != nil || out.Data.Schema == nil {
		return fmt.Errorf("no introspection result")
	}
	return nil
}

// checkSnapshot compares version v's spec file with its snapshot:
// "equal", "additive" or "breaking", with a detail line.
func checkSnapshot(p *profile.Profile, dir, v string) (string, string) {
	pv := p.Provides[v]
	file := filepath.Join(dir, pv.File)
	snap := filepath.Join(dir, pv.Snapshot)
	var imps []string
	for _, d := range pv.Imports {
		imps = append(imps, filepath.Join(dir, d))
	}
	gen, err := canonicalSpec(pv.Kind, file, imps)
	if err != nil {
		return "error", err.Error()
	}
	old, err := canonicalSpec(pv.Kind, snap, imps)
	if err != nil {
		return "error", "snapshot: " + err.Error()
	}
	if old == gen {
		return "equal", ""
	}
	changes, err := classifySnapshot(pv.Kind, old, gen)
	if err != nil {
		return "error", err.Error()
	}
	var breaking []string
	for _, c := range changes {
		if c.severity == "breaking" {
			breaking = append(breaking, c.msg)
		}
	}
	if len(breaking) > 0 {
		return "breaking", "breaking change against the snapshot: " + strings.Join(breaking, "; ")
	}
	return "additive", fmt.Sprintf("%d change(s)", len(changes))
}

// checkDepends asks the gateway which lines are present.
func checkDepends(p *profile.Profile, gwAddr, ctxName string, timeout time.Duration, add func(string, string, error)) {
	login := resolveLogin(ctxName)
	addr := resolveCtx(gwAddr, login.Gateway, "localhost:50090")
	resp, err := listServices(addr, login.Bearer, timeout)
	if err != nil {
		add("gateway "+addr, "depends", err)
		return
	}
	present := map[string]map[string]bool{}
	for _, s := range resp.GetServices() {
		if s.GetReplicaCount() == 0 {
			continue
		}
		if present[s.GetNamespace()] == nil {
			present[s.GetNamespace()] = map[string]bool{}
		}
		present[s.GetNamespace()][s.GetVersion()] = true
	}
	env := resp.GetEnvironment()
	if env == "" {
		env = addr
	}
	deps := p.DependsSorted()
	if len(deps) == 0 {
		add("depends on "+env, "depends", nil)
	}
	for _, d := range deps {
		var have []string
		for _, l := range d.Lines {
			if present[d.Service][l] {
				have = append(have, l)
			}
		}
		var err error
		if len(have) == 0 {
			var known []string
			for l := range present[d.Service] {
				known = append(known, l)
			}
			sort.Strings(known)
			err = fmt.Errorf("no accepted line [%s] is served on %s (served: [%s])", strings.Join(d.Lines, ", "), env, strings.Join(known, ", "))
		}
		add(fmt.Sprintf("depends.%s [%s]", d.Service, strings.Join(d.Lines, ", ")), "depends", err)
	}
}

func listServices(addr, bearer string, timeout time.Duration) (*cpv1.ListServicesResponse, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return cpv1.NewControlPlaneClient(conn).ListServices(withBearer(ctx, bearer), &cpv1.ListServicesRequest{})
}

// ---------------------------------------------------------------------
// wait
// ---------------------------------------------------------------------

// Exit codes of `service wait`.
const (
	waitExitGreen     = 0
	waitExitRed       = 3 // instances exist, not all green at the timeout
	waitExitNoneFound = 4 // no matching instance at the timeout
)

type waitInstance struct {
	Namespace      string   `json:"namespace"`
	Version        string   `json:"version"`
	RegistrationID string   `json:"registrationId"`
	InstanceID     string   `json:"instanceId,omitempty"`
	Addr           string   `json:"addr"`
	State          string   `json:"state"`
	Verification   string   `json:"verification"`
	Green          bool     `json:"green"`
	Reasons        []string `json:"reasons"`
}

func serviceWait(args []string, stdout, stderr io.Writer) int {
	flags, _ := splitFlagsAndPositionals(args)
	flags, _ = unswallowBoolFlags(flags, nil, "green", "json", "any")
	fs := flag.NewFlagSet("service wait", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gwAddr := fs.String("gateway", "", "Gateway control-plane address (default: from .gw primary or localhost:50090)")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json (default: primary)")
	svc := fs.String("service", "", "NAMESPACE or NAMESPACE:VERSION (required)")
	instance := fs.String("instance", "", "Only this instance: registration_id or instance_id")
	_ = fs.Bool("green", true, "Wait for green (the only mode; accepted for clarity in scripts)")
	anyGreen := fs.Bool("any", false, "Exit 0 when one matching instance is green, not all")
	timeout := fs.Duration("timeout", 2*time.Minute, "Give up after this long")
	interval := fs.Duration("interval", time.Second, "Poll interval")
	jsonOut := fs.Bool("json", false, "Print the final state as JSON")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gwag service wait --service NS[:VERSION] [--instance ID] [--green] [--any] [--timeout 2m] [--gateway HOST:PORT] [--json]")
		fmt.Fprintln(stderr, "Green: the instance is READY (start gate passed, dependencies present) and its verification is green or not declared.")
		fmt.Fprintln(stderr, "Exit codes: 0 green; 3 not green at the timeout (reasons printed); 4 no matching instance at the timeout; 1 error; 2 usage.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if *svc == "" {
		fs.Usage()
		return 2
	}
	ns, ver, _ := strings.Cut(*svc, ":")
	login := resolveLogin(*ctxName)
	addr := resolveCtx(*gwAddr, login.Gateway, "localhost:50090")
	deadline := time.Now().Add(*timeout)
	var last []waitInstance
	var lastErr error
	for {
		resp, err := listServices(addr, login.Bearer, 10*time.Second)
		lastErr = err
		if err == nil {
			last = matchInstances(resp, ns, ver, *instance)
			if len(last) > 0 && greenEnough(last, *anyGreen) {
				printWait(stdout, last, *jsonOut, "green")
				return waitExitGreen
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(*interval)
	}
	if len(last) == 0 {
		if lastErr != nil {
			fmt.Fprintf(stderr, "service wait: %s: %v\n", addr, lastErr)
		}
		printWait(stdout, last, *jsonOut, "no matching instance")
		fmt.Fprintf(stderr, "service wait: no instance of %s after %s\n", *svc, *timeout)
		return waitExitNoneFound
	}
	printWait(stdout, last, *jsonOut, "not green")
	fmt.Fprintf(stderr, "service wait: %s not green after %s\n", *svc, *timeout)
	return waitExitRed
}

func matchInstances(resp *cpv1.ListServicesResponse, ns, ver, instance string) []waitInstance {
	var out []waitInstance
	for _, s := range resp.GetServices() {
		if s.GetNamespace() != ns || (ver != "" && s.GetVersion() != ver) {
			continue
		}
		for _, in := range s.GetInstances() {
			if instance != "" && in.GetRegistrationId() != instance && in.GetInstanceId() != instance {
				continue
			}
			wi := waitInstance{
				Namespace:      s.GetNamespace(),
				Version:        s.GetVersion(),
				RegistrationID: in.GetRegistrationId(),
				InstanceID:     in.GetInstanceId(),
				Addr:           in.GetAddr(),
				State:          stateName(in.GetState()),
				Verification:   verificationName(in.GetVerification()),
				Reasons:        []string{},
			}
			ready := in.GetState() == cpv1.InstanceState_INSTANCE_STATE_READY
			v := in.GetVerification()
			verified := v == cpv1.VerificationState_VERIFICATION_STATE_GREEN || v == cpv1.VerificationState_VERIFICATION_STATE_UNSPECIFIED
			wi.Green = ready && verified
			for _, d := range in.GetDepends() {
				if len(d.GetPresentLines()) > 0 {
					continue
				}
				if d.GetChannel() != "" {
					wi.Reasons = append(wi.Reasons, "channel "+d.GetChannel()+" not served")
				} else {
					wi.Reasons = append(wi.Reasons, fmt.Sprintf("dependency %s [%s] has no ready instance", d.GetService(), strings.Join(d.GetLines(), ", ")))
				}
			}
			wi.Reasons = append(wi.Reasons, in.GetVerifyReasons()...)
			if v == cpv1.VerificationState_VERIFICATION_STATE_PENDING {
				wi.Reasons = append(wi.Reasons, "verification pending")
			}
			out = append(out, wi)
		}
	}
	return out
}

func greenEnough(l []waitInstance, anyOne bool) bool {
	n := 0
	for _, w := range l {
		if w.Green {
			n++
		}
	}
	if anyOne {
		return n > 0
	}
	return n == len(l)
}

func printWait(w io.Writer, l []waitInstance, asJSON bool, verdict string) {
	if asJSON {
		if l == nil {
			l = []waitInstance{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"verdict": verdict, "instances": l})
		return
	}
	for _, x := range l {
		mark := "red  "
		if x.Green {
			mark = "green"
		}
		id := x.InstanceID
		if id == "" {
			id = x.RegistrationID
		}
		fmt.Fprintf(w, "%s %s/%s %s state=%s verification=%s\n", mark, x.Namespace, x.Version, id, x.State, x.Verification)
		for _, r := range x.Reasons {
			fmt.Fprintf(w, "      %s\n", r)
		}
	}
	fmt.Fprintln(w, "service wait:", verdict)
}

func stateName(s cpv1.InstanceState) string {
	switch s {
	case cpv1.InstanceState_INSTANCE_STATE_READY:
		return "ready"
	case cpv1.InstanceState_INSTANCE_STATE_WAITING:
		return "waiting"
	case cpv1.InstanceState_INSTANCE_STATE_DEGRADED:
		return "degraded"
	}
	return "unknown"
}

func verificationName(s cpv1.VerificationState) string {
	switch s {
	case cpv1.VerificationState_VERIFICATION_STATE_PENDING:
		return "pending"
	case cpv1.VerificationState_VERIFICATION_STATE_GREEN:
		return "green"
	case cpv1.VerificationState_VERIFICATION_STATE_RED:
		return "red"
	}
	return "none"
}

// ---------------------------------------------------------------------
// services add / remove / profiles (pulled services)
// ---------------------------------------------------------------------

func servicesProfileCmd(args []string, stdout, stderr io.Writer) int {
	verb := args[0]
	flags, _ := splitFlagsAndPositionals(args[1:])
	fs := flag.NewFlagSet("services "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	gwAddr := fs.String("gateway", "", "Gateway control-plane address (default: from .gw primary or localhost:50090)")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json (default: primary)")
	profileURL := fs.String("profile-url", "", "URL of the service profile, e.g. http://legacy:8080"+profile.WellKnownPath)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if verb != "profiles" && *profileURL == "" {
		fmt.Fprintf(stderr, "Usage: gwag services %s --profile-url URL [--gateway HOST:PORT] [--context NAME]\n", verb)
		return 2
	}
	login := resolveLogin(*ctxName)
	addr := resolveCtx(*gwAddr, login.Gateway, "localhost:50090")
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintf(stderr, "dial %s: %v\n", addr, err)
		return 1
	}
	defer conn.Close()
	client := cpv1.NewControlPlaneClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ctx = withBearer(ctx, login.Bearer)
	switch verb {
	case "add":
		resp, err := client.AddServiceProfile(ctx, &cpv1.AddServiceProfileRequest{Url: *profileURL})
		if err != nil {
			fmt.Fprintf(stderr, "AddServiceProfile: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "added %s (service %s)\n", *profileURL, resp.GetService())
		for _, b := range resp.GetBindings() {
			fmt.Fprintf(stdout, "  %s/%s state=%s verification=%s\n", b.GetNamespace(), b.GetVersion(), stateName(b.GetState()), verificationName(b.GetVerification()))
			for _, r := range b.GetVerifyReasons() {
				fmt.Fprintf(stdout, "      %s\n", r)
			}
		}
		if len(resp.GetBindings()) == 0 {
			fmt.Fprintln(stdout, "  registered by another cluster node; see `gwag service wait`")
		}
	case "remove":
		resp, err := client.RemoveServiceProfile(ctx, &cpv1.RemoveServiceProfileRequest{Url: *profileURL})
		if err != nil {
			fmt.Fprintf(stderr, "RemoveServiceProfile: %v\n", err)
			return 1
		}
		if !resp.GetRemoved() {
			fmt.Fprintf(stdout, "no profile %s was stored\n", *profileURL)
			return 1
		}
		fmt.Fprintf(stdout, "removed %s\n", *profileURL)
	default:
		resp, err := client.ListServiceProfiles(ctx, &cpv1.ListServiceProfilesRequest{})
		if err != nil {
			fmt.Fprintf(stderr, "ListServiceProfiles: %v\n", err)
			return 1
		}
		if len(resp.GetProfiles()) == 0 {
			fmt.Fprintln(stdout, "(no pulled services)")
			return 0
		}
		fmt.Fprintf(stdout, "%-16s %-60s %s\n", "SERVICE", "URL", "ERROR")
		for _, p := range resp.GetProfiles() {
			fmt.Fprintf(stdout, "%-16s %-60s %s\n", p.GetService(), p.GetUrl(), p.GetError())
		}
	}
	return 0
}
