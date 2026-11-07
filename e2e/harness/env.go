//go:build e2e

package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// Env is one story's world: its processes, ports and temp directory.
// Everything it starts is SIGKILLed when the test ends; when the test
// failed, the tail of every process's log is written to the test log.
type Env struct {
	t      testing.TB
	dir    string
	logDir string

	mu    sync.Mutex
	procs []*Proc
}

// LogTailLines is how many lines of each process log a failed test
// prints. Override with E2E_LOG_LINES.
var LogTailLines = 60

// New returns an Env for t.
func New(t testing.TB) *Env {
	t.Helper()
	// TempDir first: cleanups run last-in first-out, so the processes
	// are killed before their directory is removed.
	dir := t.TempDir()
	e := &Env{t: t, dir: dir, logDir: filepath.Join(dir, "logs")}
	if err := os.MkdirAll(e.logDir, 0o755); err != nil {
		t.Fatalf("harness: %v", err)
	}
	t.Cleanup(e.cleanup)
	return e
}

// T returns the test the Env belongs to.
func (e *Env) T() testing.TB { return e.t }

// Dir returns the Env's temp directory (process working directory).
func (e *Env) Dir() string { return e.dir }

// Procs returns every process started in this Env, in start order.
func (e *Env) Procs() []*Proc {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*Proc(nil), e.procs...)
}

func (e *Env) cleanup() {
	procs := e.Procs()
	states := make([]string, len(procs))
	for i, p := range procs {
		states[i] = "running at test end"
		if code, ok := p.ExitCode(); ok {
			states[i] = fmt.Sprintf("exited, code %d", code)
		}
	}
	for i := len(procs) - 1; i >= 0; i-- {
		procs[i].kill()
	}
	if !e.t.Failed() {
		return
	}
	n := LogTailLines
	if v, err := strconv.Atoi(os.Getenv("E2E_LOG_LINES")); err == nil && v > 0 {
		n = v
	}
	for i, p := range procs {
		e.t.Logf("---- %s (%s; started %d times) last %d lines ----\n%s", p.Name, states[i], p.starts, n, p.tail(n))
	}
}
