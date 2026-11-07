//go:build e2e

package harness

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Proc is one OS process the harness started. Its stdout and stderr go
// to <env dir>/logs/<name>.log and to memory. Restart starts it again
// with the same command line, appending to the same log.
type Proc struct {
	Name string
	Path string
	Args []string

	t   testing.TB
	env *Env

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{} // closed when the current incarnation exits
	state   *os.ProcessState
	logBuf  bytes.Buffer
	logFile *os.File
	since   int // logBuf offset where the current incarnation starts
	starts  int
}

// logWriter tees process output into the Proc's buffer and log file.
type logWriter struct{ p *Proc }

func (w logWriter) Write(b []byte) (int, error) {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	w.p.logBuf.Write(b)
	if w.p.logFile != nil {
		_, _ = w.p.logFile.Write(b)
	}
	return len(b), nil
}

// Start runs bin with args as a managed process named name. The test
// fails if it cannot start. Every process is SIGKILLed when the test
// ends; a failed test logs the tail of every process's output.
func (e *Env) Start(name, bin string, args ...string) *Proc {
	e.t.Helper()
	e.mu.Lock()
	for _, p := range e.procs {
		if p.Name == name {
			e.mu.Unlock()
			e.t.Fatalf("harness: process name %q already used", name)
		}
	}
	p := &Proc{Name: name, Path: bin, Args: args, t: e.t, env: e}
	e.procs = append(e.procs, p)
	e.mu.Unlock()
	f, err := os.Create(filepath.Join(e.logDir, name+".log"))
	if err != nil {
		e.t.Fatalf("harness: log file for %s: %v", name, err)
	}
	p.logFile = f
	if err := p.start(); err != nil {
		e.t.Fatalf("harness: start %s: %v", name, err)
	}
	return p
}

func (p *Proc) start() error {
	cmd := exec.Command(p.Path, p.Args...)
	cmd.Dir = p.env.dir
	cmd.Stdout = logWriter{p}
	cmd.Stderr = logWriter{p}
	cmd.SysProcAttr = sysProcAttr()
	p.mu.Lock()
	if p.starts > 0 {
		fmt.Fprintf(&p.logBuf, "=== harness: restart %d ===\n", p.starts)
		if p.logFile != nil {
			fmt.Fprintf(p.logFile, "=== harness: restart %d ===\n", p.starts)
		}
	}
	p.since = p.logBuf.Len()
	p.starts++
	p.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	p.mu.Lock()
	p.cmd, p.done, p.state = cmd, done, nil
	p.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		p.mu.Lock()
		p.state = cmd.ProcessState
		p.mu.Unlock()
		close(done)
	}()
	return nil
}

// PID returns the current incarnation's process id.
func (p *Proc) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd.Process.Pid
}

// Running reports whether the current incarnation has not exited.
func (p *Proc) Running() bool {
	select {
	case <-p.doneCh():
		return false
	default:
		return true
	}
}

func (p *Proc) doneCh() chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// ExitCode returns the exit status of the current incarnation, -1 if
// it was killed by a signal, and ok=false while it still runs.
func (p *Proc) ExitCode() (code int, ok bool) {
	if p.Running() {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.ExitCode(), true
}

// Signal sends sig to the process. A process that already exited is
// not an error.
func (p *Proc) Signal(sig syscall.Signal) {
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	_ = cmd.Process.Signal(sig)
}

// WaitExit waits up to timeout for the process to exit and returns
// its exit code (-1 = killed by a signal). The test fails on timeout.
func (p *Proc) WaitExit(timeout time.Duration) int {
	p.t.Helper()
	select {
	case <-p.doneCh():
	case <-time.After(timeout):
		p.t.Fatalf("harness: %s still running %s after it was asked to exit", p.Name, timeout)
	}
	code, _ := p.ExitCode()
	return code
}

// Stop sends SIGTERM and waits up to timeout for exit; the test fails
// if the process does not exit. Returns the exit code.
func (p *Proc) Stop(timeout time.Duration) int {
	p.t.Helper()
	p.Signal(syscall.SIGTERM)
	return p.WaitExit(timeout)
}

// Kill sends SIGKILL and waits for the process to be reaped.
func (p *Proc) Kill() {
	p.t.Helper()
	p.Signal(syscall.SIGKILL)
	p.WaitExit(10 * time.Second)
}

// Restart starts the process again with the same command line. A
// process still running is stopped first with SIGTERM (10s grace).
func (p *Proc) Restart() {
	p.t.Helper()
	if p.Running() {
		p.Stop(10 * time.Second)
	}
	if err := p.start(); err != nil {
		p.t.Fatalf("harness: restart %s: %v", p.Name, err)
	}
}

// Logs returns everything the process wrote, all incarnations.
func (p *Proc) Logs() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logBuf.String()
}

// CurrentLogs returns the output of the current incarnation only.
func (p *Proc) CurrentLogs() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logBuf.String()[p.since:]
}

// WaitLog waits until the current incarnation's output contains
// substr. The test fails on timeout or if the process exits first.
func (p *Proc) WaitLog(substr string, timeout time.Duration) {
	p.t.Helper()
	Eventually(p.t, timeout, fmt.Sprintf("%s logs %q", p.Name, substr), func() error {
		if strings.Contains(p.CurrentLogs(), substr) {
			return nil
		}
		if !p.Running() {
			code, _ := p.ExitCode()
			return Permanent(fmt.Errorf("%s exited (code %d) before logging %q", p.Name, code, substr))
		}
		return fmt.Errorf("not logged yet")
	})
}

// tail returns the last n lines of the log.
func (p *Proc) tail(n int) string {
	lines := strings.Split(strings.TrimRight(p.Logs(), "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{fmt.Sprintf("... (%d earlier lines)", len(lines)-n)}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}

// kill is the cleanup path: SIGKILL, reap, close the log.
func (p *Proc) kill() {
	if p.Running() {
		p.Signal(syscall.SIGKILL)
		select {
		case <-p.doneCh():
		case <-time.After(10 * time.Second):
		}
	}
	p.mu.Lock()
	if p.logFile != nil {
		_ = p.logFile.Close()
		p.logFile = nil
	}
	p.mu.Unlock()
}
