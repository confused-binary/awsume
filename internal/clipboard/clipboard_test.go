package clipboard

import (
	"errors"
	"testing"
)

func TestCopyPicksFirstAvailable(t *testing.T) {
	var ran []string
	c := copier{
		lookPath: func(name string) (string, error) {
			if name == "second" || name == "third" {
				return "/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		run: func(path string, args []string, stdin string) error {
			ran = append(ran, path)
			if path == "/bin/second" {
				return errors.New("display not available")
			}
			if stdin != "hello" || len(args) != 1 || args[0] != "--arg" {
				t.Errorf("unexpected invocation %s %v %q", path, args, stdin)
			}
			return nil
		},
	}
	cands := [][]string{{"first"}, {"second", "--arg"}, {"third", "--arg"}}
	if err := c.copy(cands, "hello"); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 2 || ran[1] != "/bin/third" {
		t.Errorf("ran = %v", ran)
	}
}

func TestCopyErrors(t *testing.T) {
	c := copier{lookPath: func(string) (string, error) { return "", errors.New("nope") }}
	if err := c.copy([][]string{{"x"}}, "t"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
	c = copier{
		lookPath: func(n string) (string, error) { return "/bin/" + n, nil },
		run:      func(string, []string, string) error { return errors.New("failed") },
	}
	if err := c.copy([][]string{{"x"}}, "t"); err == nil || err.Error() != "x: failed" {
		t.Errorf("err = %v", err)
	}
	if len(candidates()) == 0 {
		t.Error("platform must define at least one candidate")
	}
}
