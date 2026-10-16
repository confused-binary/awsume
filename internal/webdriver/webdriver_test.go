package webdriver

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"awsume/internal/proxy"
)

// fakeDriver serves just enough of the WebDriver protocol to test the
// client. It records every request body it receives.
type fakeDriver struct {
	mu       sync.Mutex
	requests []string
	cookies  []Cookie
	gone     bool
}

func (f *fakeDriver) handler(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(data))))
	gone := f.gone
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	write := func(v any) { json.NewEncoder(w).Encode(map[string]any{"value": v}) }
	if gone && strings.HasPrefix(r.URL.Path, "/session/") {
		w.WriteHeader(404)
		write(map[string]any{"error": "invalid session id", "message": "session deleted"})
		return
	}
	switch {
	case r.URL.Path == "/status":
		write(map[string]any{"ready": true, "message": "ok"})
	case r.URL.Path == "/session" && r.Method == http.MethodPost:
		var in struct {
			Capabilities struct {
				AlwaysMatch map[string]any `json:"alwaysMatch"`
			} `json:"capabilities"`
		}
		json.Unmarshal(data, &in)
		write(map[string]any{"sessionId": "S1", "capabilities": in.Capabilities.AlwaysMatch})
	case r.URL.Path == "/session/S1" && r.Method == http.MethodDelete:
		write(nil)
	case r.URL.Path == "/session/S1/url" && r.Method == http.MethodGet:
		write("https://us-east-1.console.aws.amazon.com/console/home")
	case r.URL.Path == "/session/S1/url":
		write(nil)
	case r.URL.Path == "/session/S1/window/handles":
		write([]string{"W1", "W2"})
	case r.URL.Path == "/session/S1/window" && r.Method == http.MethodGet:
		write("W1")
	case r.URL.Path == "/session/S1/window/new":
		write(map[string]any{"handle": "W3", "type": "tab"})
	case r.URL.Path == "/session/S1/window" && r.Method == http.MethodDelete:
		write([]string{"W1"})
	case r.URL.Path == "/session/S1/window":
		write(nil)
	case r.URL.Path == "/session/S1/cookie":
		write(f.cookies)
	case r.URL.Path == "/session/S1/execute/async", r.URL.Path == "/session/S1/execute/sync":
		var in struct {
			Script string `json:"script"`
			Args   []any  `json:"args"`
		}
		json.Unmarshal(data, &in)
		if strings.Contains(in.Script, "throw") {
			w.WriteHeader(500)
			write(map[string]any{"error": "javascript error", "message": "boom"})
			return
		}
		write(in.Args)
	case r.URL.Path == "/session/S1/timeouts":
		write(nil)
	case r.URL.Path == "/session/S1/goog/cdp/execute":
		write(map[string]any{"targetId": "T1"})
	default:
		w.WriteHeader(404)
		write(map[string]any{"error": "unknown command", "message": r.URL.Path})
	}
}

// TestMain lets the test binary act as a fake chromedriver/geckodriver
// when WEBDRIVER_FAKE is set, so Start can be exercised end to end.
func TestMain(m *testing.M) {
	if os.Getenv("WEBDRIVER_FAKE") == "" {
		os.Exit(m.Run())
	}
	fs := flag.NewFlagSet("fake", flag.ContinueOnError)
	port := fs.Int("port", 0, "")
	host := fs.String("host", "", "")
	fs.Parse(os.Args[1:])
	_ = host
	f := &fakeDriver{}
	fmt.Fprintf(os.Stderr, "fake driver on %d\n", *port)
	if os.Getenv("WEBDRIVER_FAKE") == "die" {
		os.Exit(3)
	}
	http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), http.HandlerFunc(f.handler))
}

func newTestDriver(t *testing.T, browser string) (*Driver, *fakeDriver) {
	t.Helper()
	f := &fakeDriver{cookies: []Cookie{{Name: "aws-creds", Value: "v1", Domain: ".console.aws.amazon.com", HTTPOnly: true}}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return &Driver{browser: browser, base: srv.URL, session: "S1", http: srv.Client(), logf: t.Logf}, f
}

func TestCommands(t *testing.T) {
	d, f := newTestDriver(t, Chrome)
	ctx := context.Background()
	if err := d.Navigate(ctx, "https://example.com/?a=1&b=2"); err != nil {
		t.Fatal(err)
	}
	if u, err := d.CurrentURL(ctx); err != nil || !strings.Contains(u, "console.aws.amazon.com") {
		t.Errorf("url = %q, %v", u, err)
	}
	if hs, err := d.WindowHandles(ctx); err != nil || len(hs) != 2 {
		t.Errorf("handles = %v, %v", hs, err)
	}
	if h, err := d.CurrentWindow(ctx); err != nil || h != "W1" {
		t.Errorf("window = %v, %v", h, err)
	}
	if err := d.SwitchWindow(ctx, "W2"); err != nil {
		t.Error(err)
	}
	if h, err := d.NewTab(ctx); err != nil || h != "W3" {
		t.Errorf("new tab = %v, %v", h, err)
	}
	if err := d.CloseWindow(ctx); err != nil {
		t.Error(err)
	}
	raw, err := d.ExecuteAsyncScript(ctx, "return 1", "x", 2)
	if err != nil || string(raw) != `["x",2]` {
		t.Errorf("async = %s, %v", raw, err)
	}
	raw, err = d.ExecuteScript(ctx, "return 1")
	if err != nil || string(raw) != `[]` {
		t.Errorf("sync = %s, %v", raw, err)
	}
	if _, err := d.ExecuteScript(ctx, "throw 1"); err == nil || !strings.Contains(err.Error(), "javascript error: boom") || IsGone(err) {
		t.Errorf("script error = %v", err)
	}
	cs, err := d.Cookies(ctx)
	if err != nil || len(cs) != 1 || cs[0].Name != "aws-creds" || !cs[0].HTTPOnly {
		t.Errorf("cookies = %+v, %v", cs, err)
	}
	if err := d.SetTimeouts(ctx, 30*time.Second, time.Minute); err != nil {
		t.Error(err)
	}
	raw, err = d.CDP(ctx, "Target.createTarget", map[string]any{"url": "about:blank"})
	if err != nil || !strings.Contains(string(raw), "T1") {
		t.Errorf("cdp = %s, %v", raw, err)
	}
	want := []string{
		`POST /session/S1/url {"url":"https://example.com/?a=1&b=2"}`,
		`GET /session/S1/url`,
		`GET /session/S1/window/handles`,
		`GET /session/S1/window`,
		`POST /session/S1/window {"handle":"W2"}`,
		`POST /session/S1/window/new {"type":"tab"}`,
		`DELETE /session/S1/window`,
		`POST /session/S1/execute/async {"args":["x",2],"script":"return 1"}`,
		`POST /session/S1/execute/sync {"args":[],"script":"return 1"}`,
		`POST /session/S1/execute/sync {"args":[],"script":"throw 1"}`,
		`GET /session/S1/cookie`,
		`POST /session/S1/timeouts {"pageLoad":60000,"script":30000}`,
		`POST /session/S1/goog/cdp/execute {"cmd":"Target.createTarget","params":{"url":"about:blank"}}`,
	}
	if got := strings.Join(f.requests, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}

	f.gone = true
	if _, err := d.WindowHandles(ctx); !IsGone(err) {
		t.Errorf("expected gone, got %v", err)
	}
	fd, _ := newTestDriver(t, Firefox)
	if _, err := fd.CDP(ctx, "X", nil); err == nil {
		t.Error("cdp must fail on firefox")
	}
}

func TestIsGone(t *testing.T) {
	cases := map[error]bool{
		nil:                                false,
		&Error{Code: "invalid session id"}: true,
		&Error{Code: "no such window", Message: "target window already closed"}:                          true,
		&Error{Code: "unknown error", Message: "chrome not reachable"}:                                   true,
		&Error{Code: "unknown error", Message: "Tried to run command without establishing a connection"}: true,
		&Error{Code: "javascript error", Message: "x is not defined"}:                                    false,
		&net.OpError{Op: "dial", Err: errors.New("connection refused")}:                                  true,
		&url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}:     true,
		&url.Error{Op: "Post", Err: context.DeadlineExceeded}:                                            false,
		fmt.Errorf("wrapped: %w", context.Canceled):                                                      false,
		io.EOF:              true,
		errors.New("other"): false,
	}
	for err, want := range cases {
		if IsGone(err) != want {
			t.Errorf("IsGone(%v) = %v, want %v", err, !want, want)
		}
	}
	e := &Error{Status: 500, Code: "unknown error", Message: "first line\nstack"}
	if e.Error() != "webdriver: unknown error: first line" {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestCapabilities(t *testing.T) {
	p, _ := proxy.Parse("socks5://127.0.0.1:1080")
	caps := capabilities(Chrome, "/usr/bin/google-chrome", Config{Proxy: p, Insecure: true, Headless: true})
	data, _ := json.Marshal(caps)
	s := string(data)
	for _, want := range []string{`"browserName":"chrome"`, `"acceptInsecureCerts":true`, `"binary":"/usr/bin/google-chrome"`, `--proxy-server=socks5://127.0.0.1:1080`, `--proxy-bypass-list=`, `--ignore-certificate-errors`, `--headless=new`, `"excludeSwitches":["enable-automation"]`} {
		if !strings.Contains(s, want) {
			t.Errorf("chrome caps missing %s: %s", want, s)
		}
	}
	caps = capabilities(Chrome, "", Config{})
	data, _ = json.Marshal(caps)
	if strings.Contains(string(data), "binary") || strings.Contains(string(data), "proxy") || strings.Contains(string(data), "headless") || strings.Contains(string(data), "user-data-dir") || !strings.Contains(string(data), `"acceptInsecureCerts":false`) {
		t.Errorf("default chrome caps = %s", data)
	}
	caps = capabilities(Chrome, "", Config{ProfileDir: "/tmp/awsume-chrome"})
	data, _ = json.Marshal(caps)
	if !strings.Contains(string(data), `"--user-data-dir=/tmp/awsume-chrome"`) {
		t.Errorf("chrome caps with profile = %s", data)
	}
	caps = capabilities(Firefox, "", Config{ProfileDir: "/tmp/awsume-ff"})
	if got := strings.Join(caps["moz:firefoxOptions"].(map[string]any)["args"].([]string), " "); got != "--width=1920 --height=1080 -profile /tmp/awsume-ff" {
		t.Errorf("firefox args with profile = %s", got)
	}

	caps = capabilities(Firefox, "", Config{Proxy: p})
	prefs := caps["moz:firefoxOptions"].(map[string]any)["prefs"].(map[string]any)
	if prefs["network.proxy.type"] != 1 || prefs["network.proxy.socks"] != "127.0.0.1" || prefs["network.proxy.socks_port"] != 1080 || prefs["network.proxy.socks_version"] != 5 || prefs["network.proxy.socks_remote_dns"] != true || prefs["network.proxy.allow_hijacking_localhost"] != true {
		t.Errorf("firefox socks prefs = %v", prefs)
	}
	hp, _ := proxy.Parse("127.0.0.1:8080")
	caps = capabilities(Firefox, "/usr/bin/firefox", Config{Proxy: hp, Headless: true})
	opts := caps["moz:firefoxOptions"].(map[string]any)
	prefs = opts["prefs"].(map[string]any)
	if prefs["network.proxy.http"] != "127.0.0.1" || prefs["network.proxy.http_port"] != 8080 || prefs["network.proxy.ssl_port"] != 8080 || prefs["network.proxy.no_proxies_on"] != "" {
		t.Errorf("firefox http prefs = %v", prefs)
	}
	if opts["binary"] != "/usr/bin/firefox" || strings.Join(opts["args"].([]string), " ") != "--width=1920 --height=1080 -headless" {
		t.Errorf("firefox opts = %v", opts)
	}
	caps = capabilities(Firefox, "", Config{})
	if len(caps["moz:firefoxOptions"].(map[string]any)["prefs"].(map[string]any)) != 0 {
		t.Error("firefox without proxy must set no prefs")
	}
}

func TestProfileInUse(t *testing.T) {
	dir := t.TempDir()
	if profileInUse(Chrome, dir) || profileInUse(Firefox, dir) {
		t.Fatal("empty profile reported in use")
	}
	// Chrome's lock is a dangling symlink to host-pid; Lstat must see it.
	if err := os.Symlink("host-12345", filepath.Join(dir, "SingletonLock")); err != nil {
		t.Skip(err)
	}
	if !profileInUse(Chrome, dir) || profileInUse(Firefox, dir) {
		t.Error("chrome lock not detected or misattributed")
	}
	if err := os.WriteFile(filepath.Join(dir, ".parentlock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !profileInUse(Firefox, dir) {
		t.Error("firefox lock not detected")
	}
}

func TestNormalizeBrowser(t *testing.T) {
	for in, want := range map[string]string{"Chrome": Chrome, "chromium": Chrome, "google-chrome": Chrome, "firefox": Firefox, "FF": Firefox, " firefox-esr ": Firefox} {
		if got, err := NormalizeBrowser(in); err != nil || got != want {
			t.Errorf("NormalizeBrowser(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := NormalizeBrowser("safari"); err == nil {
		t.Error("safari must be rejected")
	}
}

// TestStartWithFakeDriver re-executes the test binary as the driver.
func TestStartWithFakeDriver(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	os.Setenv("WEBDRIVER_FAKE", "1")
	defer os.Unsetenv("WEBDRIVER_FAKE")
	var log strings.Builder
	d, err := Start(context.Background(), Config{
		Browser:      "chromium",
		DriverPath:   exe,
		BinaryPath:   "/nonexistent/chrome",
		Logf:         func(f string, a ...any) { fmt.Fprintf(&log, f+"\n", a...) },
		DriverOutput: &log,
		StartTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v\n%s", err, log.String())
	}
	if d.Browser() != Chrome || d.session != "S1" || !strings.Contains(string(d.Capabilities), `"binary":"/nonexistent/chrome"`) {
		t.Errorf("driver = %+v", d)
	}
	if hs, err := d.WindowHandles(context.Background()); err != nil || len(hs) != 2 {
		t.Errorf("handles = %v, %v", hs, err)
	}
	if err := d.Quit(); err != nil {
		t.Errorf("quit: %v", err)
	}
	if !d.Exited() || !strings.Contains(log.String(), "fake driver on") {
		t.Errorf("driver process not reaped: %s", log.String())
	}
	if _, err := d.WindowHandles(context.Background()); !IsGone(err) {
		t.Errorf("after quit: %v", err)
	}

	// A driver that exits immediately is reported, not waited for.
	os.Setenv("WEBDRIVER_FAKE", "die")
	_, err = Start(context.Background(), Config{Browser: Firefox, DriverPath: exe, StartTimeout: 5 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Errorf("dead driver: %v", err)
	}
	// Missing driver.
	statFile = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	defer func() { statFile = os.Stat }()
	_, err = Start(context.Background(), Config{Browser: Firefox, LookPath: func(string) (string, error) { return "", errors.New("no") }})
	if err == nil || !strings.Contains(err.Error(), "geckodriver not found") {
		t.Errorf("missing driver: %v", err)
	}
	pw, _ := proxy.Parse("http://u:p@127.0.0.1:8080")
	_, err = Start(context.Background(), Config{Browser: Chrome, DriverPath: exe, Proxy: pw})
	if !errors.Is(err, proxy.ErrCredentialsUnsupported) {
		t.Errorf("proxy credentials: %v", err)
	}
}
