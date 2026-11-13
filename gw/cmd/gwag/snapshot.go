package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/IodeSystems/graphql-go/v2/language/ast"
	"github.com/IodeSystems/graphql-go/v2/language/parser"
	"github.com/IodeSystems/graphql-go/v2/language/printer"
	"github.com/IodeSystems/graphql-go/v2/language/source"
	"github.com/bufbuild/protocompile"
	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Exit codes of `schema snapshot check`. 1 and 2 are the usual error
// and usage codes of the other subcommands.
const (
	snapExitEqual    = 0
	snapExitAdditive = 3
	snapExitBreaking = 4
)

func schemaSnapshotCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "check" && args[0] != "update") {
		fmt.Fprintln(stderr, "Usage: gwag ctl schema snapshot (check|update) --spec FILE|URL --snapshot FILE [--format openapi|graphql|proto] [-I DIR]")
		return 2
	}
	verb := args[0]
	flags, _ := splitFlagsAndPositionals(args[1:])
	fs := flag.NewFlagSet("schema snapshot "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	spec := fs.String("spec", "", "Generated spec: file path or http(s) URL (required)")
	snap := fs.String("snapshot", "", "Committed snapshot file (required)")
	format := fs.String("format", "", "openapi, graphql or proto (default: from the --snapshot extension, then --spec)")
	imports := fs.String("I", "", "proto only: comma-separated import directories (default: the directory of --spec)")
	allow := fs.Bool("allow-breaking", false, "update only: write the snapshot even if the change is breaking")
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if *spec == "" || *snap == "" {
		fs.Usage()
		return 2
	}
	f := *format
	if f == "" {
		f = detectSnapFormat(*snap)
	}
	if f == "" {
		f = detectSnapFormat(*spec)
	}
	if f != "openapi" && f != "graphql" && f != "proto" {
		fmt.Fprintf(stderr, "cannot tell the format; pass --format openapi|graphql|proto\n")
		return 2
	}
	var imps []string
	if *imports != "" {
		imps = strings.Split(*imports, ",")
	}
	gen, err := canonicalSpec(f, *spec, imps)
	if err != nil {
		fmt.Fprintf(stderr, "spec %s: %v\n", *spec, err)
		return 1
	}
	old, err := canonicalSpec(f, *snap, imps)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !(missing && verb == "update") {
		fmt.Fprintf(stderr, "snapshot %s: %v\n", *snap, err)
		return 1
	}
	if missing {
		if err := writeSnapshot(*snap, gen); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "snapshot created: %s\n", *snap)
		return 0
	}
	if old == gen {
		fmt.Fprintln(stdout, "snapshot matches")
		if verb == "update" {
			// Rewrite anyway: the file may differ from the canonical form.
			if err := writeSnapshot(*snap, gen); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
		return snapExitEqual
	}
	changes, err := classifySnapshot(f, old, gen)
	if err != nil {
		fmt.Fprintf(stderr, "classify: %v\n", err)
		return 1
	}
	breaking := 0
	for _, c := range changes {
		if c.severity == "breaking" {
			breaking++
		}
	}
	if verb == "check" {
		fmt.Fprintf(stdout, "--- snapshot %s\n+++ generated %s\n%s", *snap, *spec, lineDiff(old, gen))
		printChanges(stdout, changes, breaking)
		if breaking > 0 {
			return snapExitBreaking
		}
		return snapExitAdditive
	}
	if breaking > 0 && !*allow {
		printChanges(stdout, changes, breaking)
		fmt.Fprintln(stderr, "refusing to update: breaking change (pass --allow-breaking to override)")
		return snapExitBreaking
	}
	if err := writeSnapshot(*snap, gen); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	printChanges(stdout, changes, breaking)
	fmt.Fprintf(stdout, "snapshot updated: %s\n", *snap)
	return 0
}

func printChanges(w io.Writer, changes []change, breaking int) {
	for _, c := range changes {
		fmt.Fprintf(w, "[%s] %s\n", strings.ToUpper(c.severity), c.msg)
	}
	if len(changes) == 0 {
		fmt.Fprintln(w, "(no schema-level changes; text differs only in details the classifier does not model)")
		return
	}
	fmt.Fprintf(w, "\n%d change(s); %d breaking\n", len(changes), breaking)
}

func writeSnapshot(path, canon string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(canon), 0o644)
}

func detectSnapFormat(p string) string {
	switch strings.ToLower(filepath.Ext(strings.SplitN(p, "?", 2)[0])) {
	case ".graphql", ".graphqls", ".gql":
		return "graphql"
	case ".yaml", ".yml":
		return "openapi"
	case ".proto", ".pb", ".binpb":
		return "proto"
	}
	return ""
}

// canonicalSpec loads src and returns its canonical text: SDL with
// definitions, fields, arguments and values sorted (graphql), JSON
// with sorted keys (openapi, and the FileDescriptorProto of the proto
// file in protojson form with source info dropped).
func canonicalSpec(format, src string, imports []string) (string, error) {
	switch format {
	case "graphql":
		txt, err := loadSchemaSource(src)
		if err != nil {
			return "", err
		}
		return canonGraphQL(txt)
	case "openapi":
		txt, err := loadSchemaSource(src)
		if err != nil {
			return "", err
		}
		return canonOpenAPI([]byte(txt))
	default:
		return canonProto(src, imports)
	}
}

func canonOpenAPI(b []byte) (string, error) {
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return "", err
	}
	if _, ok := v.(map[string]any); !ok {
		return "", errors.New("not an object")
	}
	return canonJSON(v)
}

func canonJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func canonGraphQL(sdl string) (string, error) {
	doc, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{Body: []byte(sdl)})})
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	sortArgs := func(a []*ast.InputValueDefinition) {
		sort.SliceStable(a, func(i, j int) bool { return a[i].Name.Value < a[j].Name.Value })
	}
	sortFields := func(a []*ast.FieldDefinition) {
		sort.SliceStable(a, func(i, j int) bool { return a[i].Name.Value < a[j].Name.Value })
		for _, f := range a {
			sortArgs(f.Arguments)
		}
	}
	sortNamed := func(a []*ast.Named) {
		sort.SliceStable(a, func(i, j int) bool { return a[i].Name.Value < a[j].Name.Value })
	}
	key := func(d ast.Node) string {
		switch x := d.(type) {
		case *ast.ObjectDefinition:
			return "1" + x.Name.Value
		case *ast.InterfaceDefinition:
			return "2" + x.Name.Value
		case *ast.UnionDefinition:
			return "3" + x.Name.Value
		case *ast.EnumDefinition:
			return "4" + x.Name.Value
		case *ast.InputObjectDefinition:
			return "5" + x.Name.Value
		case *ast.ScalarDefinition:
			return "6" + x.Name.Value
		case *ast.DirectiveDefinition:
			return "7" + x.Name.Value
		}
		return "0"
	}
	for _, def := range doc.Definitions {
		switch d := def.(type) {
		case *ast.ObjectDefinition:
			sortFields(d.Fields)
			sortNamed(d.Interfaces)
		case *ast.InterfaceDefinition:
			sortFields(d.Fields)
		case *ast.UnionDefinition:
			sortNamed(d.Types)
		case *ast.EnumDefinition:
			sort.SliceStable(d.Values, func(i, j int) bool { return d.Values[i].Name.Value < d.Values[j].Name.Value })
		case *ast.InputObjectDefinition:
			sortArgs(d.Fields)
		case *ast.DirectiveDefinition:
			sortArgs(d.Arguments)
		}
	}
	sort.SliceStable(doc.Definitions, func(i, j int) bool { return key(doc.Definitions[i]) < key(doc.Definitions[j]) })
	out, ok := printer.Print(doc).(string)
	if !ok {
		return "", errors.New("print failed")
	}
	return strings.TrimSpace(out) + "\n", nil
}

func canonProto(src string, imports []string) (string, error) {
	var fdp *descriptorpb.FileDescriptorProto
	ext := strings.ToLower(filepath.Ext(src))
	isURL := strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://")
	switch {
	case ext == ".proto" && !isURL:
		if _, err := os.Stat(src); err != nil {
			return "", err
		}
		if len(imports) == 0 {
			imports = []string{filepath.Dir(src)}
		}
		c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: imports})}
		base := filepath.Base(src)
		for _, dir := range imports {
			if rel, err := filepath.Rel(dir, src); err == nil && !strings.HasPrefix(rel, "..") {
				base = filepath.ToSlash(rel)
				break
			}
		}
		files, err := c.Compile(context.Background(), base)
		if err != nil {
			return "", err
		}
		fdp = protodesc.ToFileDescriptorProto(files[0])
	default:
		txt, err := loadSchemaSource(src)
		if err != nil {
			return "", err
		}
		set := &descriptorpb.FileDescriptorSet{}
		if t := strings.TrimSpace(txt); strings.HasPrefix(t, "{") {
			// A snapshot written by this tool holds one file; a
			// FileDescriptorSet in protojson holds "file": [...].
			if err := (protojson.UnmarshalOptions{}).Unmarshal([]byte(t), set); err != nil {
				one := &descriptorpb.FileDescriptorProto{}
				if err2 := (protojson.UnmarshalOptions{}).Unmarshal([]byte(t), one); err2 != nil {
					return "", err
				}
				set.File = []*descriptorpb.FileDescriptorProto{one}
			}
		} else if err := proto.Unmarshal([]byte(txt), set); err != nil {
			return "", fmt.Errorf("not a .proto source, FileDescriptorSet or protojson: %w", err)
		}
		if len(set.File) == 0 {
			return "", errors.New("empty FileDescriptorSet")
		}
		fdp = set.File[len(set.File)-1]
	}
	fdp = proto.Clone(fdp).(*descriptorpb.FileDescriptorProto)
	fdp.SourceCodeInfo = nil
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(fdp)
	if err != nil {
		return "", err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	return canonJSON(v)
}

// classifySnapshot labels the difference between two canonical texts.
// GraphQL reuses the `schema diff` model. OpenAPI and proto have no
// SDL path in the CLI, so they get a structural JSON comparison: a
// removed or changed element is breaking, an added one is info.
func classifySnapshot(format, old, gen string) ([]change, error) {
	if format == "graphql" {
		om, err := parseSchemaModel(old)
		if err != nil {
			return nil, err
		}
		nm, err := parseSchemaModel(gen)
		if err != nil {
			return nil, err
		}
		return diffModels(om, nm), nil
	}
	var o, n any
	if err := json.Unmarshal([]byte(old), &o); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(gen), &n); err != nil {
		return nil, err
	}
	var out []change
	jsonDiff(format == "proto", "", o, n, &out)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].severity != out[j].severity {
			return out[i].severity == "breaking"
		}
		return out[i].msg < out[j].msg
	})
	return out, nil
}

// Documentation-only OpenAPI keys: a change is info, never breaking.
var docKeys = map[string]bool{"description": true, "summary": true, "title": true, "example": true, "examples": true, "externalDocs": true}

func jsonDiff(isProto bool, path string, o, n any, out *[]change) {
	switch ov := o.(type) {
	case map[string]any:
		nv, ok := n.(map[string]any)
		if !ok {
			*out = append(*out, change{"breaking", "type changed: " + pathOr(path)})
			return
		}
		for k, a := range ov {
			b, ok := nv[k]
			if !ok {
				sev := "breaking"
				if !isProto && docKeys[k] {
					sev = "info"
				}
				*out = append(*out, change{sev, "removed: " + path + "/" + k})
				continue
			}
			jsonDiff(isProto, path+"/"+k, a, b, out)
		}
		for k := range nv {
			if _, ok := ov[k]; !ok {
				*out = append(*out, change{"info", "added: " + path + "/" + k})
			}
		}
	case []any:
		nv, ok := n.([]any)
		if !ok {
			*out = append(*out, change{"breaking", "type changed: " + pathOr(path)})
			return
		}
		diffArray(isProto, path, ov, nv, out)
	default:
		if fmt.Sprint(o) == fmt.Sprint(n) {
			return
		}
		last := path[strings.LastIndex(path, "/")+1:]
		if !isProto && docKeys[last] {
			*out = append(*out, change{"info", "doc changed: " + path})
			return
		}
		*out = append(*out, change{"breaking", fmt.Sprintf("changed: %s %v -> %v", path, o, n)})
	}
}

// elemKey identifies an array element: objects by name (plus "in" for
// OpenAPI parameters).
func elemKey(v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	name, ok := m["name"].(string)
	if !ok {
		return "", false
	}
	if in, ok := m["in"].(string); ok {
		return in + ":" + name, true
	}
	return name, true
}

func diffArray(isProto bool, path string, o, n []any, out *[]change) {
	keyed := true
	for _, e := range append(append([]any{}, o...), n...) {
		if _, ok := elemKey(e); !ok {
			keyed = false
			break
		}
	}
	if keyed && len(o)+len(n) > 0 {
		nm := map[string]any{}
		for _, e := range n {
			k, _ := elemKey(e)
			nm[k] = e
		}
		seen := map[string]bool{}
		for _, e := range o {
			k, _ := elemKey(e)
			seen[k] = true
			b, ok := nm[k]
			if !ok {
				*out = append(*out, change{"breaking", "removed: " + path + "[" + k + "]"})
				continue
			}
			jsonDiff(isProto, path+"["+k+"]", e, b, out)
		}
		for _, e := range n {
			k, _ := elemKey(e)
			if seen[k] {
				continue
			}
			sev := "info"
			if m := e.(map[string]any); m["required"] == true {
				sev = "breaking"
			}
			*out = append(*out, change{sev, "added: " + path + "[" + k + "]"})
		}
		return
	}
	if fmt.Sprint(o) == fmt.Sprint(n) {
		return
	}
	if isProto {
		// Scalar lists (dependency, reserved ranges): growth is additive.
		have := map[string]bool{}
		for _, e := range n {
			have[fmt.Sprint(e)] = true
		}
		all := true
		for _, e := range o {
			if !have[fmt.Sprint(e)] {
				all = false
			}
		}
		if all {
			*out = append(*out, change{"info", "list grew: " + pathOr(path)})
			return
		}
	}
	*out = append(*out, change{"breaking", "list changed: " + pathOr(path)})
}

func pathOr(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// lineDiff prints removed ("-") and added ("+") lines with a few lines
// of context. It trims the common head and tail, then runs an LCS on
// the middle; a very large middle is printed as one replaced block.
func lineDiff(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	p := 0
	for p < len(x) && p < len(y) && x[p] == y[p] {
		p++
	}
	s := 0
	for s < len(x)-p && s < len(y)-p && x[len(x)-1-s] == y[len(y)-1-s] {
		s++
	}
	mx, my := x[p:len(x)-s], y[p:len(y)-s]
	var sb strings.Builder
	ctx := func(lines []string) {
		for _, l := range lines {
			sb.WriteString("  " + l + "\n")
		}
	}
	head := x[:p]
	if len(head) > 3 {
		sb.WriteString("  ...\n")
		head = head[len(head)-3:]
	}
	ctx(head)
	if len(mx)*len(my) > 25_000_000 {
		for _, l := range mx {
			sb.WriteString("- " + l + "\n")
		}
		for _, l := range my {
			sb.WriteString("+ " + l + "\n")
		}
	} else {
		dp := make([][]int32, len(mx)+1)
		for i := range dp {
			dp[i] = make([]int32, len(my)+1)
		}
		for i := len(mx) - 1; i >= 0; i-- {
			for j := len(my) - 1; j >= 0; j-- {
				if mx[i] == my[j] {
					dp[i][j] = dp[i+1][j+1] + 1
				} else if dp[i+1][j] >= dp[i][j+1] {
					dp[i][j] = dp[i+1][j]
				} else {
					dp[i][j] = dp[i][j+1]
				}
			}
		}
		i, j := 0, 0
		for i < len(mx) || j < len(my) {
			switch {
			case i < len(mx) && j < len(my) && mx[i] == my[j]:
				sb.WriteString("  " + mx[i] + "\n")
				i++
				j++
			case i < len(mx) && (j == len(my) || dp[i+1][j] >= dp[i][j+1]):
				sb.WriteString("- " + mx[i] + "\n")
				i++
			default:
				sb.WriteString("+ " + my[j] + "\n")
				j++
			}
		}
	}
	tail := x[len(x)-s:]
	if len(tail) > 4 {
		ctx(tail[:3])
		sb.WriteString("  ...\n")
	} else {
		ctx(tail)
	}
	return sb.String()
}
