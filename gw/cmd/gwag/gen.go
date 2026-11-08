package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
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
	moduleDir := fs.String("module-dir", ".", "Project root that pins the base generators (go: go.mod tool directive; ts: node_modules/.bin/protoc-gen-es; python: pyproject.toml with uv)")
	grpc := fs.Bool("grpc", false, "Also generate gRPC service stubs (go: go tool protoc-gen-go-grpc)")
	imports := &stringListValue{}
	fs.Var(imports, "I", "Import path (repeatable; default .)")
	opts := &stringListValue{}
	fs.Var(opts, "opt", "Generator parameter, e.g. paths=source_relative or target=ts (repeatable; passed to every generator; ts also reads events_import=MODULE)")
	registry := fs.String("registry", "", "Schema Registry URL to import subjects from (with --subject, or the subjects in --lock)")
	subjects := &stringListValue{}
	fs.Var(subjects, "subject", "Registry subject to import, SUBJECT[@VERSION] (repeatable; default version latest, pinned in the lock)")
	binds := &stringListValue{}
	fs.Var(binds, "bind", "Channel of an imported subject, SUBJECT=MESSAGE[,name=N][,pattern=P] (repeatable; default: the first message, its name, the topic of SUBJECT-value)")
	importDir := fs.String("import-dir", "imported", "Directory for imported schemas (read-only copies) and their lock; added to -I")
	lockPath := fs.String("lock", "", "Lock file (default IMPORT-DIR/gwag.lock.json)")
	checkLockF := fs.Bool("check-lock", false, "Compare the lock with the registry and the files on disk; exit 1 on a difference")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gwag gen --lang LANG [--out DIR] [-I DIR]... [--opt K=V]... [--grpc] [--module-dir DIR] FILE.proto...")
		fmt.Fprintln(fs.Output(), "       gwag gen --registry URL --subject SUBJECT[@VERSION]... [--bind ...] [--import-dir DIR] [--lang LANG ...] [FILE.proto...]")
		fmt.Fprintln(fs.Output(), "       gwag gen --check-lock [--import-dir DIR | --lock FILE] [--registry URL]")
		fmt.Fprintln(fs.Output(), "  Imports gwag/ps/v1/options.proto (or gw/proto/ps/v1/options.proto) need no copy on disk.")
		fmt.Fprintln(fs.Output(), "  "+registryEnvHelp)
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
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *lockPath == "" {
		*lockPath = filepath.Join(*importDir, "gwag.lock.json")
	}
	ctx := context.Background()
	if *checkLockF {
		return genCheckLock(ctx, *lockPath, *importDir, *registry, stdout, stderr)
	}
	// Imports: fetch from the registry, or read the pinned copies.
	var lock *lockFile
	switch {
	case *registry != "":
		specs := make([]importSpec, 0, len(subjects.values))
		for _, v := range subjects.values {
			specs = append(specs, parseSubject(v))
		}
		if len(specs) == 0 {
			// Re-import the pinned versions.
			l, err := readLock(*lockPath)
			if err != nil {
				fmt.Fprintf(stderr, "gen: --registry without --subject re-imports the lock: %v\n", err)
				return 2
			}
			for _, s := range l.Subjects {
				specs = append(specs, importSpec{subject: s.Subject, version: strconv.Itoa(s.Version), message: s.Message, name: s.Name, pattern: s.Pattern})
			}
		}
		for _, b := range binds.values {
			if err := parseBind(b, specs); err != nil {
				fmt.Fprintf(stderr, "gen: %v\n", err)
				return 2
			}
		}
		c, err := registryClient(*registry)
		if err != nil {
			fmt.Fprintf(stderr, "gen: %v\n", err)
			return 1
		}
		lock, err = importSchemas(ctx, c, *importDir, specs)
		if err == nil {
			err = writeLock(*lockPath, lock)
		}
		if err != nil {
			fmt.Fprintf(stderr, "gen: import: %v\n", err)
			return 1
		}
		for _, s := range lock.Subjects {
			fmt.Fprintf(stdout, "imported %s version %d id %d -> %s channel %s.%s pattern %q\n", s.Subject, s.Version, s.ID,
				filepath.Join(*importDir, s.Path), packageOf(s.Message), s.Name, s.Pattern)
		}
	case len(subjects.values) > 0 || len(binds.values) > 0:
		fmt.Fprintln(stderr, "gen: --subject and --bind need --registry")
		return 2
	case set["import-dir"] || set["lock"]:
		l, err := readLock(*lockPath)
		if err != nil {
			fmt.Fprintf(stderr, "gen: %v\n", err)
			return 1
		}
		lock = l
	}
	var bindings map[string]eventgen.Binding
	if lock != nil {
		var roots []string
		bindings, roots = lockBindings(lock)
		files = append(files, roots...)
		imports.values = append(imports.values, *importDir)
		if len(imports.values) == 1 {
			// -I defaults to "."; keep it next to the import dir.
			imports.values = []string{".", *importDir}
		}
		if *lang == "" {
			return 0 // import only
		}
	}
	if *lang == "" || len(files) == 0 {
		fs.Usage()
		return 2
	}
	written, err := eventgen.Generate(ctx, eventgen.Config{
		Lang:        *lang,
		Out:         *out,
		ImportPaths: imports.values,
		Files:       files,
		Opts:        opts.values,
		ModuleDir:   *moduleDir,
		Grpc:        *grpc,
		Bindings:    bindings,
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

func genCheckLock(ctx context.Context, lockPath, importDir, registry string, stdout, stderr io.Writer) int {
	l, err := readLock(lockPath)
	if err != nil {
		fmt.Fprintf(stderr, "gen: %v\n", err)
		return 1
	}
	if registry == "" {
		registry = l.Registry
	}
	c, err := registryClient(registry)
	if err != nil {
		fmt.Fprintf(stderr, "gen: %v\n", err)
		return 1
	}
	bad, err := checkLock(ctx, c, importDir, l, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gen: check-lock: %v\n", err)
		return 1
	}
	for _, b := range bad {
		fmt.Fprintf(stderr, "FAIL %s\n", b)
	}
	if len(bad) > 0 {
		return 1
	}
	fmt.Fprintf(stdout, "ok   %s matches %s (%d subjects)\n", lockPath, c.URL(), len(l.Subjects))
	return 0
}

func packageOf(full string) string {
	if i := strings.LastIndex(full, "."); i >= 0 {
		return full[:i]
	}
	return ""
}
