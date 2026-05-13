// gwag serve: boot the embedded gat translator (no NATS, no admin,
// no cluster) fronting a single upstream service described by an
// OpenAPI spec or a .proto file. The smallest possible "GraphQL over
// my existing service" CLI — useful for proxies, demos, and
// graphql-codegen targets backed by a single backend.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/iodesystems/gwag/gw/gat"
	"github.com/iodesystems/gwag/gw/ir"
)

func serveCmd(args []string) int {
	fs := flag.NewFlagSet("gwag serve", flag.ContinueOnError)
	openapiPath := fs.String("openapi", "", "Path to an OpenAPI 3.x spec (JSON or YAML)")
	protoPath := fs.String("proto", "", "Path to a .proto file")
	target := fs.String("to", "", "Upstream target — http(s):// URL for --openapi, host:port for --proto")
	addr := fs.String("addr", ":8080", "HTTP listen address")
	prefix := fs.String("prefix", "/", "URL prefix to mount /graphql + /schema/* under")
	namespace := fs.String("namespace", "", "Override the GraphQL namespace; defaults to spec-derived")
	version := fs.String("version", "", "Override the GraphQL version; defaults to spec-derived (\"v1\" for OpenAPI)")
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, "Usage: gwag serve {--openapi PATH | --proto PATH} --to TARGET [flags]")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Fronts one upstream service as GraphQL using the embedded gat translator.")
		fmt.Fprintln(out, "No NATS, no cluster, no admin endpoints — see `gwag up` for those.")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if (*openapiPath == "") == (*protoPath == "") {
		fmt.Fprintln(os.Stderr, "gwag serve: exactly one of --openapi or --proto is required")
		fs.Usage()
		return 2
	}
	if *target == "" {
		fmt.Fprintln(os.Stderr, "gwag serve: --to is required")
		fs.Usage()
		return 2
	}

	var (
		regs []gat.ServiceRegistration
		err  error
	)
	switch {
	case *openapiPath != "":
		regs, err = loadOpenAPIRegistration(*openapiPath, *target)
	case *protoPath != "":
		regs, err = gat.ProtoFile(*protoPath, *target)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gwag serve: %v\n", err)
		return 1
	}
	for i := range regs {
		if *namespace != "" {
			regs[i].Service.Namespace = *namespace
		}
		if *version != "" {
			regs[i].Service.Version = *version
		}
	}

	g, err := gat.New(regs...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gwag serve: build gateway: %v\n", err)
		return 1
	}

	mux := http.NewServeMux()
	if err := gat.RegisterHTTP(mux, g, *prefix); err != nil {
		fmt.Fprintf(os.Stderr, "gwag serve: register http: %v\n", err)
		return 1
	}

	pretty := strings.TrimRight(*prefix, "/")
	if pretty == "" {
		pretty = "/"
	}
	log.Printf("gwag serve: %s → %s", regs[0].Service.Namespace, *target)
	log.Printf("gwag serve: listening on %s; POST %s/graphql", *addr, strings.TrimRight(pretty, "/"))
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "gwag serve: %v\n", err)
		return 1
	}
	return 0
}

// loadOpenAPIRegistration reads spec from disk, parses it via
// kin-openapi, ingests into IR, populates schema IDs, and returns one
// ServiceRegistration pointing dispatch at the upstream target. The
// dispatcher path inside gat handles per-call HTTP forwarding.
func loadOpenAPIRegistration(path, target string) ([]gat.ServiceRegistration, error) {
	specBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	doc, err := loader.LoadFromData(specBytes)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	svc := ir.IngestOpenAPI(doc)
	if svc.Namespace == "" {
		svc.Namespace = openAPINamespaceFromDoc(doc, path)
	}
	if svc.Version == "" {
		svc.Version = "v1"
	}
	ir.PopulateSchemaIDs(svc)
	return []gat.ServiceRegistration{{
		Service: svc,
		BaseURL: target,
	}}, nil
}

// openAPINamespaceFromDoc derives a GraphQL-safe namespace string
// from the spec's Info.Title, falling back to the filename stem. The
// title path matches what gat does in the huma flow (sanitizeNamespace
// in register.go) — duplicated here so we don't re-export the helper.
func openAPINamespaceFromDoc(doc *openapi3.T, path string) string {
	if doc != nil && doc.Info != nil {
		if ns := sanitizeOpenAPINamespace(doc.Info.Title); ns != "" {
			return ns
		}
	}
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if ns := sanitizeOpenAPINamespace(stem); ns != "" {
		return ns
	}
	return "openapi"
}

// sanitizeOpenAPINamespace lower-cases s and drops characters that
// aren't valid in a GraphQL identifier. Matches gat's internal
// sanitizeNamespace; reproduced here so this command doesn't need to
// take a package-private dep.
func sanitizeOpenAPINamespace(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 0 && out[0] >= '0' && out[0] <= '9' {
		out = "_" + out
	}
	return out
}
