package eventgen

import (
	"context"

	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

func init() {
	for _, name := range []string{"python", "py"} {
		Register(&Language{Name: name, Plugins: pythonPlugins, Emitter: EmitterFunc(emitPython)})
	}
}

const (
	betterprotoCompiler = "betterproto2_compiler"
	betterprotoPlugin   = "protoc-gen-python_betterproto2"
	betterprotoPin      = "0.10.1"
)

// pythonPlugins resolves betterproto2's protoc plugin from the uv
// project in env.ModuleDir. uv runs it from that project's locked
// environment; nothing comes from PATH but uv itself.
func pythonPlugins(ctx context.Context, env Env) ([]Plugin, error) {
	base := []string{"uv", "run", "--quiet", "--locked", "--project", env.ModuleDir}
	// The plugin reads a request on stdin and has no --version, so
	// probe the installed distribution instead.
	probe := append(append([]string(nil), base...), "python", "-c",
		"import importlib.metadata as m;print('"+betterprotoPlugin+" '+m.version('"+betterprotoCompiler+"'))")
	p, err := ResolvePlugin(ctx, betterprotoPlugin, probe, env.ModuleDir,
		"uv add --dev "+betterprotoCompiler+"=="+betterprotoPin+"   (and: uv add betterproto2)")
	if err != nil {
		return nil, err
	}
	p.Command = append(base, betterprotoPlugin)
	p.Request = betterprotoRequest
	return []Plugin{p}, nil
}

// betterprotoRequest edits the request for betterproto2, which emits
// a Python module for every file in it (not only FileToGenerate) and
// needs source info on each. gwag's own option files, and the
// descriptor.proto only they pull in, would become modules nothing
// imports, so they are dropped. Any other dependency gets an empty
// source info so the plugin accepts it.
func betterprotoRequest(req *pluginpb.CodeGeneratorRequest) {
	gen := map[string]bool{}
	for _, n := range req.FileToGenerate {
		gen[n] = true
	}
	drop := map[string]bool{}
	for n := range embedded {
		if !gen[n] {
			drop[n] = true
		}
	}
	const descriptor = "google/protobuf/descriptor.proto"
	if !gen[descriptor] {
		needed := false
		for _, f := range req.ProtoFile {
			if drop[f.GetName()] {
				continue
			}
			for _, d := range f.Dependency {
				if d == descriptor {
					needed = true
				}
			}
		}
		if !needed {
			drop[descriptor] = true
		}
	}
	kept := req.ProtoFile[:0]
	for _, f := range req.ProtoFile {
		if drop[f.GetName()] {
			continue
		}
		var deps []string
		for _, d := range f.Dependency {
			if !drop[d] {
				deps = append(deps, d)
			}
		}
		if len(deps) != len(f.Dependency) {
			f.Dependency, f.PublicDependency, f.WeakDependency = deps, nil, nil
		}
		if f.SourceCodeInfo == nil {
			f.SourceCodeInfo = &descriptorpb.SourceCodeInfo{}
		}
		kept = append(kept, f)
	}
	req.ProtoFile = kept
}
