package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"", "", false},
		{"127.0.0.1:8080", "http://127.0.0.1:8080", false},
		{"http://proxy:3128", "http://proxy:3128", false},
		{"HTTPS://proxy", "https://proxy:443", false},
		{"socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080", false},
		{"socks5h://host:9050", "socks5://host:9050", false},
		{"socks://host", "socks5://host:1080", false},
		{"proxy", "http://proxy:8080", false},
		{"ftp://proxy", "", true},
		{"http://", "", true},
		{"http://proxy:8080/path", "", true},
		{"http://:8080", "", true},
	}
	for _, c := range cases {
		p, err := Parse(c.in)
		if (err != nil) != c.err {
			t.Errorf("Parse(%q) err = %v", c.in, err)
			continue
		}
		if p.String() != c.want {
			t.Errorf("Parse(%q) = %q, want %q", c.in, p.String(), c.want)
		}
	}
	p, err := Parse("http://user:pass@proxy:8080")
	if err != nil || p.User == nil || p.User.Username() != "user" || p.URL().String() != "http://user:pass@proxy:8080" || p.String() != "http://proxy:8080" {
		t.Errorf("credentials: %+v %v", p, err)
	}
}

func TestTransportAndEnv(t *testing.T) {
	if tr := Transport(nil, false); tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("nil proxy must not disable verification")
	}
	p, _ := Parse("socks5://127.0.0.1:1080")
	tr := Transport(p, true)
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("insecure not applied")
	}
	u, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "sts.amazonaws.com"}})
	if err != nil || u.String() != "socks5://127.0.0.1:1080" {
		t.Errorf("proxy func = %v, %v", u, err)
	}
	env := strings.Join(Env(p), " ")
	if !strings.Contains(env, "HTTPS_PROXY=socks5://127.0.0.1:1080") || !strings.Contains(env, "NO_PROXY=") {
		t.Errorf("env = %s", env)
	}
	if Env(nil) != nil {
		t.Error("nil proxy must yield no env")
	}
}

// TestClientUsesProxy sends a request through a local HTTP proxy and checks
// it arrives there in absolute-URI form.
func TestClientUsesProxy(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.RequestURI
		w.WriteHeader(204)
	}))
	defer srv.Close()
	p, _ := Parse(strings.TrimPrefix(srv.URL, "http://"))
	c := Client(p, false, 5*time.Second)
	resp, err := c.Get("http://example.invalid/path")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "http://example.invalid/path" {
		t.Errorf("proxy saw %q", got)
	}
}
