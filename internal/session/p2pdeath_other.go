//go:build !linux

package session

import "os/exec"

// dieWithParent has no portable equivalent outside Linux. Elsewhere the sidecar
// is reaped by stop() when the wrapper exits normally, and by its process group
// when the terminal that owns it goes away.
func dieWithParent(*exec.Cmd) {}
