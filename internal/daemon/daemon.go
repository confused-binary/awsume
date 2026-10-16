// Package daemon is the plumbing for the long-running awsume process that
// owns -auto-browser sessions: where its socket lives, the messages that
// cross it, how to start it detached from a terminal and how to install it
// as a per-user login service.
//
// The daemon listens on a Unix domain socket that only the owning user can
// reach. One request is sent per connection as a JSON document, and one
// JSON response comes back. Requests carry the client's command line and
// environment, so the daemon resolves credentials and starts the browser
// exactly as the client would have.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// SocketEnv is the environment variable that overrides the socket path.
const SocketEnv = "AWSUME_DAEMON_SOCKET"

// Operations a request can ask for.
const (
	OpSession = "session"
	OpStatus  = "status"
	OpStop    = "stop"
)

// Login is a sign-in URL the client already generated, so the daemon can
// open the browser without resolving the credentials a second time.
type Login struct {
	URL     string    `json:"url"`
	Expires time.Time `json:"expires,omitempty"`
	// Hosts are the sign-in and console host names the session involves.
	Hosts []string `json:"hosts,omitempty"`
}

// Request is one message from a client to the daemon.
type Request struct {
	Op string `json:"op"`
	// Args is the client's full command line (without the program name),
	// for OpSession.
	Args []string `json:"args,omitempty"`
	// Env is the client's environment, for OpSession.
	Env map[string]string `json:"env,omitempty"`
	// Dir is the client's working directory, for relative paths in Args.
	Dir string `json:"dir,omitempty"`
	// Profile is the display name of the profile, for OpSession.
	Profile string `json:"profile,omitempty"`
	// Login is the first sign-in URL, for OpSession.
	Login *Login `json:"login,omitempty"`
}

// SessionInfo describes one browser session the daemon is keeping alive.
type SessionInfo struct {
	ID          int       `json:"id"`
	Profile     string    `json:"profile"`
	Browser     string    `json:"browser"`
	Started     time.Time `json:"started"`
	Refreshes   int       `json:"refreshes"`
	LastRefresh time.Time `json:"last_refresh,omitempty"`
	// Last is the most recent message the session produced.
	Last string `json:"last,omitempty"`
}

// Response is the daemon's answer.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Message is a line meant for the user.
	Message string `json:"message,omitempty"`
	// PID is the daemon's process id.
	PID int `json:"pid,omitempty"`
	// Version is the daemon's version.
	Version string `json:"version,omitempty"`
	// Session is the session created by OpSession.
	Session *SessionInfo `json:"session,omitempty"`
	// Sessions lists the running sessions, for OpStatus.
	Sessions []SessionInfo `json:"sessions,omitempty"`
}

// ErrNotRunning is returned by Call when nothing listens on the socket.
var ErrNotRunning = errors.New("the awsume daemon is not running")

// ErrAlreadyRunning is returned by Listen when a daemon already answers on
// the socket.
var ErrAlreadyRunning = errors.New("an awsume daemon is already running")

// CacheDir returns the directory awsume keeps its browser profiles, socket
// and logs under: $XDG_CACHE_HOME, %LOCALAPPDATA% or ~/.cache, followed by
// "awsume". Empty when none can be determined.
func CacheDir(env func(string) string) string {
	base := env("XDG_CACHE_HOME")
	if base == "" {
		base = env("LOCALAPPDATA")
	}
	if base == "" {
		if home := env("HOME"); home != "" {
			base = filepath.Join(home, ".cache")
		}
	}
	if base == "" {
		if d, err := os.UserCacheDir(); err == nil {
			base = d
		}
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "awsume")
}

// SocketPath is where the daemon listens: $AWSUME_DAEMON_SOCKET, otherwise
// daemon.sock under $XDG_RUNTIME_DIR/awsume or under CacheDir. Empty when
// no location can be determined.
func SocketPath(env func(string) string) string {
	if p := env(SocketEnv); p != "" {
		return p
	}
	if d := env("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "awsume", "daemon.sock")
	}
	if d := CacheDir(env); d != "" {
		return filepath.Join(d, "daemon.sock")
	}
	return ""
}

// LogPath is the log file a detached daemon writes to.
func LogPath(env func(string) string) string {
	if d := CacheDir(env); d != "" {
		return filepath.Join(d, "daemon.log")
	}
	return ""
}

// Dial connects to the daemon's socket.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	if path == "" {
		return nil, errors.New("no daemon socket path")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		if isNotRunning(err) {
			return nil, ErrNotRunning
		}
		return nil, err
	}
	return conn, nil
}

// isNotRunning reports whether a dial error means no daemon is listening.
func isNotRunning(err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, m := range []string{"connection refused", "no such file", "cannot find the file", "file not found", "econnrefused", "not found"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// Running reports whether a daemon answers on path.
func Running(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, path)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Call sends req to the daemon and returns its response. A response with
// OK false is returned as a non-nil error as well.
func Call(ctx context.Context, path string, req Request) (Response, error) {
	conn, err := Dial(ctx, path)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("send to daemon: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("read daemon response: %w", err)
	}
	if !resp.OK {
		if resp.Error == "" {
			resp.Error = "daemon refused the request"
		}
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// Listen creates the daemon's socket, replacing a stale socket file that
// nobody answers on. The socket and its directory are only accessible to
// the current user.
func Listen(path string) (net.Listener, error) {
	if path == "" {
		return nil, errors.New("no daemon socket path")
	}
	if Running(path) {
		return nil, ErrAlreadyRunning
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o600)
	}
	return ln, nil
}

// Handler answers one request.
type Handler func(ctx context.Context, req Request) Response

// Serve accepts connections until ctx is done, answering each with h. Each
// connection is handled on its own goroutine; Serve returns once the
// listener is closed and every handler has finished.
func Serve(ctx context.Context, ln net.Listener, h Handler) error {
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		ln.Close()
	}()
	defer func() {
		close(done)
		wg.Wait()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			var req Request
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				_ = json.NewEncoder(conn).Encode(Response{Error: "bad request: " + err.Error()})
				return
			}
			_ = conn.SetReadDeadline(time.Time{})
			resp := h(ctx, req)
			_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			_ = json.NewEncoder(conn).Encode(resp)
		}()
	}
}

// Environ turns an environment list into a map; later entries win.
func Environ(list []string) map[string]string {
	m := make(map[string]string, len(list))
	for _, kv := range list {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			m[k] = v
		}
	}
	return m
}

// EnvList turns an environment map back into a sorted KEY=value list.
func EnvList(m map[string]string) []string {
	list := make([]string, 0, len(m))
	for k, v := range m {
		list = append(list, k+"="+v)
	}
	sort.Strings(list)
	return list
}

// LookPath returns an executable lookup that searches the directories of
// pathEnv (a PATH value) instead of this process's PATH. On Windows
// pathExt (a PATHEXT value) supplies the extensions tried.
func LookPath(pathEnv, pathExt string) func(string) (string, error) {
	exts := []string{""}
	if runtime.GOOS == "windows" {
		if pathExt == "" {
			pathExt = ".COM;.EXE;.BAT;.CMD"
		}
		for _, e := range strings.Split(strings.ToLower(pathExt), ";") {
			if e != "" {
				exts = append(exts, e)
			}
		}
	}
	dirs := filepath.SplitList(pathEnv)
	return func(file string) (string, error) {
		if strings.ContainsRune(file, filepath.Separator) || strings.ContainsRune(file, '/') {
			if isExecutable(file) {
				return file, nil
			}
			return "", fmt.Errorf("%s: executable file not found", file)
		}
		for _, dir := range dirs {
			if dir == "" {
				dir = "."
			}
			for _, ext := range exts {
				cand := filepath.Join(dir, file+ext)
				if isExecutable(cand) {
					return cand, nil
				}
			}
		}
		return "", fmt.Errorf("%s: executable file not found in PATH", file)
	}
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}
