//go:build e2e && linux

package harness

import "syscall"

// sysProcAttr makes the kernel SIGKILL a child when the test binary
// dies, so a crashed or interrupted go test leaves no orphans.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
