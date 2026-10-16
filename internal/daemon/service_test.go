package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recorder struct {
	cmds []string
	fail map[string]bool
}

func (r *recorder) run(name string, args ...string) error {
	line := name + " " + strings.Join(args, " ")
	r.cmds = append(r.cmds, line)
	if r.fail[line] {
		return errors.New("failed")
	}
	return nil
}

func newService(goos string, m map[string]string, r *recorder) *Service {
	return &Service{
		GOOS:     goos,
		Exe:      "/opt/bin/awsume",
		Socket:   "/run/user/1000/awsume/daemon.sock",
		LogPath:  "/home/me/.cache/awsume/daemon.log",
		Env:      env(m),
		UID:      501,
		Run:      r.run,
		LookPath: func(name string) (string, error) { return "/bin/" + name, nil },
	}
}

func TestServiceLinux(t *testing.T) {
	home := t.TempDir()
	r := &recorder{}
	s := newService("linux", map[string]string{"HOME": home}, r)
	s.Exe = "/opt/my tools/awsume"
	summary, err := s.Install()
	if err != nil {
		t.Fatalf("%v\n%s", err, summary)
	}
	unit := filepath.Join(home, ".config", "systemd", "user", "awsume.service")
	data, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(data); !strings.Contains(s, "ExecStart=\"/opt/my tools/awsume\" -daemon run\n") || !strings.Contains(s, "Environment=AWSUME_DAEMON_SOCKET=/run/user/1000/awsume/daemon.sock\n") || !strings.Contains(s, "Restart=on-failure") {
		t.Errorf("unit = %s", s)
	}
	if strings.Join(r.cmds, "|") != "systemctl --user daemon-reload|systemctl --user enable --now awsume.service" {
		t.Errorf("cmds = %v", r.cmds)
	}
	if !strings.Contains(summary, "wrote "+unit) || !strings.Contains(summary, "enabled and started") {
		t.Errorf("summary = %s", summary)
	}
	r.cmds = nil
	r.fail = map[string]bool{"systemctl --user disable --now awsume.service": true}
	summary, err = s.Uninstall()
	if err != nil || !strings.Contains(summary, "disable --now awsume.service: failed") || !strings.Contains(summary, "removed "+unit) {
		t.Errorf("uninstall: %v\n%s", err, summary)
	}
	if _, err := os.Stat(unit); !errors.Is(err, os.ErrNotExist) {
		t.Error("unit still there")
	}
	summary, _ = s.Uninstall()
	if !strings.Contains(summary, "was not installed") {
		t.Errorf("second uninstall: %s", summary)
	}
	// XDG_CONFIG_HOME wins over HOME.
	s = newService("linux", map[string]string{"XDG_CONFIG_HOME": "/xdg", "HOME": home}, r)
	if p, _ := s.Path(); p != "/xdg/systemd/user/awsume.service" {
		t.Errorf("path = %s", p)
	}
}

func TestServiceDarwin(t *testing.T) {
	home := t.TempDir()
	r := &recorder{fail: map[string]bool{"launchctl bootstrap gui/501 " + filepath.Join(home, "Library", "LaunchAgents", "com.awsume.daemon.plist"): true}}
	s := newService("darwin", map[string]string{"HOME": home}, r)
	s.Exe = "/Applications/a&b/awsume"
	summary, err := s.Install()
	if err != nil {
		t.Fatalf("%v\n%s", err, summary)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", "com.awsume.daemon.plist")
	data, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>com.awsume.daemon</string>", "<string>/Applications/a&amp;b/awsume</string>", "<string>-daemon</string>\n\t\t<string>run</string>", "<key>AWSUME_DAEMON_SOCKET</key>", "<key>StandardErrorPath</key>\n\t<string>/home/me/.cache/awsume/daemon.log</string>", "<key>KeepAlive</key>\n\t<true/>"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("plist lacks %q:\n%s", want, data)
		}
	}
	// bootstrap failed, so the legacy load ran.
	if strings.Join(r.cmds, "|") != "launchctl bootout gui/501/com.awsume.daemon|launchctl bootstrap gui/501 "+plist+"|launchctl load -w "+plist {
		t.Errorf("cmds = %v", r.cmds)
	}
	r.cmds = nil
	if _, err := s.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if r.cmds[0] != "launchctl bootout gui/501/com.awsume.daemon" {
		t.Errorf("cmds = %v", r.cmds)
	}
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Error("plist still there")
	}
}

func TestServiceWindows(t *testing.T) {
	appdata := t.TempDir()
	r := &recorder{}
	s := newService("windows", map[string]string{"APPDATA": appdata}, r)
	s.Exe = `C:\Tools\awsume.exe`
	s.LogPath = `C:\Users\me\AppData\Local\awsume\daemon.log`
	s.Socket = `C:\Users\me\AppData\Local\awsume\daemon.sock`
	started, stopped := false, false
	s.Start = func() error { started = true; return nil }
	s.Stop = func() error { stopped = true; return nil }
	summary, err := s.Install()
	if err != nil {
		t.Fatalf("%v\n%s", err, summary)
	}
	script := filepath.Join(appdata, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "awsume-daemon.vbs")
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	want := `sh.Run "cmd /c """"C:\Tools\awsume.exe"" -daemon run >> ""C:\Users\me\AppData\Local\awsume\daemon.log"" 2>&1""", 0, False` + "\r\n"
	if !strings.Contains(string(data), want) || !strings.Contains(string(data), `sh.Environment("PROCESS")("AWSUME_DAEMON_SOCKET") = "C:\Users\me\AppData\Local\awsume\daemon.sock"`) {
		t.Errorf("script = %s", data)
	}
	if !started || !strings.Contains(summary, "started the daemon now") || len(r.cmds) != 0 {
		t.Errorf("started=%v cmds=%v summary=%s", started, r.cmds, summary)
	}
	if _, err := s.Uninstall(); err != nil || !stopped {
		t.Errorf("uninstall: %v stopped=%v", err, stopped)
	}
	if _, err := os.Stat(script); !errors.Is(err, os.ErrNotExist) {
		t.Error("script still there")
	}
	if _, err := (&Service{GOOS: "plan9", Env: env(nil)}).Install(); err == nil {
		t.Error("plan9 installed")
	}
}
