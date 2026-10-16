package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awsume/internal/webdriver"
)

// cliFakeDriver is a browser whose console cookies change whenever a
// sign-in URL is fetched, and which "closes" after a given number of
// refreshes.
type cliFakeDriver struct {
	url       string
	cookie    string
	calls     []string
	refreshes int
	closeAt   int
	gone      bool
	quit      bool
}

var errSessionGone = &webdriver.Error{Code: "invalid session id", Message: "session deleted"}

func (d *cliFakeDriver) Browser() string { return webdriver.Chrome }
func (d *cliFakeDriver) Navigate(ctx context.Context, u string) error {
	d.calls = append(d.calls, "navigate "+u)
	d.url = "https://us-east-1.console.aws.amazon.com/console/home"
	return nil
}
func (d *cliFakeDriver) CurrentURL(ctx context.Context) (string, error) { return d.url, nil }
func (d *cliFakeDriver) WindowHandles(ctx context.Context) ([]string, error) {
	if d.gone {
		return nil, errSessionGone
	}
	return []string{"W1"}, nil
}
func (d *cliFakeDriver) CurrentWindow(ctx context.Context) (string, error) { return "W1", nil }
func (d *cliFakeDriver) SwitchWindow(ctx context.Context, h string) error  { return nil }
func (d *cliFakeDriver) NewTab(ctx context.Context) (string, error)        { return "T1", nil }
func (d *cliFakeDriver) CloseWindow(ctx context.Context) error             { return nil }
func (d *cliFakeDriver) ExecuteAsyncScript(ctx context.Context, script string, args ...any) (json.RawMessage, error) {
	d.calls = append(d.calls, "fetch "+args[0].(string))
	d.refreshes++
	d.cookie = "session-" + string(rune('0'+d.refreshes))
	if d.refreshes >= d.closeAt {
		d.gone = true
	}
	return json.RawMessage(`"ok opaque"`), nil
}
func (d *cliFakeDriver) Cookies(ctx context.Context) ([]webdriver.Cookie, error) {
	return []webdriver.Cookie{{Name: "aws-creds", Value: d.cookie, Domain: ".console.aws.amazon.com", Path: "/"}}, nil
}
func (d *cliFakeDriver) SetTimeouts(ctx context.Context, script, pageLoad time.Duration) error {
	return nil
}
func (d *cliFakeDriver) CDP(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return nil, errors.New("not used")
}
func (d *cliFakeDriver) Quit() error { d.quit = true; return nil }

func (h *harness) withBrowser(closeAt int) (*cliFakeDriver, *webdriver.Config) {
	drv := &cliFakeDriver{closeAt: closeAt}
	var got webdriver.Config
	h.startDriver = func(ctx context.Context, cfg webdriver.Config) (BrowserDriver, error) {
		got = cfg
		return drv, nil
	}
	return drv, &got
}

// newForwardingProxy starts an HTTP proxy that records the absolute URLs
// it is asked for and forwards them.
func newForwardingProxy(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.String())
		req, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(px.Close)
	return px, &seen
}

func TestAutoBrowserRefreshesUntilClosed(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	h.isTTY = true
	drv, cfg := h.withBrowser(2)
	h.env["XDG_CACHE_HOME"] = t.TempDir()
	px, seen := newForwardingProxy(t)
	if code := h.run("-ab", "chrome", "-p", "default", "-s", "iam", "-proxy", strings.TrimPrefix(px.URL, "http://"), "-k"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if cfg.Browser != webdriver.Chrome || cfg.Proxy == nil || cfg.Proxy.String() != px.URL || !cfg.Insecure || cfg.ProfileDir != filepath.Join(h.env["XDG_CACHE_HOME"], "awsume", "browser", "chrome") {
		t.Errorf("driver config = %+v", cfg)
	}
	if len(*seen) == 0 {
		t.Error("api calls did not go through the proxy")
	}
	u, q := parseURL(t, h.stdout.String())
	if q.Get("Action") != "login" || !strings.Contains(q.Get("Destination"), "/iam/home") {
		t.Errorf("stdout url = %s", u)
	}
	// The browser opened the printed URL, then two refreshes were applied
	// in the console tab before the browser went away.
	if len(drv.calls) != 3 || drv.calls[0] != "navigate "+strings.TrimSpace(h.stdout.String()) || !strings.HasPrefix(drv.calls[1], "fetch http") || !strings.HasPrefix(drv.calls[2], "fetch http") {
		t.Errorf("driver calls = %v", drv.calls)
	}
	if !drv.quit {
		t.Error("driver was not quit after the browser closed")
	}
	// Each refresh generated a new sign-in: identity check plus federation
	// token, three times in total.
	var gft int
	for _, c := range h.aws.stsCalls {
		if c.Get("Action") == "GetFederationToken" {
			gft++
		}
	}
	if gft != 3 || len(h.aws.federationCall) != 3 {
		t.Errorf("sts gft=%d federation=%d", gft, len(h.aws.federationCall))
	}
	if s := h.stderr.String(); !strings.Contains(s, "chrome is open; the console session is refreshed every 15m0s") || strings.Count(s, "session refreshed (fetch)") != 2 {
		t.Errorf("stderr = %s", s)
	}
}

func TestAutoBrowserRoleSessionNameIsStable(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	h.aws.denyFederationToken = true
	_, _ = h.withBrowser(1)
	if code := h.run("-ab", "firefox", "-role-arn", "arn:aws:iam::123456789012:role/Admin", "-no-identity-check", "-json", "default"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	var rec record
	if err := json.Unmarshal(h.stdout.Bytes(), &rec); err != nil || rec.Method != "assume-role" {
		t.Errorf("record = %+v, %v", rec, err)
	}
	for _, c := range h.aws.stsCalls {
		if c.Get("Action") == "AssumeRole" && c.Get("RoleSessionName") != "awsume" {
			t.Errorf("session name = %q", c.Get("RoleSessionName"))
		}
	}
	// The profile-based role chain gets the same stable name.
	h = newHarness(t, testConfig, testCredentials)
	h.aws.identityARN = "arn:aws:sts::123456789012:assumed-role/Dev/awsume"
	_, _ = h.withBrowser(1)
	if code := h.run("-ab", "chrome", "-session-name", "pentest", "role"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if c := h.aws.stsCalls[0]; c.Get("Action") != "AssumeRole" || c.Get("RoleSessionName") != "pentest" {
		t.Errorf("first sts call = %v", c)
	}
}

func TestBrowserProfileDir(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := browserProfileDir(webdriver.Chrome, env(map[string]string{"XDG_CACHE_HOME": "/x", "HOME": "/h"})); got != "/x/awsume/browser/chrome" {
		t.Errorf("xdg: %s", got)
	}
	if got := browserProfileDir(webdriver.Firefox, env(map[string]string{"HOME": "/h"})); got != "/h/.cache/awsume/browser/firefox" {
		t.Errorf("home: %s", got)
	}
}

func TestAutoBrowserUsageErrors(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	_, _ = h.withBrowser(1)
	for _, args := range [][]string{
		{"-ab", "safari"},
		{"-ab", "chrome", "-refresh", "30s"},
		{"-ab", "chrome", "default", "gov"},
		{"-proxy", "ftp://x"},
	} {
		if code := h.run(args...); code != ExitUsage {
			t.Errorf("%v: exit %d, want usage error: %s", args, code, h.stderr.String())
		}
	}
	h.startDriver = func(ctx context.Context, cfg webdriver.Config) (BrowserDriver, error) {
		return nil, errors.New("chromedriver not found")
	}
	if code := h.run("-ab", "chrome"); code != ExitError || !strings.Contains(h.stderr.String(), "start chrome: chromedriver not found") {
		t.Errorf("exit %d: %s", code, h.stderr.String())
	}
}

// TestProxyFlag routes the STS and federation calls through a local
// forwarding proxy and checks they arrive there.
func TestProxyFlag(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	px, seenp := newForwardingProxy(t)
	h.isTTY = true
	if code := h.run("-proxy", px.URL, "-v"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	seen := *seenp
	if len(seen) != 3 || !strings.HasPrefix(seen[0], "POST "+h.srv.URL+"/sts") || !strings.HasPrefix(seen[2], "GET "+h.srv.URL+"/federation") {
		t.Errorf("proxy saw %v", seen)
	}
	if !strings.Contains(h.stderr.String(), "proxy http://127.0.0.1:") {
		t.Errorf("stderr = %s", h.stderr.String())
	}
}
