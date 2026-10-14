// Package browser opens a URL in the user's default web browser using the
// platform's native launcher.
package browser

import (
	"errors"
	"os/exec"
	"strings"
)

// ErrUnavailable is returned when no launcher can be found.
var ErrUnavailable = errors.New("no browser launcher found (set the BROWSER environment variable)")

type opener struct {
	lookPath func(string) (string, error)
	start    func(path string, args []string) error
	env      func(string) string
}

var defaultOpener = opener{
	lookPath: exec.LookPath,
	start: func(path string, args []string) error {
		cmd := exec.Command(path, args...)
		if err := cmd.Start(); err != nil {
			return err
		}
		// Do not wait: launchers such as xdg-open return immediately, but a
		// browser named through BROWSER may stay in the foreground.
		return cmd.Process.Release()
	},
}

// Open launches url in the default browser.
func Open(url string) error {
	return defaultOpener.open(candidates(), url)
}

func (o opener) open(cands [][]string, url string) error {
	if o.env != nil {
		if b := strings.TrimSpace(o.env("BROWSER")); b != "" {
			fields := strings.Fields(b)
			cands = append([][]string{fields}, cands...)
		}
	}
	var lastErr error
	found := false
	for _, cand := range cands {
		path, err := o.lookPath(cand[0])
		if err != nil {
			continue
		}
		found = true
		args := append(append([]string(nil), cand[1:]...), url)
		if err := o.start(path, args); err != nil {
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
