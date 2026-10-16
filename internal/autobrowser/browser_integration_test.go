package autobrowser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"awsume/internal/webdriver"
)

// fakeSignin imitates the federation login flow: the login URL sets the
// session cookies through a chain of redirects that ends on a console
// page. The console page can carry a CSP that blocks fetch and frames, to
// exercise the fallback strategies.
type fakeSignin struct {
	mu     sync.Mutex
	logins []string
	csp    string
}

func (f *fakeSignin) handler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/federation":
		token := r.URL.Query().Get("SigninToken")
		f.mu.Lock()
		f.logins = append(f.logins, token+" "+r.Header.Get("Sec-Fetch-Dest"))
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "aws-creds", Value: token, Path: "/", HttpOnly: true})
		http.Redirect(w, r, "/oauth?code="+token, http.StatusFound)
	case "/oauth":
		http.SetCookie(w, &http.Cookie{Name: "aws-userInfo", Value: r.URL.Query().Get("code"), Path: "/"})
		http.Redirect(w, r, "/console/home", http.StatusFound)
	case "/console/home":
		f.mu.Lock()
		csp := f.csp
		f.mu.Unlock()
		if csp != "" {
			w.Header().Set("Content-Security-Policy", csp)
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!doctype html><html><head><title>console</title></head><body><h1>console</h1></body></html>")
	default:
		http.NotFound(w, r)
	}
}

// TestRealBrowser runs the refresh strategies in a headless browser. Set
// AWSUME_BROWSER_TEST to chrome, firefox or all to enable it.
func TestRealBrowser(t *testing.T) {
	want := os.Getenv("AWSUME_BROWSER_TEST")
	if want == "" {
		t.Skip("set AWSUME_BROWSER_TEST=chrome|firefox|all")
	}
	for _, browser := range []string{webdriver.Chrome, webdriver.Firefox} {
		if want != "all" && want != browser {
			continue
		}
		t.Run(browser, func(t *testing.T) { realBrowser(t, browser) })
	}
}

func realBrowser(t *testing.T, browser string) {
	signin := &fakeSignin{}
	srv := httptest.NewServer(http.HandlerFunc(signin.handler))
	defer srv.Close()
	loginURL := func(token string) string {
		return srv.URL + "/federation?Action=login&Issuer=awsume&Destination=%2Fconsole%2Fhome&SigninToken=" + token
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var log strings.Builder
	logf := func(f string, a ...any) { fmt.Fprintf(&log, f+"\n", a...) }
	d, err := webdriver.Start(ctx, webdriver.Config{Browser: browser, Headless: true, Logf: logf, DriverOutput: &log})
	if err != nil {
		t.Fatalf("start %s: %v\n%s", browser, err, log.String())
	}
	defer d.Quit()
	t.Cleanup(func() { t.Log(log.String()) })

	s := &Session{Driver: d, Logf: logf, Notify: logf, Interval: time.Minute}
	s.addHost(srv.URL)
	if err := d.SetTimeouts(ctx, 60*time.Second, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.Navigate(ctx, loginURL("first")); err != nil {
		t.Fatal(err)
	}
	cookie := func(name string) string {
		cs, err := d.Cookies(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.Name == name {
				return c.Value
			}
		}
		return ""
	}
	if u, _ := d.CurrentURL(ctx); !strings.HasSuffix(u, "/console/home") || cookie("aws-creds") != "first" || cookie("aws-userInfo") != "first" {
		t.Fatalf("initial login: url=%s creds=%s", u, cookie("aws-creds"))
	}

	// 1. Plain console page: fetch does the job.
	if err := s.Refresh(ctx, loginURL("second")); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if cookie("aws-creds") != "second" || cookie("aws-userInfo") != "second" || !strings.Contains(log.String(), "refresh via fetch replaced cookies") {
		t.Fatalf("fetch refresh: creds=%s\n%s", cookie("aws-creds"), log.String())
	}
	if hs, _ := d.WindowHandles(ctx); len(hs) != 1 {
		t.Errorf("fetch refresh opened a tab: %v", hs)
	}

	// 2. A CSP that blocks fetch: the hidden iframe takes over.
	signin.mu.Lock()
	signin.csp = "connect-src 'none'"
	signin.mu.Unlock()
	if err := d.Navigate(ctx, srv.URL+"/console/home"); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if err := s.Refresh(ctx, loginURL("third")); err != nil {
		t.Fatalf("refresh under connect-src none: %v\n%s", err, log.String())
	}
	if cookie("aws-creds") != "third" || !strings.Contains(log.String(), "refresh via iframe replaced cookies") {
		t.Fatalf("iframe refresh: creds=%s\n%s", cookie("aws-creds"), log.String())
	}
	if hs, _ := d.WindowHandles(ctx); len(hs) != 1 {
		t.Errorf("iframe refresh opened a tab: %v", hs)
	}

	// 3. A CSP that blocks fetch and frames: Chrome uses a DevTools
	// target, Firefox a temporary tab. Either way the session is refreshed
	// and no extra tab remains.
	signin.mu.Lock()
	signin.csp = "connect-src 'none'; frame-src 'none'; child-src 'none'"
	signin.mu.Unlock()
	if err := d.Navigate(ctx, srv.URL+"/console/home"); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if err := s.Refresh(ctx, loginURL("fourth")); err != nil {
		t.Fatalf("refresh under strict csp: %v\n%s", err, log.String())
	}
	wantStrategy := "refresh via tab replaced cookies"
	if browser == webdriver.Chrome {
		wantStrategy = "refresh via devtools-target replaced cookies"
	}
	if cookie("aws-creds") != "fourth" || !strings.Contains(log.String(), wantStrategy) {
		t.Fatalf("strict csp refresh: creds=%s\n%s", cookie("aws-creds"), log.String())
	}
	if hs, _ := d.WindowHandles(ctx); len(hs) != 1 {
		t.Errorf("tab left open: %v", hs)
	}
	if u, _ := d.CurrentURL(ctx); !strings.HasSuffix(u, "/console/home") {
		t.Errorf("current window changed to %s", u)
	}

	// Closing the browser's last window is detected as the browser going
	// away.
	if err := d.CloseWindow(ctx); err != nil && !webdriver.IsGone(err) {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := s.alive(ctx)
		if err == ErrBrowserClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("browser close not detected: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	signin.mu.Lock()
	t.Logf("logins seen by the fake sign-in: %v", signin.logins)
	signin.mu.Unlock()
}
