//go:build linux

package scheduling_test

import (
	"os/exec"
	"syscall"
)

// dieWithParent asks the kernel to signal the child when this process exits.
//
// It is the difference between "the test binary was killed and left a postgres
// running" and "it did not". Without it, a `go test` interrupted with ^C or an
// OOM kill leaks a postmaster holding a temp directory open, which on a shared
// build machine is somebody else's problem an hour later.
func dieWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: false}
}
