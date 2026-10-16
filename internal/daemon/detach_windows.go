//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
)

const detachedProcess = 0x00000008

// detachAttrs starts the child without a console window and outside the
// caller's process group so Ctrl-C in the terminal does not reach it.
func detachAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
		HideWindow:    true,
	}
}
