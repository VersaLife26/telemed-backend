//go:build !linux

package scheduling_test

import "os/exec"

// dieWithParent is a no-op outside Linux, which has no equivalent primitive.
// The deferred shutdown in TestMain is the only cleanup path there.
func dieWithParent(cmd *exec.Cmd) {}
