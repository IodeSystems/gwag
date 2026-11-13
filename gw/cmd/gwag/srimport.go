package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	gateway "github.com/iodesystems/gwag/v2/gw"
	"github.com/iodesystems/gwag/v2/gw/internal/srserve"
)

// schemaRegistryPrefix is where gwag up mounts the registry.
const schemaRegistryPrefix = "/schema-registry"

// schemaImportCmd implements `gwag ctl schema import`: copy subjects,
// versions, ids and compatibility levels from a registry into a named
// context of the registry a gwag gateway serves, keeping the ids. The
// context is required: the default context is gwag's own id space.
func schemaImportCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("schema import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "Source Schema Registry URL (required)")
	into := fs.String("into", "", "Context of gwag's registry to import into, e.g. .corp (required; its own id space)")
	to := fs.String("to", "", "gwag registry URL (default: .gw primary endpoint + "+schemaRegistryPrefix+")")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json (default: primary)")
	bearer := fs.String("bearer", "", "gwag admin token (default: from .gw)")
	subjects := &stringListValue{}
	fs.Var(subjects, "subject", "Import only this subject and the subjects its references name (repeatable; default all)")
	deleted := fs.Bool("include-deleted", false, "Also import soft-deleted subjects and versions (served as deleted)")
	dry := fs.Bool("dry-run", false, "Read the source and print what would be sent; send nothing")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gwag ctl schema import --from URL --into CONTEXT [--to URL] [--subject S]... [--include-deleted] [--dry-run]")
		fmt.Fprintln(fs.Output(), "  Copies a registry into a context of gwag's (its own id space), keeping schema ids")
		fmt.Fprintln(fs.Output(), "  and versions, so records framed with the source's ids decode against gwag at")
		fmt.Fprintln(fs.Output(), "  <registry URL>/contexts/CONTEXT. Safe to run again: it adds what is new.")
		fmt.Fprintln(fs.Output(), "  Fails, writing nothing, when an id or version differs from what the context holds.")
		fmt.Fprintln(fs.Output(), "  Source auth: "+registryEnvHelp)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *from == "" || *into == "" || fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	intoCtx, err := srserve.NormContext(*into)
	if err == nil && intoCtx == srserve.DefaultContext {
		err = fmt.Errorf("the default context holds the ids gwag allocates; name another (e.g. .corp)")
	}
	if err != nil {
		fmt.Fprintf(stderr, "schema import: --into %q: %v\n", *into, err)
		return 2
	}
	login := resolveLogin(*ctxName)
	ctxURL := ""
	if login.Endpoint != "" {
		ctxURL = strings.TrimRight(login.Endpoint, "/") + schemaRegistryPrefix
	}
	target := strings.TrimRight(resolveCtx(*to, ctxURL, "http://localhost:8080"+schemaRegistryPrefix), "/")
	token := resolveCtx(*bearer, login.Bearer, "")

	src, err := registryClient(*from)
	if err != nil {
		fmt.Fprintf(stderr, "schema import: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	b, err := srserve.Export(ctx, src, subjects.values, *deleted)
	if err != nil {
		fmt.Fprintf(stderr, "schema import: read %s: %v\n", src.URL(), err)
		return 1
	}
	fmt.Fprintf(stdout, "read %s: %d schemas, %d subject versions\n", src.URL(), len(b.Schemas), len(b.Versions))
	if *dry {
		for _, v := range b.Versions {
			fmt.Fprintf(stdout, "  %s version %d id %d%s\n", v.Subject, v.Version, v.ID, map[bool]string{true: " (deleted)"}[v.Deleted])
		}
		return 0
	}
	body, err := json.Marshal(b)
	if err != nil {
		fmt.Fprintf(stderr, "schema import: %v\n", err)
		return 1
	}
	target += "/contexts/" + intoCtx
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"/gwag/import", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "schema import: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", srserve.ContentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "schema import: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code    int    `json:"error_code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		fmt.Fprintf(stderr, "schema import: %s: %d %s (error_code %d)\n", target, resp.StatusCode, e.Message, e.Code)
		return 1
	}
	var res srserve.ImportResult
	if err := json.Unmarshal(data, &res); err != nil {
		fmt.Fprintf(stderr, "schema import: %s: %v\n", target, err)
		return 1
	}
	fmt.Fprintf(stdout, "imported into %s: %d schemas added, %d versions added, %d unchanged; highest id %d\n",
		target, res.SchemasAdded, res.VersionsAdded, res.Unchanged, res.MaxID)
	for _, w := range res.Warnings {
		fmt.Fprintf(stdout, "warning: %s\n", w)
	}
	return 0
}

// schemaRegistryFlags are the --schema-registry* flags of gwag up and
// gwag serve.
type schemaRegistryFlags struct {
	on       *bool
	writes   *bool
	strategy *string
	topics   *stringListValue
}

func addSchemaRegistryFlags(fs *flag.FlagSet) *schemaRegistryFlags {
	f := &schemaRegistryFlags{
		on:       fs.Bool("schema-registry", false, "Serve a Confluent-compatible Schema Registry at "+schemaRegistryPrefix+" (reads open, READONLY unless --schema-registry-writes)"),
		writes:   fs.Bool("schema-registry-writes", false, "Let the admin token register schemas and set compatibility over the registry API (basic-auth password or bearer)"),
		strategy: fs.String("schema-registry-strategy", "topic", "Subject of a channel's message: topic (<topic>-value), record (<message>), topic-record (<topic>-<message>)"),
		topics:   &stringListValue{},
	}
	fs.Var(f.topics, "schema-registry-topic", "Kafka topic of a channel, CHANNEL_KEY=TOPIC (repeatable; default EV_<pkg>_<Name>)")
	return f
}

// option returns the gateway option, or nil when the registry is off.
func (f *schemaRegistryFlags) option() (gateway.Option, error) {
	if !*f.on {
		return nil, nil
	}
	o := gateway.SchemaRegistryOptions{Strategy: *f.strategy, Writes: *f.writes, Topics: map[string]string{}}
	for _, t := range f.topics.values {
		k, v, ok := strings.Cut(t, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("--schema-registry-topic %q: want CHANNEL_KEY=TOPIC", t)
		}
		o.Topics[k] = v
	}
	if _, err := sr.StrategyByName(o.Strategy); err != nil {
		return nil, err
	}
	return gateway.WithSchemaRegistry(o), nil
}

// mount adds the registry to mux when it is on.
func (f *schemaRegistryFlags) mount(mux *http.ServeMux, gw *gateway.Gateway) {
	if *f.on {
		mux.Handle(schemaRegistryPrefix+"/", http.StripPrefix(schemaRegistryPrefix, gw.SchemaRegistryHandler()))
	}
}
