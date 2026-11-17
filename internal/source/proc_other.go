//go:build !unix

package source

import "os/exec"

// setProcessGroup is a no-op where process groups are not available: only
// the direct child is killed on cancellation.
func setProcessGroup(cmd *exec.Cmd) {}
