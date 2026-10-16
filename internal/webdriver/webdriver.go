// Package webdriver is a minimal W3C WebDriver client that launches
// chromedriver or geckodriver, starts a browser through it and drives the
// browser over the driver's local HTTP endpoint. It needs nothing outside
// the standard library; this is the same wire protocol Selenium speaks.
package webdriver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"awsume/internal/proxy"
)

// Browser names accepted by Config.Browser.
const (
	Chrome  = "chrome"
	Firefox = "firefox"
)

// NormalizeBrowser maps the names a user may type to Chrome or Firefox.
func NormalizeBrowser(name string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "chrome", "chromium", "google-chrome", "chromium-browser":
		return Chrome, nil
	case "firefox", "ff", "firefox-esr":
		return Firefox, nil
	}
	return "", fmt.Errorf("unknown browser %q (want chrome or firefox)", name)
}

// Config describes the browser to start.
type Config struct {
	// Browser is Chrome or Firefox.
	Browser string
	// DriverPath is the chromedriver/geckodriver executable; empty means
	// search PATH and the usual install locations.
	DriverPath string
	// BinaryPath is the browser executable; empty lets the driver find it.
	BinaryPath string
	// Proxy routes all browser traffic through a proxy.
	Proxy *proxy.Proxy
	// Insecure makes the browser accept invalid TLS certificates.
	Insecure bool
	// Headless hides the browser window (used by tests).
	Headless bool
	// ProfileDir keeps the browser profile (cookies, local storage and
	// the preferences behind them, such as a dismissed console tour) in
	// this directory across runs. Empty means a throwaway profile.
	ProfileDir string
	// LookPath overrides executable lookup.
	LookPath func(string) (string, error)
	// Logf receives diagnostics; nil discards them.
	Logf func(string, ...any)
	// DriverOutput receives the driver's own stdout/stderr; nil discards.
	DriverOutput io.Writer
	// Env is the environment of the driver process (and so of the browser
	// it starts); nil means the environment of this process.
	Env []string
	// StartTimeout bounds driver start-up and session creation (default 60s).
	StartTimeout time.Duration
}

// Driver is a running browser session.
type Driver struct {
	browser string
	base    string
	session string
	http    *http.Client
	cmd     *exec.Cmd
	exited  chan struct{}
	logf    func(string, ...any)
	// Capabilities is the capabilities document the driver returned.
	Capabilities json.RawMessage
}

// Cookie is one browser cookie as returned by the Get All Cookies command.
type Cookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain,omitempty"`
	Path     string `json:"path,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"httpOnly,omitempty"`
	Expiry   int64  `json:"expiry,omitempty"`
	SameSite string `json:"sameSite,omitempty"`
}

// Error is an error response from the driver.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("webdriver: %s (HTTP %d)", e.Code, e.Status)
	}
	msg := e.Message
	if i := strings.Index(msg, "\n"); i >= 0 {
		msg = msg[:i]
	}
	return fmt.Sprintf("webdriver: %s: %s", e.Code, msg)
}

// goneMessages are driver messages that mean the browser is no longer
// reachable, in addition to the invalid session id and no such window
// error codes.
var goneMessages = []string{
	"chrome not reachable",
	"disconnected",
	"target window already closed",
	"web view not found",
	"session deleted",
	"Tried to run command without establishing a connection",
	"Browsing context has been discarded",
	"browser has closed",
	"no such window",
}

// IsGone reports whether err means the browser has been closed or the
// driver process has gone away, as opposed to a failure of one command.
func IsGone(err error) bool {
	if err == nil {
		return false
	}
	var we *Error
	if errors.As(err, &we) {
		switch we.Code {
		case "invalid session id", "no such window":
			return true
		}
		for _, m := range goneMessages {
			if strings.Contains(we.Message, m) {
				return true
			}
		}
		return false
	}
	// A command that merely ran out of time says nothing about the browser.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var oe *net.OpError
	var ue *url.Error
	return errors.As(err, &oe) || errors.As(err, &ue) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// Start launches the driver and the browser.
func Start(ctx context.Context, cfg Config) (*Driver, error) {
	browser, err := NormalizeBrowser(cfg.Browser)
	if err != nil {
		return nil, err
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	lookPath := cfg.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	timeout := cfg.StartTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if cfg.Proxy != nil && cfg.Proxy.User != nil {
		return nil, proxy.ErrCredentialsUnsupported
	}

	driverPath := cfg.DriverPath
	if driverPath == "" {
		driverPath = findExecutable(lookPath, driverNames(browser), driverPaths(browser))
		if driverPath == "" {
			return nil, fmt.Errorf("%s not found on PATH; install it or pass -%s <path>", driverName(browser), driverName(browser))
		}
	}
	binary := cfg.BinaryPath
	if binary == "" {
		binary = findExecutable(lookPath, binaryNames(browser), binaryPaths(browser))
	}
	logf("%s driver %s, browser binary %s", browser, driverPath, orDefault(binary, "(driver default)"))
	if cfg.ProfileDir != "" {
		switch {
		case profileInUse(browser, cfg.ProfileDir):
			logf("browser profile %s is in use by another browser; using a throwaway profile", cfg.ProfileDir)
			cfg.ProfileDir = ""
		default:
			if err := os.MkdirAll(cfg.ProfileDir, 0o700); err != nil {
				logf("cannot create browser profile %s (%v); using a throwaway profile", cfg.ProfileDir, err)
				cfg.ProfileDir = ""
			} else {
				logf("browser profile kept in %s", cfg.ProfileDir)
			}
		}
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := driverArgs(browser, port)
	cmd := exec.Command(driverPath, args...)
	out := cfg.DriverOutput
	if out == nil {
		out = io.Discard
	}
	cmd.Stdout = out
	cmd.Stderr = out
	if cfg.Env != nil {
		cmd.Env = cfg.Env
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", driverPath, err)
	}
	d := &Driver{
		browser: browser,
		base:    fmt.Sprintf("http://127.0.0.1:%d", port),
		http:    &http.Client{Transport: &http.Transport{Proxy: nil}},
		cmd:     cmd,
		exited:  make(chan struct{}),
		logf:    logf,
	}
	go func() {
		_ = cmd.Wait()
		close(d.exited)
	}()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := d.waitReady(ctx); err != nil {
		d.kill()
		return nil, fmt.Errorf("%s did not become ready: %w", driverName(browser), err)
	}
	caps := capabilities(browser, binary, cfg)
	logf("creating %s session", browser)
	resp, err := d.do(ctx, http.MethodPost, "/session", map[string]any{"capabilities": map[string]any{"alwaysMatch": caps}})
	if err != nil {
		d.kill()
		return nil, fmt.Errorf("start %s: %w", browser, err)
	}
	var sess struct {
		SessionID    string          `json:"sessionId"`
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(resp, &sess); err != nil || sess.SessionID == "" {
		d.kill()
		return nil, fmt.Errorf("start %s: unexpected session response %s", browser, string(resp))
	}
	d.session = sess.SessionID
	d.Capabilities = sess.Capabilities
	logf("%s session %s started", browser, d.session)
	return d, nil
}

// Browser returns Chrome or Firefox.
func (d *Driver) Browser() string { return d.browser }

// Quit ends the session, which closes the browser, and stops the driver.
func (d *Driver) Quit() error {
	var err error
	if d.session != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, err = d.do(ctx, http.MethodDelete, "/session/"+d.session, nil)
		cancel()
		d.session = ""
		if IsGone(err) {
			err = nil
		}
	}
	d.kill()
	return err
}

// kill stops the driver process if it has not exited on its own.
func (d *Driver) kill() {
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	select {
	case <-d.exited:
		return
	case <-time.After(2 * time.Second):
	}
	_ = d.cmd.Process.Kill()
	<-d.exited
}

// Exited reports whether the driver process has stopped.
func (d *Driver) Exited() bool {
	select {
	case <-d.exited:
		return true
	default:
		return false
	}
}

// Navigate loads url in the current window and waits for the page load.
func (d *Driver) Navigate(ctx context.Context, url string) error {
	_, err := d.cmd1(ctx, http.MethodPost, "/url", map[string]any{"url": url})
	return err
}

// CurrentURL returns the URL of the current window.
func (d *Driver) CurrentURL(ctx context.Context) (string, error) {
	var s string
	err := d.cmdInto(ctx, http.MethodGet, "/url", nil, &s)
	return s, err
}

// WindowHandles lists the open windows and tabs.
func (d *Driver) WindowHandles(ctx context.Context) ([]string, error) {
	var hs []string
	err := d.cmdInto(ctx, http.MethodGet, "/window/handles", nil, &hs)
	return hs, err
}

// CurrentWindow returns the handle of the current window.
func (d *Driver) CurrentWindow(ctx context.Context) (string, error) {
	var h string
	err := d.cmdInto(ctx, http.MethodGet, "/window", nil, &h)
	return h, err
}

// SwitchWindow makes handle the current window.
func (d *Driver) SwitchWindow(ctx context.Context, handle string) error {
	_, err := d.cmd1(ctx, http.MethodPost, "/window", map[string]any{"handle": handle})
	return err
}

// NewTab opens a blank tab and returns its handle without switching to it.
func (d *Driver) NewTab(ctx context.Context) (string, error) {
	var out struct {
		Handle string `json:"handle"`
	}
	err := d.cmdInto(ctx, http.MethodPost, "/window/new", map[string]any{"type": "tab"}, &out)
	return out.Handle, err
}

// CloseWindow closes the current window.
func (d *Driver) CloseWindow(ctx context.Context) error {
	_, err := d.cmd1(ctx, http.MethodDelete, "/window", nil)
	return err
}

// ExecuteScript runs script in the current window and returns its result.
func (d *Driver) ExecuteScript(ctx context.Context, script string, args ...any) (json.RawMessage, error) {
	return d.cmd1(ctx, http.MethodPost, "/execute/sync", map[string]any{"script": script, "args": argsOrEmpty(args)})
}

// ExecuteAsyncScript runs script in the current window; the script must
// call its last argument to finish and that value is returned.
func (d *Driver) ExecuteAsyncScript(ctx context.Context, script string, args ...any) (json.RawMessage, error) {
	return d.cmd1(ctx, http.MethodPost, "/execute/async", map[string]any{"script": script, "args": argsOrEmpty(args)})
}

// Cookies returns the cookies visible to the current window's document,
// HttpOnly ones included.
func (d *Driver) Cookies(ctx context.Context) ([]Cookie, error) {
	var cs []Cookie
	err := d.cmdInto(ctx, http.MethodGet, "/cookie", nil, &cs)
	return cs, err
}

// SetTimeouts sets the script and page load timeouts.
func (d *Driver) SetTimeouts(ctx context.Context, script, pageLoad time.Duration) error {
	_, err := d.cmd1(ctx, http.MethodPost, "/timeouts", map[string]any{
		"script":   script.Milliseconds(),
		"pageLoad": pageLoad.Milliseconds(),
	})
	return err
}

// CDP sends a Chrome DevTools Protocol command through chromedriver. It
// fails for other browsers.
func (d *Driver) CDP(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if d.browser != Chrome {
		return nil, fmt.Errorf("devtools commands are only available with chrome, not %s", d.browser)
	}
	if params == nil {
		params = map[string]any{}
	}
	return d.cmd1(ctx, http.MethodPost, "/goog/cdp/execute", map[string]any{"cmd": method, "params": params})
}

func argsOrEmpty(args []any) []any {
	if args == nil {
		return []any{}
	}
	return args
}

func (d *Driver) cmd1(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	if d.session == "" {
		return nil, &Error{Code: "invalid session id", Message: "session has been closed"}
	}
	return d.do(ctx, method, "/session/"+d.session+path, body)
}

func (d *Driver) cmdInto(ctx context.Context, method, path string, body, out any) error {
	raw, err := d.cmd1(ctx, method, path, body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("webdriver: decode %s response %s: %w", path, string(raw), err)
	}
	return nil
}

// do performs one driver request and returns the "value" member.
func (d *Driver) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(body); err != nil {
			return nil, err
		}
		reader = &buf
	} else if method == http.MethodPost {
		reader = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var env struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("webdriver: HTTP %d with non-JSON body from %s %s", resp.StatusCode, method, path)
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(env.Value, &e)
		if e.Error == "" {
			e.Error = "HTTP " + fmt.Sprint(resp.StatusCode)
		}
		return nil, &Error{Status: resp.StatusCode, Code: e.Error, Message: e.Message}
	}
	return env.Value, nil
}

func (d *Driver) waitReady(ctx context.Context) error {
	for {
		raw, err := d.do(ctx, http.MethodGet, "/status", nil)
		if err == nil {
			var st struct {
				Ready bool `json:"ready"`
			}
			if json.Unmarshal(raw, &st) == nil && st.Ready {
				return nil
			}
		}
		if d.Exited() {
			return errors.New("driver process exited")
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
			}
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// capabilities builds the new session capabilities for the browser,
// mirroring the options used by waf-fu's replay backends.
func capabilities(browser, binary string, cfg Config) map[string]any {
	caps := map[string]any{
		"browserName":         browser,
		"acceptInsecureCerts": cfg.Insecure,
	}
	switch browser {
	case Chrome:
		args := []string{
			"--no-first-run",
			"--no-default-browser-check",
			"--disable-dev-shm-usage",
			"--disable-gpu",
			"--no-sandbox",
			"--window-size=1920,1080",
		}
		if cfg.Insecure {
			args = append(args, "--ignore-certificate-errors")
		}
		if cfg.Proxy != nil {
			args = append(args, "--proxy-server="+cfg.Proxy.String(), "--proxy-bypass-list=<-loopback>")
		}
		if cfg.Headless {
			args = append(args, "--headless=new")
		}
		if cfg.ProfileDir != "" {
			args = append(args, "--user-data-dir="+cfg.ProfileDir)
		}
		opts := map[string]any{
			"args":            args,
			"excludeSwitches": []string{"enable-automation"},
		}
		if binary != "" {
			opts["binary"] = binary
		}
		caps["goog:chromeOptions"] = opts
	case Firefox:
		args := []string{"--width=1920", "--height=1080"}
		if cfg.Headless {
			args = append(args, "-headless")
		}
		if cfg.ProfileDir != "" {
			// geckodriver uses a -profile given in args in place, instead
			// of copying a fresh profile to a temporary directory.
			args = append(args, "-profile", cfg.ProfileDir)
		}
		prefs := map[string]any{}
		if p := cfg.Proxy; p != nil {
			prefs["network.proxy.type"] = 1
			prefs["network.proxy.no_proxies_on"] = ""
			prefs["network.proxy.allow_hijacking_localhost"] = true
			port := atoi(p.Port)
			if p.Scheme == "socks5" {
				prefs["network.proxy.socks"] = p.Host
				prefs["network.proxy.socks_port"] = port
				prefs["network.proxy.socks_version"] = 5
				prefs["network.proxy.socks_remote_dns"] = true
			} else {
				prefs["network.proxy.http"] = p.Host
				prefs["network.proxy.http_port"] = port
				prefs["network.proxy.ssl"] = p.Host
				prefs["network.proxy.ssl_port"] = port
			}
		}
		opts := map[string]any{"args": args, "prefs": prefs}
		if binary != "" {
			opts["binary"] = binary
		}
		caps["moz:firefoxOptions"] = opts
	}
	return caps
}

// profileLocks are the files a running browser keeps in its profile
// directory. A stale one after a crash only costs a throwaway profile.
var profileLocks = map[string][]string{
	Chrome:  {"SingletonLock", "lockfile"},
	Firefox: {"lock", ".parentlock", "parent.lock"},
}

// profileInUse reports whether another browser has dir open.
func profileInUse(browser, dir string) bool {
	for _, name := range profileLocks[browser] {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func driverName(browser string) string {
	if browser == Firefox {
		return "geckodriver"
	}
	return "chromedriver"
}

func driverNames(browser string) []string {
	return []string{driverName(browser)}
}

func driverArgs(browser string, port int) []string {
	if browser == Firefox {
		return []string{"--port", fmt.Sprint(port), "--host", "127.0.0.1"}
	}
	return []string{fmt.Sprintf("--port=%d", port)}
}

func driverPaths(browser string) []string {
	switch runtime.GOOS {
	case "windows", "darwin":
		return nil
	}
	if browser == Firefox {
		return []string{"/usr/local/bin/geckodriver", "/usr/bin/geckodriver", "/snap/bin/geckodriver"}
	}
	return []string{
		"/usr/lib64/chromium-browser/chromedriver",
		"/usr/local/bin/chromedriver",
		"/usr/lib/chromium-browser/chromedriver",
		"/opt/google/chrome/chromedriver",
		"/snap/bin/chromedriver",
		"/usr/bin/chromedriver",
	}
}

func binaryNames(browser string) []string {
	if browser == Firefox {
		return []string{"firefox", "firefox-esr"}
	}
	return []string{"google-chrome-stable", "google-chrome", "chromium-browser", "chromium", "chrome"}
}

func binaryPaths(browser string) []string {
	switch runtime.GOOS {
	case "darwin":
		if browser == Firefox {
			return []string{"/Applications/Firefox.app/Contents/MacOS/firefox"}
		}
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "windows":
		if browser == Firefox {
			return []string{
				`C:\Program Files\Mozilla Firefox\firefox.exe`,
				`C:\Program Files (x86)\Mozilla Firefox\firefox.exe`,
			}
		}
		return []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		}
	}
	if browser == Firefox {
		return []string{"/usr/lib64/firefox/firefox", "/usr/lib/firefox/firefox", "/snap/bin/firefox"}
	}
	return []string{
		"/opt/google/chrome/google-chrome",
		"/usr/lib64/chromium-browser/chromium-browser",
		"/usr/lib/chromium-browser/chromium-browser",
		"/snap/bin/chromium",
	}
}

// statFile checks the fallback install locations; tests override it.
var statFile = os.Stat

func findExecutable(lookPath func(string) (string, error), names, paths []string) string {
	for _, n := range names {
		if p, err := lookPath(n); err == nil {
			return p
		}
	}
	for _, p := range paths {
		if info, err := statFile(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find a free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
