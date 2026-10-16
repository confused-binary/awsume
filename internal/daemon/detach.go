package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Detach starts exe with args as a background process that outlives the
// caller, appending its output to logPath, and returns its process id.
func Detach(exe string, args []string, logPath string) (int, error) {
	cmd := exec.Command(exe, args...)
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			return 0, err
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		cmd.Stdout = f
		cmd.Stderr = f
	}
	detachAttrs(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// WaitRunning polls socket until a daemon answers on it or ctx ends.
func WaitRunning(ctx context.Context, socket string) error {
	for {
		if Running(socket) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the daemon did not start listening on %s", socket)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
