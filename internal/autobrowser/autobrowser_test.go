package autobrowser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"awsume/internal/webdriver"
)

// fakeDriver scripts the browser's reaction to each refresh strategy.
type fakeDriver struct {
	browser string
	windows map[string]string // handle -> url
	current string
	cookies map[string]string
	calls   []string
	gone    bool
	nextTab int

	// What each strategy does: "change" replaces the cookies, "error"
	// fails, "" does nothing.
	fetch, iframe, cdp, tab string
	cdpHidden               bool
}

var errGone = &webdriver.Error{Code: "invalid session id", Message: "session deleted"}

func newFake(browser string) *fakeDriver {
	return &fakeDriver{
		browser: browser,
		windows: map[string]string{"W1": "https://us-east-1.console.aws.amazon.com/ec2/home"},
		current: "W1",
		cookies: map[string]string{"aws-creds": "old", "aws-userInfo": "old"},
		fetch:   "change",
	}
}

func (f *fakeDriver) record(s string) { f.calls = append(f.calls, s) }
func (f *fakeDriver) Browser() string { return f.browser }
func (f *fakeDriver) Navigate(ctx context.Context, u string) error {
	f.record("navigate " + f.current)
	if f.gone {
		return errGone
	}
	f.windows[f.current] = u
	if strings.Contains(u, "SigninToken") {
		// The sign-in flow redirects to the console.
		f.windows[f.current] = "https://us-east-1.console.aws.amazon.com/console/home"
		if f.tab == "change" {
			f.change()
		}
	}
	return nil
}
func (f *fakeDriver) change() {
	for k := range f.cookies {
		f.cookies[k] = "new"
	}
	f.cookies["aws-signin-extra"] = "x"
}
func (f *fakeDriver) CurrentURL(ctx context.Context) (string, error) {
	if f.gone {
		return "", errGone
	}
	u, ok := f.windows[f.current]
	if !ok {
		return "", &webdriver.Error{Code: "no such window", Message: "target window already closed"}
	}
	return u, nil
}
func (f *fakeDriver) WindowHandles(ctx context.Context) ([]string, error) {
	f.record("handles")
	if f.gone {
		return nil, errGone
	}
	var hs []string
	for _, h := range []string{"W1", "W2", "W3", "T1", "T2"} {
		if _, ok := f.windows[h]; ok {
			hs = append(hs, h)
		}
	}
	return hs, nil
}
func (f *fakeDriver) CurrentWindow(ctx context.Context) (string, error) {
	if f.gone {
		return "", errGone
	}
	if _, ok := f.windows[f.current]; !ok {
		return "", &webdriver.Error{Code: "no such window", Message: "target window already closed"}
	}
	return f.current, nil
}
func (f *fakeDriver) SwitchWindow(ctx context.Context, h string) error {
	f.record("switch " + h)
	if _, ok := f.windows[h]; !ok {
		return &webdriver.Error{Code: "no such window"}
	}
	f.current = h
	return nil
}
func (f *fakeDriver) NewTab(ctx context.Context) (string, error) {
	f.nextTab++
	h := fmt.Sprintf("T%d", f.nextTab)
	f.windows[h] = "about:blank"
	f.record("newtab " + h)
	return h, nil
}
func (f *fakeDriver) CloseWindow(ctx context.Context) error {
	f.record("close " + f.current)
	delete(f.windows, f.current)
	return nil
}
func (f *fakeDriver) ExecuteAsyncScript(ctx context.Context, script string, args ...any) (json.RawMessage, error) {
	kind, mode := "fetch", f.fetch
	if strings.Contains(script, "iframe") {
		kind, mode = "iframe", f.iframe
	}
	f.record(kind + " " + fmt.Sprint(args[0]))
	if !strings.Contains(f.windows[f.current], "console") {
		return nil, errors.New("script ran outside the console")
	}
	switch mode {
	case "change":
		f.change()
		return json.RawMessage(`"ok opaque"`), nil
	case "error":
		return json.RawMessage(`"error TypeError: Failed to fetch"`), nil
	case "throw":
		return nil, &webdriver.Error{Code: "javascript error", Message: "boom"}
	}
	return json.RawMessage(`"ok"`), nil
}
func (f *fakeDriver) Cookies(ctx context.Context) ([]webdriver.Cookie, error) {
	if f.gone {
		return nil, errGone
	}
	var cs []webdriver.Cookie
	for k, v := range f.cookies {
		cs = append(cs, webdriver.Cookie{Name: k, Value: v, Domain: ".console.aws.amazon.com", Path: "/"})
	}
	return cs, nil
}
func (f *fakeDriver) SetTimeouts(ctx context.Context, script, pageLoad time.Duration) error {
	f.record("timeouts")
	return nil
}
func (f *fakeDriver) CDP(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p := params.(map[string]any)
	switch method {
	case "Target.createTarget":
		_, hidden := p["hidden"]
		f.record(fmt.Sprintf("createTarget hidden=%v background=%v", hidden, p["background"]))
		if hidden && !f.cdpHidden {
			return nil, &webdriver.Error{Code: "unknown error", Message: "Invalid parameters: hidden: unexpected property"}
		}
		if f.cdp == "error" {
			return nil, &webdriver.Error{Code: "unknown error", Message: "createTarget failed"}
		}
		return json.RawMessage(`{"targetId":"CDP1"}`), nil
	case "Target.getTargetInfo":
		f.record("getTargetInfo")
		if f.cdp == "change" {
			f.change()
			return json.RawMessage(`{"targetInfo":{"url":"https://us-east-1.console.aws.amazon.com/console/home"}}`), nil
		}
		return json.RawMessage(`{"targetInfo":{"url":"https://signin.aws.amazon.com/federation?SigninToken=x"}}`), nil
	case "Target.closeTarget":
		f.record("closeTarget " + fmt.Sprint(p["targetId"]))
		return json.RawMessage(`{"success":true}`), nil
	}
	return nil, fmt.Errorf("unexpected cdp %s", method)
}

func newSession(f *fakeDriver, t *testing.T) (*Session, *strings.Builder) {
	var notes strings.Builder
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &Session{
		Driver:   f,
		Hosts:    []string{"console.aws.amazon.com", "signin.aws.amazon.com"},
		Logf:     t.Logf,
		Notify:   func(format string, a ...any) { fmt.Fprintf(&notes, format+"\n", a...) },
		Sleep:    func(ctx context.Context, d time.Duration) error { now = now.Add(d); return ctx.Err() },
		Now:      func() time.Time { return now },
		Poll:     time.Second,
		Interval: 15 * time.Minute,
	}
	return s, &notes
}

const login = "https://signin.aws.amazon.com/federation?Action=login&SigninToken=NEW"

func TestRefreshViaFetch(t *testing.T) {
	f := newFake(webdriver.Chrome)
	s, notes := newSession(f, t)
	if err := s.Refresh(context.Background(), login); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, "; "); got != "fetch "+login {
		t.Errorf("calls = %s", got)
	}
	if !strings.Contains(notes.String(), "session refreshed (fetch)") {
		t.Errorf("notes = %s", notes.String())
	}
}

func TestRefreshFallsBackToDevTools(t *testing.T) {
	f := newFake(webdriver.Chrome)
	f.fetch, f.iframe, f.cdp = "error", "", "change"
	s, notes := newSession(f, t)
	if err := s.Refresh(context.Background(), login); err != nil {
		t.Fatal(err)
	}
	want := "fetch " + login + "; iframe " + login + "; createTarget hidden=true background=true; createTarget hidden=false background=true; getTargetInfo; closeTarget CDP1"
	if got := strings.Join(f.calls, "; "); got != want {
		t.Errorf("calls = %s", got)
	}
	if !strings.Contains(notes.String(), "(devtools-target)") {
		t.Errorf("notes = %s", notes.String())
	}
	// A Chrome that accepts hidden targets is only asked once.
	f = newFake(webdriver.Chrome)
	f.fetch, f.iframe, f.cdp, f.cdpHidden = "throw", "error", "change", true
	s, _ = newSession(f, t)
	if err := s.Refresh(context.Background(), login); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, "; "); strings.Contains(got, "hidden=false") || !strings.Contains(got, "hidden=true") {
		t.Errorf("calls = %s", got)
	}
}

func TestRefreshFallsBackToTab(t *testing.T) {
	f := newFake(webdriver.Firefox)
	f.fetch, f.iframe, f.tab = "", "", "change"
	s, notes := newSession(f, t)
	if err := s.Refresh(context.Background(), login); err != nil {
		t.Fatal(err)
	}
	want := "fetch " + login + "; iframe " + login + "; newtab T1; switch T1; navigate T1; close T1; switch W1"
	if got := strings.Join(f.calls, "; "); got != want {
		t.Errorf("calls = %s", got)
	}
	if !strings.Contains(notes.String(), "temporary tab") || f.current != "W1" {
		t.Errorf("notes = %s, current = %s", notes.String(), f.current)
	}
	// Nothing works: the error names every strategy.
	f = newFake(webdriver.Firefox)
	f.fetch, f.iframe, f.tab = "error", "", ""
	s, _ = newSession(f, t)
	err := s.Refresh(context.Background(), login)
	if err == nil || !strings.Contains(err.Error(), "fetch: TypeError") || !strings.Contains(err.Error(), "iframe: no console cookie changed") || !strings.Contains(err.Error(), "tab: no console cookie changed") {
		t.Errorf("err = %v", err)
	}
}

func TestRefreshPicksConsoleWindow(t *testing.T) {
	f := newFake(webdriver.Chrome)
	f.windows["W2"] = "https://example.com/"
	f.windows["W3"] = "https://signin.aws.amazon.com/oauth?x=1"
	f.current = "W2"
	s, _ := newSession(f, t)
	if err := s.Refresh(context.Background(), login); err != nil {
		t.Fatal(err)
	}
	// Handles are listed in order; W1 is the first console window.
	if got := strings.Join(f.calls, "; "); got != "handles; switch W1; fetch "+login {
		t.Errorf("calls = %s", got)
	}
	// The current tab was closed by the user: another console tab is used.
	f = newFake(webdriver.Chrome)
	f.windows["W2"] = "https://us-west-2.console.aws.amazon.com/s3/home"
	f.current = "gone"
	s, _ = newSession(f, t)
	if err := s.Refresh(context.Background(), login); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, "; "); got != "handles; switch W1; fetch "+login {
		t.Errorf("calls = %s", got)
	}
}

func TestRefreshWithoutConsoleTab(t *testing.T) {
	f := newFake(webdriver.Chrome)
	f.windows["W1"] = "https://example.com/"
	f.windows["W2"] = "https://example.org/"
	f.tab = "change"
	s, notes := newSession(f, t)
	if err := s.Refresh(context.Background(), login); err != nil {
		t.Fatal(err)
	}
	want := "handles; switch W2; switch W1; newtab T1; switch T1; navigate T1"
	if got := strings.Join(f.calls, "; "); got != want {
		t.Errorf("calls = %s", got)
	}
	if _, open := f.windows["T1"]; !open || !strings.Contains(notes.String(), "temporary tab") {
		t.Errorf("tab should stay open: %v, %s", f.windows, notes.String())
	}
}

func TestRunRefreshesUntilBrowserCloses(t *testing.T) {
	f := newFake(webdriver.Chrome)
	s, notes := newSession(f, t)
	var generated int
	s.Generate = func(ctx context.Context) (Login, error) {
		generated++
		if generated == 1 {
			return Login{}, errors.New("sts down")
		}
		if generated == 3 {
			f.gone = true
		}
		return Login{URL: fmt.Sprintf("%s%d", login, generated)}, nil
	}
	start := s.now()
	err := s.Run(context.Background(), Login{URL: login + "0", Expires: start.Add(10 * time.Minute)})
	if !errors.Is(err, ErrBrowserClosed) {
		t.Fatalf("err = %v", err)
	}
	// Expiry at +10m brings the first refresh forward to +8m; the failed
	// generate retries after 1m; the successful one (which waits 500ms
	// for cookies to settle) schedules +15m.
	if generated != 3 || s.now().Sub(start) != 8*time.Minute+time.Minute+500*time.Millisecond+15*time.Minute {
		t.Errorf("generated = %d, elapsed = %s", generated, s.now().Sub(start))
	}
	if f.calls[0] != "timeouts" || f.calls[1] != "navigate W1" || !strings.Contains(strings.Join(f.calls, ";"), "fetch "+login+"2") {
		t.Errorf("calls = %v", f.calls)
	}
	if !strings.Contains(notes.String(), "sts down; retrying in 1m") || !strings.Contains(notes.String(), "session refreshed (fetch)") {
		t.Errorf("notes = %s", notes.String())
	}

	// Cancelling the context (Ctrl-C) stops the loop.
	f = newFake(webdriver.Chrome)
	s, _ = newSession(f, t)
	s.Generate = func(ctx context.Context) (Login, error) { return Login{URL: login}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	s.Sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	if err := s.Run(ctx, Login{URL: login}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}

	// A browser that is gone from the start.
	f = newFake(webdriver.Chrome)
	f.gone = true
	s, _ = newSession(f, t)
	s.Generate = func(ctx context.Context) (Login, error) { return Login{URL: login}, nil }
	if err := s.Run(context.Background(), Login{URL: login}); !errors.Is(err, ErrBrowserClosed) {
		t.Errorf("err = %v", err)
	}
}

func TestHelpers(t *testing.T) {
	s := &Session{Hosts: []string{"console.aws.amazon.com", ".signin.aws.amazon.com"}}
	s.addHost("https://corp.awsapps.com/start/#/console")
	s.addHost("https://corp.awsapps.com/other")
	for u, want := range map[string]bool{
		"https://us-east-1.console.aws.amazon.com/ec2/home": true,
		"https://console.aws.amazon.com/":                   true,
		"https://signin.aws.amazon.com/oauth":               true,
		"https://corp.awsapps.com/start":                    true,
		"https://evilconsole.aws.amazon.com.example.com/":   false,
		"https://example.com/":                              false,
		"about:blank":                                       false,
		"":                                                  false,
	} {
		if s.isConsole(u) != want {
			t.Errorf("isConsole(%q) = %v", u, !want)
		}
	}
	if len(s.Hosts) != 3 {
		t.Errorf("hosts = %v", s.Hosts)
	}
	changed := changedCookies(map[string]string{"d|/|a": "1", "d|/|b": "2"}, map[string]string{"d|/|a": "1", "d|/|b": "3", "d|/|c": "4"})
	if strings.Join(changed, ",") != "b,c" {
		t.Errorf("changed = %v", changed)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Interval = 15 * time.Minute
	if got := s.nextRefresh(time.Time{}, now); got != now.Add(15*time.Minute) {
		t.Errorf("no expiry: %s", got)
	}
	if got := s.nextRefresh(now.Add(5*time.Minute), now); got != now.Add(3*time.Minute) {
		t.Errorf("early expiry: %s", got)
	}
	if got := s.nextRefresh(now.Add(30*time.Second), now); got != now.Add(time.Minute) {
		t.Errorf("imminent expiry: %s", got)
	}
}
