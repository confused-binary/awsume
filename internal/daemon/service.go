package daemon

import (
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
)

// Service installs the daemon as something that starts when the user logs
// in: a systemd user unit on Linux, a launchd agent on macOS and a Startup
// folder script on Windows. A browser has to appear on the user's desktop,
// so a system-wide service (which runs outside the login session) is never
// the right tool; all three are per-user.
type Service struct {
	// GOOS selects the platform: linux, darwin or windows.
	GOOS string
	// Exe is the absolute path of the awsume executable.
	Exe string
	// Socket is the socket path the daemon is told to listen on, so that
	// clients and daemon agree even when the service environment differs.
	Socket string
	// LogPath receives the daemon's output.
	LogPath string
	// Env reads environment variables (HOME, XDG_CONFIG_HOME, APPDATA).
	Env func(string) string
	// UID is the user id, for launchctl's gui/<uid> domain.
	UID int
	// Run executes a command; nil means the command is not available.
	Run func(name string, args ...string) error
	// LookPath finds executables; it decides whether systemctl or
	// launchctl is available.
	LookPath func(string) (string, error)
	// Start starts the daemon now (Windows, where the Startup folder only
	// acts at the next logon); nil skips it.
	Start func() error
	// Stop stops a running daemon before uninstalling (Windows); nil
	// skips it.
	Stop func() error
}

// Names of the installed service.
const (
	systemdUnit  = "awsume.service"
	launchdLabel = "com.awsume.daemon"
	windowsEntry = "awsume-daemon.vbs"
)

// Path returns the service definition file for the platform.
func (s *Service) Path() (string, error) {
	switch s.GOOS {
	case "linux":
		base := s.Env("XDG_CONFIG_HOME")
		if base == "" {
			home := s.Env("HOME")
			if home == "" {
				return "", errors.New("HOME is not set")
			}
			base = filepath.Join(home, ".config")
		}
		return filepath.Join(base, "systemd", "user", systemdUnit), nil
	case "darwin":
		home := s.Env("HOME")
		if home == "" {
			return "", errors.New("HOME is not set")
		}
		return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
	case "windows":
		appdata := s.Env("APPDATA")
		if appdata == "" {
			return "", errors.New("APPDATA is not set")
		}
		return filepath.Join(appdata, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", windowsEntry), nil
	}
	return "", fmt.Errorf("installing a login service is not supported on %s", s.GOOS)
}

// Definition returns the contents of the service definition file.
func (s *Service) Definition() (string, error) {
	switch s.GOOS {
	case "linux":
		var b strings.Builder
		b.WriteString("[Unit]\nDescription=awsume browser session daemon\nAfter=graphical-session.target\n\n[Service]\n")
		fmt.Fprintf(&b, "ExecStart=%s -daemon run\n", systemdQuote(s.Exe))
		if s.Socket != "" {
			fmt.Fprintf(&b, "Environment=%s=%s\n", SocketEnv, systemdQuote(s.Socket))
		}
		b.WriteString("Restart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n")
		return b.String(), nil
	case "darwin":
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + html.EscapeString(s.Exe) + `</string>
		<string>-daemon</string>
		<string>run</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
`)
		if s.Socket != "" {
			b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>" + SocketEnv + "</key>\n\t\t<string>" + html.EscapeString(s.Socket) + "</string>\n\t</dict>\n")
		}
		if s.LogPath != "" {
			b.WriteString("\t<key>StandardOutPath</key>\n\t<string>" + html.EscapeString(s.LogPath) + "</string>\n")
			b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>" + html.EscapeString(s.LogPath) + "</string>\n")
		}
		b.WriteString("</dict>\n</plist>\n")
		return b.String(), nil
	case "windows":
		// cmd runs the daemon hidden (window style 0) and appends its
		// output to the log file; VBScript doubles quotes inside strings.
		cmd := fmt.Sprintf(`"%s" -daemon run`, s.Exe)
		if s.LogPath != "" {
			cmd += fmt.Sprintf(` >> "%s" 2>&1`, s.LogPath)
		}
		var b strings.Builder
		b.WriteString("' Starts the awsume browser session daemon at logon. Installed by awsume -daemon install.\r\n")
		b.WriteString("Set sh = CreateObject(\"WScript.Shell\")\r\n")
		if s.Socket != "" {
			fmt.Fprintf(&b, "sh.Environment(\"PROCESS\")(\"%s\") = \"%s\"\r\n", SocketEnv, strings.ReplaceAll(s.Socket, `"`, `""`))
		}
		fmt.Fprintf(&b, "sh.Run \"cmd /c \"\"%s\"\"\", 0, False\r\n", strings.ReplaceAll(cmd, `"`, `""`))
		return b.String(), nil
	}
	return "", fmt.Errorf("installing a login service is not supported on %s", s.GOOS)
}

// systemdQuote quotes a path for a unit file when it needs it.
func systemdQuote(p string) string {
	if strings.ContainsAny(p, " \t\"'\\$%") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"`
	}
	return p
}

// Install writes the definition and starts the service, returning a
// summary of what was done.
func (s *Service) Install() (string, error) {
	path, err := s.Path()
	if err != nil {
		return "", err
	}
	def, err := s.Definition()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(def), 0o644); err != nil {
		return "", err
	}
	var lines []string
	lines = append(lines, "wrote "+path)
	switch s.GOOS {
	case "linux":
		if _, err := s.lookPath("systemctl"); err != nil {
			return strings.Join(lines, "\n"), errors.New("systemctl not found; the unit file was written but could not be enabled. Start the daemon with: awsume -daemon start")
		}
		if err := s.run("systemctl", "--user", "daemon-reload"); err != nil {
			return strings.Join(lines, "\n"), fmt.Errorf("systemctl --user daemon-reload: %w", err)
		}
		if err := s.run("systemctl", "--user", "enable", "--now", systemdUnit); err != nil {
			return strings.Join(lines, "\n"), fmt.Errorf("systemctl --user enable --now %s: %w", systemdUnit, err)
		}
		lines = append(lines, "enabled and started "+systemdUnit+" (systemctl --user status "+systemdUnit+", journalctl --user -u "+systemdUnit+")")
	case "darwin":
		if _, err := s.lookPath("launchctl"); err != nil {
			return strings.Join(lines, "\n"), errors.New("launchctl not found; the agent was written but could not be loaded. Start the daemon with: awsume -daemon start")
		}
		domain := fmt.Sprintf("gui/%d", s.UID)
		// A previous copy may still be loaded; replace it.
		_ = s.run("launchctl", "bootout", domain+"/"+launchdLabel)
		if err := s.run("launchctl", "bootstrap", domain, path); err != nil {
			if err2 := s.run("launchctl", "load", "-w", path); err2 != nil {
				return strings.Join(lines, "\n"), fmt.Errorf("launchctl bootstrap %s: %w", domain, err)
			}
		}
		lines = append(lines, "loaded "+launchdLabel+" into "+domain+" (launchctl print "+domain+"/"+launchdLabel+")")
		if s.LogPath != "" {
			lines = append(lines, "log: "+s.LogPath)
		}
	case "windows":
		lines = append(lines, "the daemon starts at the next logon")
		if s.Start != nil {
			if err := s.Start(); err != nil {
				return strings.Join(lines, "\n"), fmt.Errorf("start the daemon now: %w", err)
			}
			lines = append(lines, "started the daemon now")
		}
		if s.LogPath != "" {
			lines = append(lines, "log: "+s.LogPath)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// Uninstall stops the service and removes its definition.
func (s *Service) Uninstall() (string, error) {
	path, err := s.Path()
	if err != nil {
		return "", err
	}
	var lines []string
	switch s.GOOS {
	case "linux":
		if _, err := s.lookPath("systemctl"); err == nil {
			if err := s.run("systemctl", "--user", "disable", "--now", systemdUnit); err != nil {
				lines = append(lines, "systemctl --user disable --now "+systemdUnit+": "+err.Error())
			} else {
				lines = append(lines, "stopped and disabled "+systemdUnit)
			}
		}
	case "darwin":
		if _, err := s.lookPath("launchctl"); err == nil {
			domain := fmt.Sprintf("gui/%d", s.UID)
			if err := s.run("launchctl", "bootout", domain+"/"+launchdLabel); err != nil {
				_ = s.run("launchctl", "unload", "-w", path)
			}
			lines = append(lines, "unloaded "+launchdLabel)
		}
	case "windows":
		if s.Stop != nil {
			if err := s.Stop(); err == nil {
				lines = append(lines, "stopped the daemon")
			}
		}
	}
	switch err := os.Remove(path); {
	case err == nil:
		lines = append(lines, "removed "+path)
	case errors.Is(err, os.ErrNotExist):
		lines = append(lines, path+" was not installed")
	default:
		return strings.Join(lines, "\n"), err
	}
	if s.GOOS == "linux" {
		if _, err := s.lookPath("systemctl"); err == nil {
			_ = s.run("systemctl", "--user", "daemon-reload")
		}
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) lookPath(name string) (string, error) {
	if s.LookPath == nil {
		return "", errors.New("no executable lookup")
	}
	return s.LookPath(name)
}

func (s *Service) run(name string, args ...string) error {
	if s.Run == nil {
		return errors.New("cannot run " + name)
	}
	return s.Run(name, args...)
}
