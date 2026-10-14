package clipboard

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestForkingToolDoesNotHang mimics xclip/wl-copy, which fork a child that
// keeps running after the parent exits.
func TestForkingToolDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script tool")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "fakeclip")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\ncat >/dev/null\nsleep 30 &\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- defaultCopier.copy([][]string{{tool}}, "url") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("copy hung while the forked child held our pipes")
	}

	failing := filepath.Join(dir, "failclip")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := defaultCopier.copy([][]string{{failing}}, "url"); err == nil {
		t.Fatal("expected failure exit status to be reported")
	}
}
