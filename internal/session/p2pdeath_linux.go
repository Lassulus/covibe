//go:build linux

package session

import (
	"os/exec"
	"syscall"
)

// dieWithParent ties the sidecar's life to this wrapper's, however the wrapper
// ends — including a SIGKILL, which runs no deferred cleanup at all.
//
// The whole ticket model rests on this. A ticket is revoked by the session
// ending, so an orphaned sidecar would keep answering for an endpoint whose
// session is gone: the tickets would still resolve, and a dialer would reach a
// bridge with nothing behind it instead of a clean refusal.
func dieWithParent(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
