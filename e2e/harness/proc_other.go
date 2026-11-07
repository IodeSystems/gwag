//go:build e2e && !linux

package harness

import "syscall"

// sysProcAttr: no parent-death signal off Linux; t.Cleanup still kills
// every process when a test ends normally.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }
