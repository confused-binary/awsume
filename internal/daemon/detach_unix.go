//go:build !windows

package daemon

import (
	"os/exec"
	"syscall"
)

// detachAttrs puts the child in its own session so it survives the
// terminal that started it and never receives its signals.
func detachAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
