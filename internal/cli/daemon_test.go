package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"awsume/internal/awsconfig"
	"awsume/internal/daemon"
	"awsume/internal/webdriver"
)

// syncBuffer is a bytes.Buffer safe for the daemon's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startDaemon runs "awsume -daemon run" on the harness's fakes in a
// goroutine, with its own output buffers. advance makes the session loop
// advance the fake clock instead of sleeping; otherwise it blocks until
// the daemon stops, so sessions stay listed.
func (h *harness) startDaemon(t *testing.T, advance bool) (log *syncBuffer, done <-chan int) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	h.env[daemon.SocketEnv] = sock
	log = &syncBuffer{}
	sleep := func(ctx context.Context, d time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
	if advance {
		sleep = func(ctx context.Context, d time.Duration) error {
			h.advance(d)
			return ctx.Err()
		}
	}
	ch := make(chan int, 1)
	go func() {
		ch <- Run(Options{
			Args:         []string{"-daemon", "run"},
			Stdout:       log,
			Stderr:       log,
			Env:          func(k string) string { return h.env[k] },
			Version:      "test",
			HTTP:         h.srv.Client(),
			LookPath:     h.lookPath,
			Now:          h.now,
			LoadConfig:   func(string, string) (*awsconfig.Config, error) { return h.cfg, nil },
			StartDriver:  func(ctx context.Context, cfg webdriver.Config) (BrowserDriver, error) { return h.startDriver(ctx, cfg) },
			browserPoll:  time.Minute,
			browserSleep: sleep,
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := daemon.WaitRunning(ctx, sock); err != nil {
		t.Fatalf("daemon did not start: %v\n%s", err, log.String())
	}
	return log, ch
}

func waitExit(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not exit")
		return -1
	}
}

func TestDaemonHandOffKeepsSessionListed(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	drv, cfg := h.withBrowser(100)
	h.env["XDG_CACHE_HOME"] = t.TempDir()
	log, done := h.startDaemon(t, false)

	if code := h.run("-ab", "chrome", "-p", "default", "-s", "iam", "-c"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	// The client printed nothing (the URL went to the clipboard), told the
	// user where the session lives and returned without a browser of its
	// own.
	if h.stdout.Len() != 0 || len(h.copied) != 1 {
		t.Errorf("stdout = %q, copied = %v", h.stdout.String(), h.copied)
	}
	if s := h.stderr.String(); !strings.Contains(s, "chrome is open; the awsume daemon (pid ") || !strings.Contains(s, "keeps session 1 refreshed every 15m0s") {
		t.Errorf("client stderr = %s", s)
	}
	if cfg.Browser != "chrome" || cfg.ProfileDir != filepath.Join(h.env["XDG_CACHE_HOME"], "awsume", "browser", "chrome") || cfg.Env == nil {
		t.Errorf("daemon driver config = %+v", cfg)
	}

	if code := h.run("-daemon", "status"); code != ExitOK {
		t.Fatalf("status exit %d: %s", code, h.stderr.String())
	}
	if s := h.stdout.String(); !strings.Contains(s, "daemon pid ") || !strings.Contains(s, "session 1: default in chrome, open for 0s, 0 refresh(es)") {
		t.Errorf("status = %s", s)
	}
	if code := h.run("-daemon", "status", "-json"); code != ExitOK || !strings.Contains(h.stdout.String(), `"profile":"default","browser":"chrome"`) {
		t.Errorf("json status exit %d: %s", code, h.stdout.String())
	}

	// A second client is told the daemon is busy with a bad command line.
	h.startDriver = func(ctx context.Context, cfg webdriver.Config) (BrowserDriver, error) {
		return nil, errors.New("no second browser")
	}
	if code := h.run("-ab", "firefox", "default"); code != ExitError || !strings.Contains(h.stderr.String(), "daemon: start firefox: no second browser") {
		t.Errorf("exit %d: %s", code, h.stderr.String())
	}

	if code := h.run("-daemon", "stop"); code != ExitOK || !strings.Contains(h.stderr.String(), "daemon stopping; closing 1 browser session(s)") {
		t.Errorf("stop exit %d: %s", code, h.stderr.String())
	}
	if code := waitExit(t, done); code != ExitOK {
		t.Errorf("daemon exit %d: %s", code, log.String())
	}
	if !drv.quit || len(drv.calls) != 1 || !strings.HasPrefix(drv.calls[0], "navigate "+h.copied[0]) {
		t.Errorf("driver quit=%v calls=%v", drv.quit, drv.calls)
	}
	if s := log.String(); !strings.Contains(s, "session 1 (default): interrupted; closing the browser") || !strings.Contains(s, "daemon: stopped") {
		t.Errorf("daemon log = %s", s)
	}
	if daemon.Running(h.env[daemon.SocketEnv]) {
		t.Error("socket still answers after stop")
	}
}

func TestDaemonRefreshesUntilBrowserCloses(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	drv, _ := h.withBrowser(2)
	log, done := h.startDaemon(t, true)
	if code := h.run("-ab", "chrome", "-p", "default"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if code := h.run("-daemon", "status"); code != ExitOK {
			t.Fatalf("status exit %d: %s", code, h.stderr.String())
		}
		if strings.Contains(h.stdout.String(), "no browser sessions") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not end: %s\n%s", h.stdout.String(), log.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code := h.run("-daemon", "stop"); code != ExitOK {
		t.Fatalf("stop exit %d: %s", code, h.stderr.String())
	}
	waitExit(t, done)
	if !drv.quit || len(drv.calls) != 3 {
		t.Errorf("driver quit=%v calls=%v", drv.quit, drv.calls)
	}
	if s := log.String(); strings.Count(s, "session refreshed (fetch)") != 2 || !strings.Contains(s, "session 1 (default): browser closed") || !strings.Contains(s, "session 1 (default) ended (exit 0)") {
		t.Errorf("daemon log = %s", s)
	}
}

func TestNoDaemonFlagStaysInForeground(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	drv, _ := h.withBrowser(1)
	_, done := h.startDaemon(t, false)
	if code := h.run("-ab", "chrome", "-no-daemon"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if s := h.stderr.String(); !strings.Contains(s, "chrome is open; the console session is refreshed every 15m0s") || !strings.Contains(s, "awsume -daemon start") {
		t.Errorf("stderr = %s", s)
	}
	if !drv.quit {
		t.Error("foreground browser was not quit")
	}
	h.run("-daemon", "stop")
	waitExit(t, done)
}

func TestDaemonCommandErrors(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	h.env[daemon.SocketEnv] = filepath.Join(t.TempDir(), "none.sock")
	if code := h.run("-daemon", "bogus"); code != ExitUsage || !strings.Contains(h.stderr.String(), "unknown action") {
		t.Errorf("exit %d: %s", code, h.stderr.String())
	}
	for _, action := range []string{"status", "stop"} {
		if code := h.run("-daemon", action); code != ExitError || !strings.Contains(h.stderr.String(), "not running") {
			t.Errorf("%s: exit %d: %s", action, code, h.stderr.String())
		}
	}
	// Without a daemon -ab runs in the foreground as before.
	drv, _ := h.withBrowser(1)
	if code := h.run("-ab", "chrome"); code != ExitOK || !drv.quit {
		t.Errorf("exit %d quit=%v: %s", code, drv.quit, h.stderr.String())
	}
	// Two daemons cannot share a socket.
	_, done := h.startDaemon(t, false)
	if code := h.run("-daemon", "run"); code != ExitError || !strings.Contains(h.stderr.String(), "already running") {
		t.Errorf("second daemon exit %d: %s", code, h.stderr.String())
	}
	h.run("-daemon", "stop")
	waitExit(t, done)
}

func TestDaemonStartDetaches(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	sock := filepath.Join(t.TempDir(), "d.sock")
	h.env[daemon.SocketEnv] = sock
	h.env["XDG_CACHE_HOME"] = t.TempDir()
	var got []string
	detached := make(chan struct{})
	h.detach = func(exe string, args []string, logPath string) (int, error) {
		got = append([]string{exe, logPath}, args...)
		// Stand in for the detached process.
		go func() {
			ln, err := daemon.Listen(sock)
			if err != nil {
				t.Error(err)
				return
			}
			close(detached)
			ctx, cancel := context.WithCancel(context.Background())
			_ = daemon.Serve(ctx, ln, func(context.Context, daemon.Request) daemon.Response {
				cancel()
				return daemon.Response{OK: true}
			})
		}()
		return 4242, nil
	}
	if code := h.run("-daemon", "start", "-v"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	<-detached
	want := []string{"/opt/awsume", filepath.Join(h.env["XDG_CACHE_HOME"], "awsume", "daemon.log"), "-daemon", "run", "-v"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("detach args = %v, want %v", got, want)
	}
	if s := h.stderr.String(); !strings.Contains(s, "daemon started (pid 4242) on "+sock) {
		t.Errorf("stderr = %s", s)
	}
	// Already running: nothing is started again.
	got = nil
	if code := h.run("-daemon", "start"); code != ExitOK || got != nil || !strings.Contains(h.stderr.String(), "already running") {
		t.Errorf("exit %d got=%v: %s", code, got, h.stderr.String())
	}
	h.run("-daemon", "stop")
}

func TestDaemonInstallLinux(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	home := t.TempDir()
	h.env["HOME"] = home
	h.env["XDG_RUNTIME_DIR"] = "/run/user/1000"
	h.serviceGOOS = "linux"
	h.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	var cmds []string
	h.runCommand = func(name string, args ...string) error {
		cmds = append(cmds, name+" "+strings.Join(args, " "))
		return nil
	}
	if code := h.run("-daemon", "install"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	unit := filepath.Join(home, ".config", "systemd", "user", "awsume.service")
	data, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(data); !strings.Contains(s, "ExecStart=/opt/awsume -daemon run\n") || !strings.Contains(s, "Environment=AWSUME_DAEMON_SOCKET=/run/user/1000/awsume/daemon.sock\n") || !strings.Contains(s, "WantedBy=default.target") {
		t.Errorf("unit = %s", s)
	}
	if want := []string{"systemctl --user daemon-reload", "systemctl --user enable --now awsume.service"}; strings.Join(cmds, "|") != strings.Join(want, "|") {
		t.Errorf("commands = %v", cmds)
	}
	if s := h.stderr.String(); !strings.Contains(s, "wrote "+unit) || !strings.Contains(s, "enabled and started awsume.service") {
		t.Errorf("stderr = %s", s)
	}
	cmds = nil
	if code := h.run("-daemon", "uninstall"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if _, err := os.Stat(unit); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit still exists: %v", err)
	}
	if want := []string{"systemctl --user disable --now awsume.service", "systemctl --user daemon-reload"}; strings.Join(cmds, "|") != strings.Join(want, "|") {
		t.Errorf("commands = %v", cmds)
	}
	// Without systemctl the unit is written but the command fails loudly.
	h.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if code := h.run("-daemon", "install"); code != ExitError || !strings.Contains(h.stderr.String(), "systemctl not found") {
		t.Errorf("exit %d: %s", code, h.stderr.String())
	}
}

func TestAbsolutize(t *testing.T) {
	f := flags{configFile: "cfg", credentialsFile: "/abs/creds", chromedriver: "chromedriver", geckodriver: "./bin/geckodriver", browserBinary: "google-chrome"}
	f.absolutize("/work")
	if f.configFile != filepath.Join("/work", "cfg") || f.credentialsFile != "/abs/creds" || f.chromedriver != "chromedriver" || f.geckodriver != filepath.Join("/work", "bin/geckodriver") || f.browserBinary != "google-chrome" {
		t.Errorf("flags = %+v", f)
	}
}

func TestPrefixWriter(t *testing.T) {
	var out bytes.Buffer
	w := &prefixWriter{w: &out, now: func() time.Time { return time.Date(2026, 1, 1, 9, 5, 7, 0, time.UTC) }, tag: "session 1 (prod)"}
	w.Write([]byte("awsume: session "))
	w.Write([]byte("refreshed (fetch)\nplain line\npartial"))
	if got, want := out.String(), "awsume: 09:05:07 session 1 (prod): session refreshed (fetch)\nawsume: 09:05:07 session 1 (prod): plain line\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
