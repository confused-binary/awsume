// Package clipboard copies text to the system clipboard using the native
// tool of each platform (clip on Windows, pbcopy on macOS, wl-copy / xclip /
// xsel on Linux and BSDs) so that no third party library is required.
package clipboard

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// runTimeout bounds how long a clipboard tool may take. Tools such as xclip
// and wl-copy fork a child that keeps serving the clipboard; the parent exits
// immediately, so a healthy tool returns well within this limit.
const runTimeout = 10 * time.Second

// ErrUnavailable is returned when no clipboard tool can be found.
var ErrUnavailable = errors.New("no clipboard tool found (install wl-clipboard, xclip or xsel, or use pbcopy/clip on macOS/Windows)")

// copier abstracts process execution for tests.
type copier struct {
	lookPath func(string) (string, error)
	run      func(path string, args []string, stdin string) error
}

var defaultCopier = copier{
	lookPath: exec.LookPath,
	run: func(path string, args []string, stdin string) error {
		ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Stdin = strings.NewReader(stdin)
		// Do not attach pipes for stdout/stderr: xclip and wl-copy fork a
		// background child that inherits them and would keep them open
		// forever, which makes any read-until-EOF wait hang.
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return errors.New("timed out after " + runTimeout.String())
			}
			return err
		}
		return nil
	},
}

// Copy writes text to the clipboard.
func Copy(text string) error {
	return defaultCopier.copy(candidates(), text)
}

func (c copier) copy(cands [][]string, text string) error {
	var lastErr error
	found := false
	for _, cand := range cands {
		path, err := c.lookPath(cand[0])
		if err != nil {
			continue
		}
		found = true
		if err := c.run(path, cand[1:], text); err != nil {
			lastErr = errors.New(cand[0] + ": " + err.Error())
			continue
		}
		return nil
	}
	if !found {
		return ErrUnavailable
	}
	return lastErr
}
