package browser

import (
	"errors"
	"testing"
)

func TestOpenPrefersBrowserEnv(t *testing.T) {
	var started [][]string
	o := opener{
		lookPath: func(name string) (string, error) {
			if name == "firefox" || name == "xdg-open" {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("missing")
		},
		start: func(path string, args []string) error {
			started = append(started, append([]string{path}, args...))
			if path == "/usr/bin/firefox" {
				return errors.New("crashed")
			}
			return nil
		},
		env: func(k string) string {
			if k == "BROWSER" {
				return "firefox --new-tab"
			}
			return ""
		},
	}
	if err := o.open([][]string{{"xdg-open"}}, "https://example.com/?a=1&b=2"); err != nil {
		t.Fatal(err)
	}
	if len(started) != 2 || started[0][0] != "/usr/bin/firefox" || started[0][1] != "--new-tab" || started[1][1] != "https://example.com/?a=1&b=2" {
		t.Errorf("started = %v", started)
	}
}

func TestOpenErrors(t *testing.T) {
	o := opener{lookPath: func(string) (string, error) { return "", errors.New("missing") }}
	if err := o.open([][]string{{"x"}}, "u"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
	o = opener{
		lookPath: func(n string) (string, error) { return n, nil },
		start:    func(string, []string) error { return errors.New("boom") },
	}
	if err := o.open([][]string{{"x"}}, "u"); err == nil || err.Error() != "x: boom" {
		t.Errorf("err = %v", err)
	}
	if len(candidates()) == 0 {
		t.Error("platform must define a launcher")
	}
}
