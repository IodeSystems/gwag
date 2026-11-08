//go:build e2e

// Package harness runs gwag gateways and fixture services as real OS
// processes for the e2e stories. See e2e/README.md.
package harness

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// target is one binary the harness can build.
type target struct {
	dir  string   // module directory relative to the repo root; "" = the root module
	pkg  string   // package path relative to dir
	tags []string // build tags
}

var targets = map[string]target{
	// The example gateway, not gw/cmd/gwag: it exposes every flag the
	// stories need (backpressure caps, --caller-id-public, cluster,
	// --admin-data-dir) and is the wiring demo.sh exercises.
	"gateway": {pkg: "./examples/multi/cmd/gateway"},
	"fixture": {pkg: "./e2e/fixture", tags: []string{"e2e"}},
	"gwag":    {pkg: "./gw/cmd/gwag"},
	// Its own module: franz-go stays out of the root go.mod.
	"kafka-fixture": {dir: "e2e/kafka-fixture", pkg: "."},
}

var (
	binDir  string
	builds  sync.Map // name -> *buildResult
	rootDir string
	rootErr error
	rootOne sync.Once
)

type buildResult struct {
	once sync.Once
	path string
	err  error
}

// Main wraps testing.M for a story package. It owns the directory the
// binaries are built into (once per go test run) and removes it after
// the tests. Call it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
func Main(m *testing.M) int {
	dir, err := os.MkdirTemp("", "gwag-e2e-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		return 1
	}
	binDir = dir
	defer os.RemoveAll(dir)
	return m.Run()
}

// RepoRoot returns the gwag repository root (the directory holding
// go.mod), found by walking up from the working directory.
func RepoRoot() (string, error) {
	rootOne.Do(func() {
		dir, err := os.Getwd()
		if err != nil {
			rootErr = err
			return
		}
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				if _, err := os.Stat(filepath.Join(dir, "e2e", "fixture")); err == nil {
					rootDir = dir
					return
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				rootErr = errors.New("harness: repo root not found above the working directory")
				return
			}
			dir = parent
		}
	})
	return rootDir, rootErr
}

// Binary returns the path of a built binary: "gateway", "fixture",
// "gwag" or "kafka-fixture". The first call per name builds it; later calls reuse it.
func Binary(t testing.TB, name string) string {
	t.Helper()
	if binDir == "" {
		t.Fatalf("harness: binaries need harness.Main; add\n\tfunc TestMain(m *testing.M) { os.Exit(harness.Main(m)) }")
	}
	tg, ok := targets[name]
	if !ok {
		t.Fatalf("harness: unknown binary %q", name)
	}
	v, _ := builds.LoadOrStore(name, &buildResult{})
	br := v.(*buildResult)
	br.once.Do(func() { br.path, br.err = build(name, tg) })
	if br.err != nil {
		t.Fatalf("harness: build %s: %v", name, br.err)
	}
	return br.path
}

func build(name string, tg target) (string, error) {
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	if err := seedUIDist(root); err != nil {
		return "", err
	}
	out := filepath.Join(binDir, name)
	args := []string{"build", "-o", out}
	if len(tg.tags) > 0 {
		args = append(args, "-tags", strings.Join(tg.tags, ","))
	}
	args = append(args, tg.pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Join(root, tg.dir)
	start := time.Now()
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go %v: %v\n%s", args, err, b)
	}
	fmt.Fprintf(os.Stderr, "harness: built %s in %s\n", name, time.Since(start).Round(time.Millisecond))
	return out, nil
}

// seedUIDist gives ui/embed.go's //go:embed all:dist something to embed
// when the Vite build has not run, the way bin/build does: copy
// ui/fallback/index.html to ui/dist/index.html. ui/dist is gitignored.
func seedUIDist(root string) error {
	dist := filepath.Join(root, "ui", "dist")
	if _, err := os.Stat(filepath.Join(dist, "index.html")); err == nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(root, "ui", "fallback", "index.html"))
	if err != nil {
		return fmt.Errorf("seed ui/dist: %w", err)
	}
	if err := os.MkdirAll(dist, 0o755); err != nil {
		return err
	}
	// Write then rename: two test binaries may seed at once.
	tmp, err := os.CreateTemp(dist, ".index-*.html")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dist, "index.html"))
}
