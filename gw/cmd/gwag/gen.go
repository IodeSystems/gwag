package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/iodesystems/gwag/v2/gw/eventgen"
)

// genCmd implements `gwag gen`: run the language's pinned protobuf
// generator over the .proto files, then write typed event code for
// the channels they declare. See docs/events.md.
func genCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lang := fs.String("lang", "", "Target language: "+strings.Join(eventgen.Languages(), ", "))
	out := fs.String("out", ".", "Output directory")
	moduleDir := fs.String("module-dir", ".", "Project root that pins the base generators (go: go.mod tool directive; python: pyproject.toml with uv)")
	grpc := fs.Bool("grpc", false, "Also generate gRPC service stubs (go: go tool protoc-gen-go-grpc)")
	imports := &stringListValue{}
	fs.Var(imports, "I", "Import path (repeatable; default .)")
	opts := &stringListValue{}
	fs.Var(opts, "opt", "Generator parameter, e.g. paths=source_relative (repeatable; passed to every generator)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gwag gen --lang LANG [--out DIR] [-I DIR]... [--opt K=V]... [--grpc] [--module-dir DIR] FILE.proto...")
		fmt.Fprintln(fs.Output(), "  Imports gwag/ps/v1/options.proto (or gw/proto/ps/v1/options.proto) need no copy on disk.")
		fs.PrintDefaults()
	}
	// Flags may follow file names.
	var files []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		files = append(files, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if *lang == "" || len(files) == 0 {
		fs.Usage()
		return 2
	}
	written, err := eventgen.Generate(context.Background(), eventgen.Config{
		Lang:        *lang,
		Out:         *out,
		ImportPaths: imports.values,
		Files:       files,
		Opts:        opts.values,
		ModuleDir:   *moduleDir,
		Grpc:        *grpc,
	})
	if err != nil {
		fmt.Fprintf(stderr, "gen: %v\n", err)
		return 1
	}
	for _, w := range written {
		fmt.Fprintln(stdout, w)
	}
	return 0
}
