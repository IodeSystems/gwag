package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const clusterStateUsage = `Usage:
  gwag ctl cluster freeze   [--ttl 10m] [--note TEXT]
  gwag ctl cluster unfreeze
  gwag ctl cluster export   [--allow-unfrozen] > state.json
  gwag ctl cluster import   [--overwrite] [--dry-run] state.json   (- reads stdin)

State copy of a cutover (docs/cutover.md). freeze makes every node of
the cluster refuse the admin writes that change copied state (stable,
deprecated, app and system keys, external dependents, Schema Registry
writes) until --ttl passes or unfreeze; registrations and traffic go
on. export prints the cluster state document (one JSON object, "v": 1)
and refuses unless the cluster is frozen. import applies a document;
an item held with another value is a conflict, and nothing is written
unless --overwrite. Registrations are not copied: services register
with both clusters.

freeze and unfreeze need the admin token; export and import the admin
token or a system key (gwag ctl key create --system).

Flags: --gateway URL (the gateway's HTTP endpoint; default: .gw
primary endpoint, else http://localhost:8080; --endpoint is the same),
--bearer TOKEN (default: from .gw), --context NAME.`

// clusterStateCmd runs `gwag ctl cluster freeze|unfreeze|export|import`.
func clusterStateCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, clusterStateUsage)
		return 2
	}
	verb := args[0]
	flags, positionals := splitBoolFlags(args[1:], "allow-unfrozen", "overwrite", "dry-run")
	fs := flag.NewFlagSet("cluster "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	gwURL := fs.String("gateway", "", "Gateway HTTP endpoint (default: from .gw primary, else http://localhost:8080)")
	endpoint := fs.String("endpoint", "", "Same as --gateway")
	bearer := fs.String("bearer", "", "Admin token or system key (default: from .gw)")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json (default: primary)")
	ttl := fs.String("ttl", "", "How long the freeze holds: 10m (default), 1h, 1d; at most 24h (freeze)")
	note := fs.String("note", "", "Shown in the refusals while frozen (freeze)")
	allowUnfrozen := fs.Bool("allow-unfrozen", false, "Export although the cluster is not frozen; the document carries a warning (export)")
	overwrite := fs.Bool("overwrite", false, "Take the document's value for every conflicting item; Schema Registry conflicts still refuse (import)")
	dryRun := fs.Bool("dry-run", false, "Report what the import would do; write nothing (import)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, clusterStateUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	login := resolveLogin(*ctxName)
	u := *gwURL
	if u == "" {
		u = *endpoint
	}
	u = resolveCtx(u, login.Endpoint, "http://localhost:8080")
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	root := strings.TrimRight(u, "/") + "/api/admin/cluster"
	tok := resolveCtx(*bearer, login.Bearer, "")

	switch verb {
	case "freeze":
		if len(positionals) > 0 {
			fs.Usage()
			return 2
		}
		body, _ := json.Marshal(map[string]any{"ttl": *ttl, "note": *note})
		var out struct {
			Until string `json:"until"`
			SetBy string `json:"setBy"`
		}
		if err := adminCall(http.MethodPost, root+"/freeze", tok, body, &out, 15*time.Second); err != nil {
			fmt.Fprintf(stderr, "cluster freeze: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "frozen until %s\n", out.Until)
		return 0
	case "unfreeze":
		if len(positionals) > 0 {
			fs.Usage()
			return 2
		}
		var out struct {
			WasFrozen bool `json:"wasFrozen"`
		}
		if err := adminCall(http.MethodPost, root+"/unfreeze", tok, nil, &out, 15*time.Second); err != nil {
			fmt.Fprintf(stderr, "cluster unfreeze: %v\n", err)
			return 1
		}
		if out.WasFrozen {
			fmt.Fprintln(stdout, "unfrozen")
		} else {
			fmt.Fprintln(stdout, "was not frozen")
		}
		return 0
	case "export":
		if len(positionals) > 0 {
			fs.Usage()
			return 2
		}
		q := ""
		if *allowUnfrozen {
			q = "?allowUnfrozen=true"
		}
		var doc map[string]any
		if err := adminCall(http.MethodGet, root+"/state"+q, tok, nil, &doc, 2*time.Minute); err != nil {
			fmt.Fprintf(stderr, "cluster export: %v\n", err)
			return 1
		}
		delete(doc, "$schema")
		if ws, ok := doc["warnings"].([]any); ok {
			for _, w := range ws {
				fmt.Fprintf(stderr, "warning: %v\n", w)
			}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			fmt.Fprintf(stderr, "cluster export: %v\n", err)
			return 1
		}
		return 0
	case "import":
		if len(positionals) != 1 {
			fs.Usage()
			return 2
		}
		var raw []byte
		var err error
		if positionals[0] == "-" {
			raw, err = io.ReadAll(stdin)
		} else {
			raw, err = os.ReadFile(positionals[0])
		}
		if err != nil {
			fmt.Fprintf(stderr, "cluster import: %v\n", err)
			return 1
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			fmt.Fprintf(stderr, "cluster import: %s: %v\n", positionals[0], err)
			return 1
		}
		delete(doc, "$schema")
		body, _ := json.Marshal(map[string]any{"state": doc, "overwrite": *overwrite, "dryRun": *dryRun})
		var out struct {
			DryRun             bool           `json:"dryRun"`
			Stable             importSection  `json:"stable"`
			Deprecated         importSection  `json:"deprecated"`
			Keys               importSection  `json:"keys"`
			ExternalDependents importSection  `json:"externalDependents"`
			SchemaRegistry     map[string]int `json:"schemaRegistry"`
			Conflicts          []string       `json:"conflicts"`
			Warnings           []string       `json:"warnings"`
		}
		if err := adminCall(http.MethodPost, root+"/state", tok, body, &out, 2*time.Minute); err != nil {
			fmt.Fprintf(stderr, "cluster import: %v\n", err)
			return 1
		}
		for _, w := range out.Warnings {
			fmt.Fprintf(stderr, "warning: %s\n", w)
		}
		verb := "imported"
		if out.DryRun {
			verb = "dry run"
		}
		fmt.Fprintf(stdout, "%s: stable %s; deprecated %s; keys %s; external dependents %s; schema registry %d schemas, %d versions added, %d unchanged\n",
			verb, out.Stable, out.Deprecated, out.Keys, out.ExternalDependents,
			out.SchemaRegistry["schemasAdded"], out.SchemaRegistry["versionsAdded"], out.SchemaRegistry["unchanged"])
		for _, c := range out.Conflicts {
			what := "conflict"
			if *overwrite && !out.DryRun {
				what = "overwritten"
			}
			fmt.Fprintf(stdout, "  %s: %s\n", what, c)
		}
		return 0
	}
	fs.Usage()
	return 2
}

// splitBoolFlags is splitFlagsAndPositionals that knows the named
// flags take no value, so --dry-run state.json keeps the file.
func splitBoolFlags(args []string, bools ...string) (flags, positionals []string) {
	isBool := func(a string) bool {
		name := strings.TrimLeft(a, "-")
		for _, b := range bools {
			if name == b {
				return true
			}
		}
		return false
	}
	var rest []string
	for _, a := range args {
		if isBool(a) {
			flags = append(flags, a)
			continue
		}
		rest = append(rest, a)
	}
	f, p := splitFlagsAndPositionals(rest)
	return append(flags, f...), p
}

type importSection struct {
	Added       int `json:"added"`
	Unchanged   int `json:"unchanged"`
	Overwritten int `json:"overwritten"`
}

func (s importSection) String() string {
	return fmt.Sprintf("%d added, %d unchanged, %d overwritten", s.Added, s.Unchanged, s.Overwritten)
}

// adminCall is keyCall with a timeout.
func adminCall(method, u, tok string, body []byte, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		var e struct {
			Detail string `json:"detail"`
			Title  string `json:"title"`
		}
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &e) == nil && (e.Detail != "" || e.Title != "") {
			msg = strings.TrimSpace(e.Title + ": " + e.Detail)
		}
		return fmt.Errorf("%s %s: %d %s", method, u, resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
