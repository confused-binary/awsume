package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestPaths(t *testing.T) {
	if got := SocketPath(env(map[string]string{SocketEnv: "/s.sock", "XDG_RUNTIME_DIR": "/run/user/1"})); got != "/s.sock" {
		t.Errorf("override: %s", got)
	}
	if got := SocketPath(env(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1", "HOME": "/h"})); got != filepath.Join("/run/user/1", "awsume", "daemon.sock") {
		t.Errorf("runtime dir: %s", got)
	}
	if got := SocketPath(env(map[string]string{"HOME": "/h"})); got != filepath.Join("/h", ".cache", "awsume", "daemon.sock") {
		t.Errorf("cache dir: %s", got)
	}
	if got := LogPath(env(map[string]string{"XDG_CACHE_HOME": "/c"})); got != filepath.Join("/c", "awsume", "daemon.log") {
		t.Errorf("log: %s", got)
	}
	if got := CacheDir(env(map[string]string{"LOCALAPPDATA": `C:\Users\me\AppData\Local`})); got != filepath.Join(`C:\Users\me\AppData\Local`, "awsume") {
		t.Errorf("localappdata: %s", got)
	}
}

func TestServeAndCall(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	if Running(sock) {
		t.Fatal("running before listen")
	}
	if _, err := Call(context.Background(), sock, Request{Op: OpStatus}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("call without daemon: %v", err)
	}
	// A stale socket file is replaced.
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(sock); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("socket mode %v, %v", info.Mode(), err)
		}
	}
	if _, err := Listen(sock); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var got Request
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, func(_ context.Context, req Request) Response {
			got = req
			switch req.Op {
			case OpSession:
				return Response{OK: true, Session: &SessionInfo{ID: 7, Profile: req.Profile}}
			case OpStop:
				cancel()
				return Response{OK: true, Message: "bye"}
			}
			return Response{Error: "nope"}
		})
	}()
	if !Running(sock) {
		t.Fatal("not running after listen")
	}
	resp, err := Call(context.Background(), sock, Request{Op: OpSession, Args: []string{"-ab", "chrome"}, Env: map[string]string{"A": "1"}, Profile: "prod", Login: &Login{URL: "https://x", Hosts: []string{"x"}}})
	if err != nil || resp.Session == nil || resp.Session.ID != 7 || resp.Session.Profile != "prod" {
		t.Fatalf("session call: %+v, %v", resp, err)
	}
	if got.Args[1] != "chrome" || got.Env["A"] != "1" || got.Login.URL != "https://x" || got.Login.Hosts[0] != "x" {
		t.Errorf("daemon saw %+v", got)
	}
	if _, err := Call(context.Background(), sock, Request{Op: "other"}); err == nil || err.Error() != "nope" {
		t.Errorf("refused call: %v", err)
	}
	if resp, err := Call(context.Background(), sock, Request{Op: OpStop}); err != nil || resp.Message != "bye" {
		t.Errorf("stop: %+v, %v", resp, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
}

func TestEnvironAndLookPath(t *testing.T) {
	m := Environ([]string{"A=1", "B=x=y", "bad", "A=2"})
	if m["A"] != "2" || m["B"] != "x=y" || len(m) != 2 {
		t.Errorf("environ = %v", m)
	}
	if got := EnvList(m); strings.Join(got, ",") != "A=2,B=x=y" {
		t.Errorf("list = %v", got)
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "tool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	look := LookPath(strings.Join([]string{t.TempDir(), dir}, string(os.PathListSeparator)), "")
	if got, err := look("tool"); err != nil || got != exe {
		t.Errorf("tool: %s, %v", got, err)
	}
	if _, err := look("missing"); err == nil {
		t.Error("missing found")
	}
	if runtime.GOOS != "windows" {
		if _, err := look("plain"); err == nil {
			t.Error("non-executable found")
		}
	}
	if got, err := look(exe); err != nil || got != exe {
		t.Errorf("absolute: %s, %v", got, err)
	}
}

func TestDetach(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sub", "daemon.log")
	pid, err := Detach("/bin/sh", []string{"-c", "echo detached $$"}, logPath)
	if err != nil {
		t.Fatal(err)
	}
	if pid <= 0 {
		t.Errorf("pid %d", pid)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(logPath)
		if strings.HasPrefix(string(data), "detached ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("log = %q", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := WaitRunning(ctx, filepath.Join(dir, "none.sock")); err == nil {
		t.Error("WaitRunning succeeded without a daemon")
	}
}
