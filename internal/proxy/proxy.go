// Package proxy parses the -proxy flag and turns it into an HTTP client,
// subprocess environment and browser settings so that every connection the
// tool makes, directly or indirectly, goes through the same proxy.
package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Proxy is a parsed proxy address.
type Proxy struct {
	// Scheme is http, https or socks5.
	Scheme string
	Host   string
	Port   string
	// User is set when the address carried credentials.
	User *url.Userinfo
}

// Parse accepts http://host:port, https://host:port, socks5://host:port
// (socks5h and socks are treated as socks5) and a bare host:port, which
// means an HTTP proxy. An empty string yields nil.
func Parse(s string) (*Proxy, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy %q: %w", s, err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https":
	case "socks5", "socks5h", "socks":
		scheme = "socks5"
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (want http, https or socks5)", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("invalid proxy %q: missing host", s)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" {
		return nil, fmt.Errorf("invalid proxy %q: only scheme://host:port is accepted", s)
	}
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "8080"
		case "https":
			port = "443"
		case "socks5":
			port = "1080"
		}
	}
	return &Proxy{Scheme: scheme, Host: u.Hostname(), Port: port, User: u.User}, nil
}

// HostPort returns host:port.
func (p *Proxy) HostPort() string {
	return net.JoinHostPort(p.Host, p.Port)
}

// URL returns the proxy as a URL, including credentials.
func (p *Proxy) URL() *url.URL {
	return &url.URL{Scheme: p.Scheme, Host: p.HostPort(), User: p.User}
}

// String returns the proxy as scheme://host:port without credentials.
func (p *Proxy) String() string {
	if p == nil {
		return ""
	}
	return p.Scheme + "://" + p.HostPort()
}

// Transport returns an HTTP transport that sends everything through p
// (nil keeps the environment proxy settings). insecure disables TLS
// certificate verification, which an intercepting proxy such as Burp needs
// unless its CA is trusted by the system.
func Transport(p *Proxy, insecure bool) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if p != nil {
		t.Proxy = http.ProxyURL(p.URL())
	}
	if insecure {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // requested with -insecure
	}
	return t
}

// Client returns an HTTP client built on Transport.
func Client(p *Proxy, insecure bool, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport(p, insecure)}
}

// Env returns the environment variables that make the AWS CLI, Python and
// most other tools use p, for subprocesses such as credential_process and
// "aws configure export-credentials". nil yields nil.
func Env(p *Proxy) []string {
	if p == nil {
		return nil
	}
	u := p.URL().String()
	return []string{
		"HTTP_PROXY=" + u, "HTTPS_PROXY=" + u, "ALL_PROXY=" + u,
		"http_proxy=" + u, "https_proxy=" + u, "all_proxy=" + u,
		"NO_PROXY=", "no_proxy=",
	}
}

// ErrCredentialsUnsupported is returned by browser settings when the proxy
// address carries a user name, which browsers cannot be given on the
// command line.
var ErrCredentialsUnsupported = errors.New("proxy credentials cannot be passed to a browser; use an unauthenticated proxy or configure the browser's own proxy settings")
