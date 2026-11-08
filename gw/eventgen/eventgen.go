// Package eventgen is the code generator behind `gwag gen`.
//
// One run: compile the .proto files (protocompile, gwag's own options
// embedded), build a protoc plugin request, run the language's pinned
// base generators (protoc-gen-go, ...) as subprocesses over the
// plugin protocol, then run the language's event Emitter in process.
// Both outputs land under the same out directory.
//
// A language is a Language value passed to Register. It names how to
// resolve its base generators (Plugins) and the Emitter that writes
// the typed event code. See golang.go for the Go language.
//
// Stability: experimental
package eventgen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"
)

// Language is one target language.
//
// Stability: experimental
type Language struct {
	// Name is the --lang value: "go", "typescript", "python".
	Name string
	// Plugins resolves the base generators to run, in order. Each must
	// be pinned by the consumer project (go.mod tool directive,
	// package.json devDependency, pyproject dependency), never taken
	// from PATH. Use ResolvePlugin.
	Plugins func(ctx context.Context, env Env) ([]Plugin, error)
	// Emitter writes the typed event code.
	Emitter Emitter
}

// Env is what a Language sees of the command line.
//
// Stability: experimental
type Env struct {
	// ModuleDir is the consumer project's root: where go.mod,
	// package.json or pyproject.toml pins the base generators.
	ModuleDir string
	// Parameter is the plugin parameter (--opt values joined by ","),
	// passed to every base generator and to the Emitter.
	Parameter string
	// Grpc asks for service stubs too, where the language has a
	// pinned generator for them.
	Grpc bool
}

// Plugin is one resolved base generator: a command speaking the protoc
// plugin protocol on stdin / stdout.
//
// Stability: experimental
type Plugin struct {
	// Name is the generator's name, e.g. "protoc-gen-go".
	Name string
	// Command is the argv, e.g. ["go", "tool", "protoc-gen-go"].
	Command []string
	// Dir is the working directory the command runs in.
	Dir string
	// Version is the generator's own version line, e.g.
	// "protoc-gen-go v1.36.12". Written into generated headers.
	Version string
	// Parameter overrides Env.Parameter for this generator when set.
	Parameter string
}

// Emitter writes one language's event code.
//
// Stability: experimental
type Emitter interface {
	// Emit returns the files to write, paths relative to the out
	// directory. Return nil when no file declares a named channel.
	Emit(in *Input) ([]*pluginpb.CodeGeneratorResponse_File, error)
}

// EmitterFunc adapts a function to Emitter.
//
// Stability: experimental
type EmitterFunc func(in *Input) ([]*pluginpb.CodeGeneratorResponse_File, error)

// Emit implements Emitter.
func (f EmitterFunc) Emit(in *Input) ([]*pluginpb.CodeGeneratorResponse_File, error) { return f(in) }

// Input is what an Emitter gets.
//
// Stability: experimental
type Input struct {
	// Request is the plugin request the base generators got, with
	// Parameter = Env.Parameter.
	Request *pluginpb.CodeGeneratorRequest
	// Files are the files to generate, in command-line order.
	Files []protoreflect.FileDescriptor
	// Channels are the named channels those files declare.
	Channels []*Channel
	// Header is the version stamp for generated file headers.
	Header Header
}

// Header is the version stamp generated event files carry.
//
// Stability: experimental
type Header struct {
	// GwagVersion is gwag's version, "(devel)" for an untagged build.
	GwagVersion string
	// Plugins are the base generators that ran.
	Plugins []Plugin
}

// Lines returns "gwag <version>" then each plugin's version line, for
// a "versions:" comment block.
func (h Header) Lines() []string {
	out := []string{"gwag " + h.GwagVersion}
	for _, p := range h.Plugins {
		v := p.Version
		if v == "" {
			v = p.Name + " (unknown)"
		}
		out = append(out, v)
	}
	return out
}

var languages = map[string]*Language{}

// Register adds a language. Call from init.
//
// Stability: experimental
func Register(l *Language) {
	if _, dup := languages[l.Name]; dup {
		panic("eventgen: language registered twice: " + l.Name)
	}
	languages[l.Name] = l
}

// Languages returns the registered language names, sorted.
//
// Stability: experimental
func Languages() []string {
	out := make([]string, 0, len(languages))
	for n := range languages {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Config is one `gwag gen` run.
//
// Stability: experimental
type Config struct {
	Lang        string
	Out         string   // output directory; default "."
	ImportPaths []string // -I; default ["."]
	Files       []string // .proto files, on disk or relative to an import path
	Opts        []string // plugin parameters (--opt), joined with ","
	ModuleDir   string   // default "."
	Grpc        bool
	GwagVersion string // default GwagVersion()
}

// Generate runs cfg and returns the written paths (relative to Out),
// sorted.
//
// Stability: experimental
func Generate(ctx context.Context, cfg Config) ([]string, error) {
	lang := languages[cfg.Lang]
	if lang == nil {
		return nil, fmt.Errorf("unknown language %q (have %s)", cfg.Lang, strings.Join(Languages(), ", "))
	}
	if len(cfg.Files) == 0 {
		return nil, errors.New("no .proto files given")
	}
	if cfg.Out == "" {
		cfg.Out = "."
	}
	if len(cfg.ImportPaths) == 0 {
		cfg.ImportPaths = []string{"."}
	}
	if cfg.ModuleDir == "" {
		cfg.ModuleDir = "."
	}
	if cfg.GwagVersion == "" {
		cfg.GwagVersion = GwagVersion()
	}
	moduleDir, err := filepath.Abs(cfg.ModuleDir)
	if err != nil {
		return nil, err
	}

	names, err := protoNames(cfg.ImportPaths, cfg.Files)
	if err != nil {
		return nil, err
	}
	files, all, err := compile(ctx, cfg.ImportPaths, names)
	if err != nil {
		return nil, err
	}
	chans, err := Channels(files)
	if err != nil {
		return nil, err
	}

	env := Env{ModuleDir: moduleDir, Parameter: strings.Join(cfg.Opts, ","), Grpc: cfg.Grpc}
	plugins, err := lang.Plugins(ctx, env)
	if err != nil {
		return nil, err
	}
	req := buildRequest(names, all, env.Parameter)

	out := newWriter(cfg.Out)
	for _, p := range plugins {
		r := proto.Clone(req).(*pluginpb.CodeGeneratorRequest)
		if p.Parameter != "" {
			r.Parameter = proto.String(p.Parameter)
		}
		resp, err := RunPlugin(ctx, p, r)
		if err != nil {
			return nil, err
		}
		if err := out.add(p.Name, resp.GetFile()); err != nil {
			return nil, err
		}
	}
	evFiles, err := lang.Emitter.Emit(&Input{
		Request:  req,
		Files:    files,
		Channels: chans,
		Header:   Header{GwagVersion: cfg.GwagVersion, Plugins: plugins},
	})
	if err != nil {
		return nil, fmt.Errorf("%s events: %w", cfg.Lang, err)
	}
	if err := out.add("gwag events", evFiles); err != nil {
		return nil, err
	}
	return out.flush()
}

// RunPlugin runs one base generator over req.
//
// Stability: experimental
func RunPlugin(ctx context.Context, p Plugin, req *pluginpb.CodeGeneratorRequest) (*pluginpb.CodeGeneratorResponse, error) {
	in, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, p.Command[0], p.Command[1:]...)
	cmd.Dir = p.Dir
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s (%s): %v\n%s", p.Name, strings.Join(p.Command, " "), err, strings.TrimSpace(stderr.String()))
	}
	resp := &pluginpb.CodeGeneratorResponse{}
	if err := proto.Unmarshal(stdout.Bytes(), resp); err != nil {
		return nil, fmt.Errorf("%s: bad response: %w", p.Name, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s: %s", p.Name, resp.GetError())
	}
	return resp, nil
}

// ResolvePlugin checks that command runs (with --version, in dir) and
// returns the Plugin with its version line. On failure the error
// carries install, the command that pins the generator in the
// consumer project.
//
// Stability: experimental
func ResolvePlugin(ctx context.Context, name string, command []string, dir, install string) (Plugin, error) {
	args := append(append([]string(nil), command[1:]...), "--version")
	cmd := exec.CommandContext(ctx, command[0], args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return Plugin{}, fmt.Errorf("%s is not available in %s (%s): %s\npin it in the project with:\n\t%s", name, dir, strings.Join(command, " "), msg, install)
	}
	v := strings.TrimSpace(stdout.String())
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		v = v[:i]
	}
	return Plugin{Name: name, Command: command, Dir: dir, Version: v}, nil
}

// writer collects generated files, rejects collisions and paths that
// leave the out directory, and writes them.
type writer struct {
	dir   string
	files map[string][]byte
	from  map[string]string
}

func newWriter(dir string) *writer {
	return &writer{dir: dir, files: map[string][]byte{}, from: map[string]string{}}
}

func (w *writer) add(source string, files []*pluginpb.CodeGeneratorResponse_File) error {
	for _, f := range files {
		if f.GetInsertionPoint() != "" {
			return fmt.Errorf("%s: insertion points are not supported (%s)", source, f.GetName())
		}
		name := filepath.ToSlash(filepath.Clean(f.GetName()))
		if name == "" || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("%s: output path %q leaves the out directory", source, f.GetName())
		}
		if prev, dup := w.from[name]; dup {
			return fmt.Errorf("%s and %s both write %s", prev, source, name)
		}
		w.from[name] = source
		w.files[name] = []byte(f.GetContent())
	}
	return nil
}

func (w *writer) flush() ([]string, error) {
	names := make([]string, 0, len(w.files))
	for n := range w.files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(w.dir, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, w.files[n], 0o644); err != nil {
			return nil, err
		}
	}
	return names, nil
}
