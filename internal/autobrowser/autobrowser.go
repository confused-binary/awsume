// Package autobrowser keeps an AWS console session alive in a browser that
// awsume launched. It periodically generates a fresh sign-in URL and makes
// the browser load it in the background, so the console cookies are
// replaced without opening a new tab or window.
//
// The login URL is applied inside an existing console tab, trying the
// least visible mechanism first:
//
//  1. a fetch() of the login URL with credentials, which follows the
//     sign-in redirects and stores the cookies they set;
//  2. a hidden iframe, which is a real navigation and therefore also runs
//     any HTML or script steps of the sign-in flow;
//  3. with Chrome, a DevTools target that loads the URL in the background
//     without taking focus (hidden entirely on Chromes that support hidden
//     targets) and is closed once it reaches the console;
//  4. a new tab that is closed again as soon as the login has completed.
//
// After every step the console cookies are compared with the ones seen
// before; the first step that changes them wins.
package autobrowser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"awsume/internal/webdriver"
)

// Driver is the subset of the WebDriver client the session uses.
type Driver interface {
	Browser() string
	Navigate(ctx context.Context, url string) error
	CurrentURL(ctx context.Context) (string, error)
	WindowHandles(ctx context.Context) ([]string, error)
	CurrentWindow(ctx context.Context) (string, error)
	SwitchWindow(ctx context.Context, handle string) error
	NewTab(ctx context.Context) (string, error)
	CloseWindow(ctx context.Context) error
	ExecuteAsyncScript(ctx context.Context, script string, args ...any) (json.RawMessage, error)
	Cookies(ctx context.Context) ([]webdriver.Cookie, error)
	SetTimeouts(ctx context.Context, script, pageLoad time.Duration) error
	CDP(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// Login is a freshly generated sign-in URL.
type Login struct {
	URL string
	// Expires is when the console session created by URL ends; zero when
	// unknown.
	Expires time.Time
}

// Session drives one browser.
type Session struct {
	Driver Driver
	// Generate produces a new sign-in URL.
	Generate func(ctx context.Context) (Login, error)
	// Interval is how often the session is refreshed (default 15m).
	Interval time.Duration
	// Hosts lists the host names (or parent domains) of console and
	// sign-in pages; a tab on one of them is used to apply the refresh.
	Hosts []string
	// Logf receives verbose diagnostics; nil discards them.
	Logf func(string, ...any)
	// Notify receives messages meant for the user; nil discards them.
	Notify func(string, ...any)
	// Refreshed is called after every successful refresh; nil ignores it.
	Refreshed func(Login)
	// Now supplies the current time; nil uses time.Now.
	Now func() time.Time
	// Poll is how often the browser is checked for being open (default 2s).
	Poll time.Duration
	// CallTimeout bounds each generate and each browser command (default 2m).
	CallTimeout time.Duration
	// Sleep waits for d or ctx; nil uses time.After.
	Sleep func(ctx context.Context, d time.Duration) error
}

// ErrBrowserClosed is returned by Run when the user closed the browser.
var ErrBrowserClosed = errors.New("browser closed")

func (s *Session) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Session) notify(format string, args ...any) {
	if s.Notify != nil {
		s.Notify(format, args...)
	}
}

func (s *Session) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Session) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return 15 * time.Minute
}

func (s *Session) poll() time.Duration {
	if s.Poll > 0 {
		return s.Poll
	}
	return 2 * time.Second
}

func (s *Session) callTimeout() time.Duration {
	if s.CallTimeout > 0 {
		return s.CallTimeout
	}
	return 2 * time.Minute
}

func (s *Session) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// Run opens first in the browser and refreshes the session until the
// browser is closed (ErrBrowserClosed) or ctx is cancelled (ctx.Err()).
func (s *Session) Run(ctx context.Context, first Login) error {
	if s.Driver == nil || s.Generate == nil {
		return errors.New("autobrowser: driver and generate are required")
	}
	s.addHost(first.URL)
	if err := s.call(ctx, func(c context.Context) error {
		return s.Driver.SetTimeouts(c, 60*time.Second, s.callTimeout())
	}); err != nil {
		s.logf("set browser timeouts: %v", err)
	}
	if err := s.call(ctx, func(c context.Context) error { return s.Driver.Navigate(c, first.URL) }); err != nil {
		if webdriver.IsGone(err) {
			return ErrBrowserClosed
		}
		return fmt.Errorf("open sign-in url: %w", err)
	}
	next := s.nextRefresh(first.Expires, s.now())
	s.logf("next refresh at %s", next.Format(time.RFC3339))
	for {
		if err := s.wait(ctx, next); err != nil {
			return err
		}
		login, err := s.generate(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.notify("refresh: could not generate a new sign-in url: %v; retrying in 1m", err)
			next = s.now().Add(time.Minute)
			continue
		}
		err = s.Refresh(ctx, login.URL)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case webdriver.IsGone(err):
			return ErrBrowserClosed
		case err != nil:
			s.notify("refresh: %v; retrying in 1m", err)
			next = s.now().Add(time.Minute)
			continue
		}
		if s.Refreshed != nil {
			s.Refreshed(login)
		}
		next = s.nextRefresh(login.Expires, s.now())
		s.logf("next refresh at %s", next.Format(time.RFC3339))
	}
}

// nextRefresh picks the next refresh time: the interval, brought forward
// when the console session would expire before then.
func (s *Session) nextRefresh(expires, now time.Time) time.Time {
	next := now.Add(s.interval())
	if !expires.IsZero() {
		early := expires.Add(-2 * time.Minute)
		if early.Before(next) {
			next = early
		}
	}
	if min := now.Add(time.Minute); next.Before(min) {
		next = min
	}
	return next
}

// wait sleeps until deadline while checking that the browser is still
// open.
func (s *Session) wait(ctx context.Context, deadline time.Time) error {
	for {
		remaining := deadline.Sub(s.now())
		if remaining <= 0 {
			return nil
		}
		d := s.poll()
		if remaining < d {
			d = remaining
		}
		if err := s.sleep(ctx, d); err != nil {
			return err
		}
		if err := s.alive(ctx); err != nil {
			return err
		}
	}
}

// alive returns ErrBrowserClosed when no window is left.
func (s *Session) alive(ctx context.Context) error {
	var handles []string
	err := s.call(ctx, func(c context.Context) error {
		var err error
		handles, err = s.Driver.WindowHandles(c)
		return err
	})
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case webdriver.IsGone(err) || (err == nil && len(handles) == 0):
		return ErrBrowserClosed
	case err != nil:
		s.logf("browser check failed: %v", err)
	}
	return nil
}

func (s *Session) generate(ctx context.Context) (Login, error) {
	c, cancel := context.WithTimeout(ctx, s.callTimeout())
	defer cancel()
	s.logf("generating a new sign-in url")
	login, err := s.Generate(c)
	if err == nil {
		s.addHost(login.URL)
	}
	return login, err
}

func (s *Session) call(ctx context.Context, fn func(context.Context) error) error {
	c, cancel := context.WithTimeout(ctx, s.callTimeout())
	defer cancel()
	return fn(c)
}

// addHost records the host of a URL as a console host.
func (s *Session) addHost(raw string) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return
	}
	h := strings.ToLower(u.Hostname())
	for _, x := range s.Hosts {
		if x == h {
			return
		}
	}
	s.Hosts = append(s.Hosts, h)
}

// isConsole reports whether raw is a page on one of the console hosts.
func (s *Session) isConsole(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	for _, h := range s.Hosts {
		h = strings.ToLower(strings.TrimPrefix(h, "."))
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// Strategy names, as reported in diagnostics.
const (
	StrategyFetch  = "fetch"
	StrategyIframe = "iframe"
	StrategyCDP    = "devtools-target"
	StrategyTab    = "tab"
)

// Refresh applies loginURL to the browser session without disturbing what
// the user is looking at.
func (s *Session) Refresh(ctx context.Context, loginURL string) error {
	ctx, cancel := context.WithTimeout(ctx, s.callTimeout())
	defer cancel()
	d := s.Driver

	target, err := s.consoleWindow(ctx)
	if err != nil {
		return err
	}
	if target == "" {
		// No console page is open anywhere: the login has to happen in a
		// tab of its own.
		s.logf("no console tab is open; loading the sign-in url in a temporary tab")
		if err := s.viaTab(ctx, loginURL, ""); err != nil {
			return err
		}
		s.notify("session refreshed (temporary tab)")
		return nil
	}

	before, err := s.cookies(ctx)
	if err != nil {
		return err
	}
	type strategy struct {
		name string
		run  func() error
	}
	strategies := []strategy{
		{StrategyFetch, func() error { return s.viaScript(ctx, fetchScript, loginURL) }},
		{StrategyIframe, func() error { return s.viaScript(ctx, iframeScript, loginURL) }},
	}
	if d.Browser() == webdriver.Chrome {
		strategies = append(strategies, strategy{StrategyCDP, func() error { return s.viaCDP(ctx, loginURL) }})
	}
	strategies = append(strategies, strategy{StrategyTab, func() error { return s.viaTab(ctx, loginURL, target) }})

	var failures []string
	for _, st := range strategies {
		if err := st.run(); err != nil {
			if webdriver.IsGone(err) || ctx.Err() != nil {
				return err
			}
			s.logf("refresh via %s failed: %v", st.name, err)
			failures = append(failures, st.name+": "+err.Error())
			continue
		}
		after, err := s.cookies(ctx)
		if err != nil {
			return err
		}
		if changed := changedCookies(before, after); len(changed) > 0 {
			s.logf("refresh via %s replaced cookies %s", st.name, strings.Join(changed, ", "))
			if st.name == StrategyTab {
				s.notify("session refreshed, but only by opening a temporary tab; run with -v to see why the quieter methods failed")
			} else {
				s.notify("session refreshed (%s)", st.name)
			}
			return nil
		}
		s.logf("refresh via %s did not change any console cookie", st.name)
		failures = append(failures, st.name+": no console cookie changed")
	}
	return fmt.Errorf("could not refresh the console session (%s)", strings.Join(failures, "; "))
}

// consoleWindow finds a window showing a console page and makes it
// current, preferring the one that is already current. It returns "" when
// there is none.
func (s *Session) consoleWindow(ctx context.Context) (string, error) {
	d := s.Driver
	current, err := d.CurrentWindow(ctx)
	if err == nil {
		u, err := d.CurrentURL(ctx)
		if err == nil && s.isConsole(u) {
			return current, nil
		}
	} else if webdriver.IsGone(err) {
		// The current tab was closed; another one may still be open.
		var we *webdriver.Error
		if !errors.As(err, &we) || we.Code != "no such window" {
			return "", err
		}
	}
	handles, err := d.WindowHandles(ctx)
	if err != nil {
		return "", err
	}
	for _, h := range handles {
		if h == current {
			continue
		}
		if err := d.SwitchWindow(ctx, h); err != nil {
			s.logf("switch to window %s: %v", h, err)
			continue
		}
		u, err := d.CurrentURL(ctx)
		if err == nil && s.isConsole(u) {
			s.logf("using console tab %s (%s)", h, u)
			return h, nil
		}
	}
	// Leave the user's window current again when it still exists.
	if current != "" {
		for _, h := range handles {
			if h == current {
				_ = d.SwitchWindow(ctx, current)
				break
			}
		}
	}
	return "", nil
}

func (s *Session) cookies(ctx context.Context) (map[string]string, error) {
	cs, err := s.Driver.Cookies(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(cs))
	for _, c := range cs {
		m[c.Domain+"|"+c.Path+"|"+c.Name] = c.Value
	}
	return m, nil
}

// changedCookies lists the cookies whose value differs between before and
// after, including new ones.
func changedCookies(before, after map[string]string) []string {
	var names []string
	for k, v := range after {
		if old, ok := before[k]; !ok || old != v {
			names = append(names, k[strings.LastIndex(k, "|")+1:])
		}
	}
	sort.Strings(names)
	return names
}

// fetchScript loads the login URL from inside the console page. The
// redirects of the sign-in flow run in the browser's network stack and set
// their cookies; the response itself is not needed.
const fetchScript = `var url = arguments[0], done = arguments[arguments.length - 1];
try {
  fetch(url, {mode: 'no-cors', credentials: 'include', redirect: 'follow', cache: 'no-store', referrerPolicy: 'no-referrer'})
    .then(function (r) { done('ok ' + r.type); }, function (e) { done('error ' + e); });
} catch (e) { done('error ' + e); }`

// iframeScript loads the login URL in an invisible frame, which is a full
// navigation. The frame is removed once it has loaded.
const iframeScript = `var url = arguments[0], done = arguments[arguments.length - 1];
try {
  var f = document.createElement('iframe');
  f.setAttribute('aria-hidden', 'true');
  f.setAttribute('tabindex', '-1');
  f.style.cssText = 'position:absolute;left:-9999px;top:-9999px;width:1px;height:1px;border:0;visibility:hidden';
  var finished = false;
  function finish(state) {
    if (finished) { return; }
    finished = true;
    setTimeout(function () { try { f.remove(); } catch (e) {} done(state); }, 1500);
  }
  f.addEventListener('load', function () { finish('ok load'); });
  f.addEventListener('error', function () { finish('error load failed'); });
  setTimeout(function () { finish('error timeout'); }, 30000);
  f.src = url;
  (document.body || document.documentElement).appendChild(f);
} catch (e) { done('error ' + e); }`

// viaScript runs an async script that reports "ok ..." or "error ...".
func (s *Session) viaScript(ctx context.Context, script, loginURL string) error {
	raw, err := s.Driver.ExecuteAsyncScript(ctx, script, loginURL)
	if err != nil {
		return err
	}
	var result string
	if json.Unmarshal(raw, &result) != nil {
		result = string(raw)
	}
	if !strings.HasPrefix(result, "ok") {
		return errors.New(strings.TrimSpace(strings.TrimPrefix(result, "error")))
	}
	s.logf("script finished: %s", result)
	// Give the browser a moment to commit cookies from the last redirect.
	return s.sleep(ctx, 500*time.Millisecond)
}

// viaCDP opens the login URL in a DevTools target that is not shown as a
// tab (hidden targets need a recent Chrome; otherwise the target is a
// background tab) and closes it once the sign-in flow has landed on the
// console.
func (s *Session) viaCDP(ctx context.Context, loginURL string) error {
	d := s.Driver
	params := map[string]any{"url": loginURL, "background": true, "newWindow": false, "hidden": true}
	raw, err := d.CDP(ctx, "Target.createTarget", params)
	if err != nil {
		var we *webdriver.Error
		if errors.As(err, &we) && strings.Contains(strings.ToLower(we.Message), "hidden") {
			s.logf("this chrome cannot create hidden targets; using a background tab instead")
			delete(params, "hidden")
			raw, err = d.CDP(ctx, "Target.createTarget", params)
		}
		if err != nil {
			return err
		}
	}
	var created struct {
		TargetID string `json:"targetId"`
	}
	if json.Unmarshal(raw, &created) != nil || created.TargetID == "" {
		return fmt.Errorf("unexpected Target.createTarget result %s", raw)
	}
	s.logf("created devtools target %s (hidden=%v)", created.TargetID, params["hidden"] != nil)
	defer func() {
		_, _ = d.CDP(context.Background(), "Target.closeTarget", map[string]any{"targetId": created.TargetID})
	}()
	deadline := s.now().Add(30 * time.Second)
	for {
		if err := s.sleep(ctx, time.Second); err != nil {
			return err
		}
		raw, err := d.CDP(ctx, "Target.getTargetInfo", map[string]any{"targetId": created.TargetID})
		if err != nil {
			return err
		}
		var info struct {
			TargetInfo struct {
				URL string `json:"url"`
			} `json:"targetInfo"`
		}
		_ = json.Unmarshal(raw, &info)
		u := info.TargetInfo.URL
		if u != "" && u != loginURL && s.isConsole(u) && !strings.Contains(u, "SigninToken=") {
			s.logf("devtools target landed on %s", u)
			return s.sleep(ctx, time.Second)
		}
		if s.now().After(deadline) {
			return errors.New("sign-in flow did not reach the console within 30s")
		}
	}
}

// viaTab opens the login URL in a new tab, waits for the load and closes
// the tab again, returning to the window named by back (when not empty).
func (s *Session) viaTab(ctx context.Context, loginURL, back string) error {
	d := s.Driver
	h, err := d.NewTab(ctx)
	if err != nil {
		return err
	}
	if err := d.SwitchWindow(ctx, h); err != nil {
		return err
	}
	navErr := d.Navigate(ctx, loginURL)
	if navErr != nil && webdriver.IsGone(navErr) {
		return navErr
	}
	if back == "" {
		// Nothing to go back to: leave the console open for the user.
		return navErr
	}
	if err := d.CloseWindow(ctx); err != nil && webdriver.IsGone(err) {
		return err
	}
	if err := d.SwitchWindow(ctx, back); err != nil {
		return err
	}
	return navErr
}
