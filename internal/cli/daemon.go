package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"awsume/internal/autobrowser"
	"awsume/internal/daemon"
)

// daemonActions are the values -daemon accepts.
var daemonActions = []string{"run", "start", "stop", "status", "install", "uninstall"}

// daemonCommand dispatches -daemon <action>.
func daemonCommand(o Options, f *flags) int {
	switch strings.ToLower(f.daemon) {
	case "run":
		return runDaemon(o, f)
	case "start":
		return startDaemon(o, f)
	case "stop":
		return stopDaemon(o, f)
	case "status":
		return daemonStatus(o, f)
	case "install":
		return installDaemon(o, f, true)
	case "uninstall":
		return installDaemon(o, f, false)
	}
	fmt.Fprintf(o.Stderr, "awsume: -daemon: unknown action %q (want one of %s)\n", f.daemon, strings.Join(daemonActions, ", "))
	return ExitUsage
}

// runCommand runs an external command, showing its output on failure.
func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(out.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// handOff gives the browser session for name to the daemon listening on
// sock: the URL is generated (and printed, copied or opened) here, where
// prompts can still be answered, and the daemon opens the browser and
// refreshes it from then on.
func (a *app) handOff(name, sock string) int {
	o, f := a.o, a.f
	rec, code := a.firstLogin(name)
	if code != ExitOK {
		return code
	}
	dir, _ := os.Getwd()
	req := daemon.Request{
		Op:      daemon.OpSession,
		Args:    o.Args,
		Env:     daemon.Environ(o.Environ()),
		Dir:     dir,
		Profile: rec.Profile,
		Login:   &daemon.Login{URL: rec.URL, Expires: rec.expires, Hosts: rec.hosts},
	}
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	resp, err := daemon.Call(ctx, sock, req)
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: daemon: %v\n", err)
		return ExitError
	}
	id := 0
	if resp.Session != nil {
		id = resp.Session.ID
	}
	fmt.Fprintf(o.Stderr, "awsume: %s is open; the awsume daemon (pid %d) keeps session %d refreshed every %s. \"awsume -daemon status\" lists sessions, \"awsume -daemon stop\" ends them.\n", a.browser, resp.PID, id, f.refresh)
	return ExitOK
}

// absolutize resolves the file flags against dir, the directory the client
// ran in, so the daemon reads the same files.
func (f *flags) absolutize(dir string) {
	if dir == "" {
		return
	}
	file := func(p *string) {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	// Executables may be bare names looked up on PATH; only paths with a
	// directory part are relative to dir.
	executable := func(p *string) {
		if strings.ContainsAny(*p, `/\`) {
			file(p)
		}
	}
	file(&f.configFile)
	file(&f.credentialsFile)
	executable(&f.chromedriver)
	executable(&f.geckodriver)
	executable(&f.browserBinary)
}

// daemonServer owns the browser sessions of a running daemon.
type daemonServer struct {
	o       Options
	verbose bool
	socket  string
	stop    context.CancelFunc

	mu       sync.Mutex
	nextID   int
	sessions map[int]*daemonSession
	wg       sync.WaitGroup
}

// daemonSession is the bookkeeping for one browser.
type daemonSession struct {
	mu   sync.Mutex
	info daemon.SessionInfo
}

func (s *daemonSession) snapshot() daemon.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// runDaemon serves requests on the socket until interrupted or told to
// stop, closing every browser on the way out.
func runDaemon(o Options, f *flags) int {
	sock := daemon.SocketPath(o.Env)
	ln, err := daemon.Listen(sock)
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: daemon: %v\n", err)
		return ExitError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := &daemonServer{o: o, verbose: f.verbose, socket: sock, stop: cancel, sessions: map[int]*daemonSession{}}
	srv.logf("listening on %s (pid %d, awsume %s)", sock, os.Getpid(), versionString(o.Version))
	err = daemon.Serve(ctx, ln, srv.handle)
	srv.logf("shutting down; closing %d browser session(s)", srv.count())
	cancel()
	srv.wg.Wait()
	_ = os.Remove(sock)
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: daemon: %v\n", err)
		return ExitError
	}
	srv.logf("stopped")
	return ExitOK
}

func (s *daemonServer) logf(format string, args ...any) {
	fmt.Fprintf(s.o.Stderr, "awsume: %s daemon: %s\n", s.o.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func (s *daemonServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *daemonServer) list() []daemon.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []daemon.SessionInfo
	for _, sess := range s.sessions {
		list = append(list, sess.snapshot())
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

// handle answers one request.
func (s *daemonServer) handle(ctx context.Context, req daemon.Request) daemon.Response {
	base := daemon.Response{OK: true, PID: os.Getpid(), Version: versionString(s.o.Version)}
	switch req.Op {
	case daemon.OpStatus:
		base.Sessions = s.list()
		return base
	case daemon.OpStop:
		n := s.count()
		s.logf("stop requested")
		base.Message = fmt.Sprintf("daemon stopping; closing %d browser session(s)", n)
		s.stop()
		return base
	case daemon.OpSession:
		info, err := s.startSession(ctx, req)
		if err != nil {
			s.logf("session for %s refused: %v", req.Profile, err)
			return daemon.Response{Error: err.Error(), PID: base.PID, Version: base.Version}
		}
		base.Session = &info
		return base
	}
	return daemon.Response{Error: fmt.Sprintf("unknown operation %q", req.Op), PID: base.PID, Version: base.Version}
}

// startSession builds the client's command line into an app of this
// process, opens the browser and leaves a goroutine refreshing it.
func (s *daemonServer) startSession(ctx context.Context, req daemon.Request) (daemon.SessionInfo, error) {
	if req.Login == nil || req.Login.URL == "" {
		return daemon.SessionInfo{}, errors.New("session request without a sign-in url")
	}
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()

	env := req.Env
	if env == nil {
		env = map[string]string{}
	}
	o := s.o
	o.Args = req.Args
	o.Env = func(k string) string { return env[k] }
	o.Environ = func() []string { return daemon.EnvList(env) }
	o.environ = daemon.EnvList(env)
	o.Stdin = nil
	o.StdinIsTerminal = false
	o.Stdout = io.Discard
	o.Stderr = &prefixWriter{w: s.o.Stderr, now: s.o.Now, tag: fmt.Sprintf("session %d (%s)", id, req.Profile)}
	if o.defaultLookPath {
		o.LookPath = daemon.LookPath(env["PATH"], env["PATHEXT"])
	}
	var f flags
	fs := newFlagSet(&f, io.Discard)
	positional, err := parseArgs(fs, o.Args)
	if err != nil {
		return daemon.SessionInfo{}, fmt.Errorf("parse command line: %w", err)
	}
	if f.autoBrowser == "" || f.daemon != "" {
		return daemon.SessionInfo{}, errors.New("not an -auto-browser command line")
	}
	f.absolutize(req.Dir)
	app, err := newApp(o, &f)
	if err != nil {
		return daemon.SessionInfo{}, err
	}
	profiles, err := app.profiles(positional)
	if err != nil {
		return daemon.SessionInfo{}, err
	}
	if len(profiles) != 1 {
		return daemon.SessionInfo{}, fmt.Errorf("-auto-browser works with exactly one profile, got %d", len(profiles))
	}
	name := profiles[0]
	s.logf("session %d: starting %s for %s (%s)", id, app.browser, req.Profile, strings.Join(req.Args, " "))
	drv, err := app.startBrowser()
	if err != nil {
		return daemon.SessionInfo{}, err
	}
	sess := &daemonSession{info: daemon.SessionInfo{ID: id, Profile: req.Profile, Browser: app.browser, Started: s.o.Now()}}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	hooks := sessionHooks{
		refreshed: func() {
			sess.mu.Lock()
			sess.info.Refreshes++
			sess.info.LastRefresh = s.o.Now()
			sess.mu.Unlock()
		},
		noted: func(msg string) {
			sess.mu.Lock()
			sess.info.Last = msg
			sess.mu.Unlock()
		},
	}
	login := autobrowser.Login{URL: req.Login.URL, Expires: req.Login.Expires}
	hosts := append(hostsOf(req.Login.URL), req.Login.Hosts...)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		code := app.runSession(ctx, drv, name, hosts, login, hooks)
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		s.logf("session %d (%s) ended (exit %d)", id, req.Profile, code)
	}()
	return sess.snapshot(), nil
}

// prefixWriter tags every line a session writes with a timestamp and the
// session's name, so the output of several sessions can be told apart in
// one log.
type prefixWriter struct {
	w   io.Writer
	now func() time.Time
	tag string
	mu  sync.Mutex
	buf []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimPrefix(string(p.buf[:i]), "awsume: ")
		p.buf = p.buf[i+1:]
		if _, err := fmt.Fprintf(p.w, "awsume: %s %s: %s\n", p.now().Format("15:04:05"), p.tag, line); err != nil {
			return len(b), err
		}
	}
	return len(b), nil
}

// startDaemon launches "awsume -daemon run" detached from this terminal.
func startDaemon(o Options, f *flags) int {
	sock := daemon.SocketPath(o.Env)
	if sock == "" {
		fmt.Fprintf(o.Stderr, "awsume: cannot pick a socket path; set %s\n", daemon.SocketEnv)
		return ExitError
	}
	if daemon.Running(sock) {
		fmt.Fprintf(o.Stderr, "awsume: the daemon is already running on %s\n", sock)
		return ExitOK
	}
	exe, err := o.Executable()
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: %v\n", err)
		return ExitError
	}
	args := []string{"-daemon", "run"}
	if f.verbose {
		args = append(args, "-v")
	}
	logPath := daemon.LogPath(o.Env)
	pid, err := o.Detach(exe, args, logPath)
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: start daemon: %v\n", err)
		return ExitError
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := daemon.WaitRunning(ctx, sock); err != nil {
		fmt.Fprintf(o.Stderr, "awsume: %v (pid %d); see %s\n", err, pid, logPath)
		return ExitError
	}
	fmt.Fprintf(o.Stderr, "awsume: daemon started (pid %d) on %s, logging to %s\n", pid, sock, logPath)
	return ExitOK
}

// stopDaemon asks the daemon to close its browsers and exit.
func stopDaemon(o Options, f *flags) int {
	sock := daemon.SocketPath(o.Env)
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	resp, err := daemon.Call(ctx, sock, daemon.Request{Op: daemon.OpStop})
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(o.Stderr, "awsume: %s (pid %d)\n", resp.Message, resp.PID)
	return ExitOK
}

// daemonStatus prints the daemon's sessions.
func daemonStatus(o Options, f *flags) int {
	sock := daemon.SocketPath(o.Env)
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	resp, err := daemon.Call(ctx, sock, daemon.Request{Op: daemon.OpStatus})
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: %v\n", err)
		return ExitError
	}
	if f.jsonOut {
		if resp.Sessions == nil {
			resp.Sessions = []daemon.SessionInfo{}
		}
		enc := json.NewEncoder(o.Stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(struct {
			PID      int                  `json:"pid"`
			Version  string               `json:"version"`
			Socket   string               `json:"socket"`
			Sessions []daemon.SessionInfo `json:"sessions"`
		}{resp.PID, resp.Version, sock, resp.Sessions})
		return ExitOK
	}
	fmt.Fprintf(o.Stdout, "daemon pid %d (awsume %s) on %s\n", resp.PID, resp.Version, sock)
	if len(resp.Sessions) == 0 {
		fmt.Fprintln(o.Stdout, "no browser sessions")
		return ExitOK
	}
	now := o.Now()
	for _, s := range resp.Sessions {
		line := fmt.Sprintf("session %d: %s in %s, open for %s, %d refresh(es)", s.ID, s.Profile, s.Browser, now.Sub(s.Started).Round(time.Second), s.Refreshes)
		if !s.LastRefresh.IsZero() {
			line += fmt.Sprintf(", last %s ago", now.Sub(s.LastRefresh).Round(time.Second))
		}
		if s.Last != "" {
			line += "; " + s.Last
		}
		fmt.Fprintln(o.Stdout, line)
	}
	return ExitOK
}

// installDaemon installs (or removes) the per-user login service.
func installDaemon(o Options, f *flags, install bool) int {
	exe, err := o.Executable()
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: %v\n", err)
		return ExitError
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	goos := o.serviceGOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	logPath := daemon.LogPath(o.Env)
	svc := &daemon.Service{
		GOOS:     goos,
		Exe:      exe,
		Socket:   daemon.SocketPath(o.Env),
		LogPath:  logPath,
		Env:      o.Env,
		UID:      os.Getuid(),
		Run:      o.RunCommand,
		LookPath: o.LookPath,
		Start: func() error {
			if daemon.Running(daemon.SocketPath(o.Env)) {
				return nil
			}
			_, err := o.Detach(exe, []string{"-daemon", "run"}, logPath)
			return err
		},
		Stop: func() error {
			ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
			defer cancel()
			_, err := daemon.Call(ctx, daemon.SocketPath(o.Env), daemon.Request{Op: daemon.OpStop})
			return err
		},
	}
	var summary string
	if install {
		summary, err = svc.Install()
	} else {
		summary, err = svc.Uninstall()
	}
	for _, line := range strings.Split(summary, "\n") {
		if line != "" {
			fmt.Fprintf(o.Stderr, "awsume: %s\n", line)
		}
	}
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: %v\n", err)
		return ExitError
	}
	return ExitOK
}
