//go:build !windows && !darwin

package browser

import "os"

func init() {
	defaultOpener.env = os.Getenv
}

func candidates() [][]string {
	return [][]string{
		{"xdg-open"},
		{"wslview"},
		{"sensible-browser"},
		{"x-www-browser"},
		{"gio", "open"},
	}
}
