package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awsume/internal/awsconfig"
)

// completeWith runs the hidden __complete command through the harness
// (fake LoadConfig) and returns stdout.
func completeWith(t *testing.T, h *harness, words ...string) string {
	t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	args := append([]string{completeCommand}, words...)
	code := Run(Options{
		Args:       args,
		Stdout:     &h.stdout,
		Stderr:     &h.stderr,
		Env:        func(k string) string { return h.env[k] },
		LoadConfig: func(string, string) (*awsconfig.Config, error) { return h.cfg, nil },
	})
	if code != ExitOK {
		t.Fatalf("__complete %v: exit %d: %s", words, code, h.stderr.String())
	}
	return h.stdout.String()
}

func TestCompleteProfilesAndFlags(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	cases := []struct {
		words []string
		want  string
	}{
		{[]string{"1", ""}, "default\ngov\nrole\ntemp\n"},
		{[]string{"1"}, "default\ngov\nrole\ntemp\n"}, // empty word dropped by the shell
		{[]string{"1", "t"}, "temp\n"},
		{[]string{"3", "-r", "us-east-1", "g"}, "gov\n"},
		{[]string{"2", "-p", "ro"}, "role\n"},
		{[]string{"2", "-profile", ""}, "default\ngov\nrole\ntemp\n"},
		{[]string{"1", "-p=r"}, "-p=role\n"},
		{[]string{"1", "-li"}, "-list-profiles\n-list-services\n"},
		{[]string{"1", "--vers"}, "--version\n"},
		{[]string{"1", "-h"}, "-h\n-help\n"},
		{[]string{"2", "--", "-"}, ""},
		{[]string{"2", "-config-file", ""}, ":file\n"},
		{[]string{"2", "-credentials-file", "x"}, ":file\n"},
		{[]string{"2", "-s", "ec"}, "ec2\necr\necs\n"},
		{[]string{"2", "-resolver", "n"}, "native\n"},
		{[]string{"2", "-daemon", "st"}, "start\nstop\nstatus\n"},
		{[]string{"2", "-partition", "aws-"}, "aws-us-gov\naws-cn\n"},
		{[]string{"2", "-methods", "direct,s"}, "direct,sso-portal\n"},
		{[]string{"2", "-region", "u"}, ""},
		{[]string{"3", "-v", "-json", "d"}, "default\n"}, // bool flags take no value
		{[]string{}, "default\ngov\nrole\ntemp\n"},
	}
	for _, tc := range cases {
		if got := completeWith(t, h, tc.words...); got != tc.want {
			t.Errorf("%v: got %q, want %q", tc.words, got, tc.want)
		}
	}
}

func TestCompleteReadsNamedAndDefaultFiles(t *testing.T) {
	dir := t.TempDir()
	envCfg := filepath.Join(dir, "env-config")
	envCreds := filepath.Join(dir, "env-credentials")
	lootCfg := filepath.Join(dir, "loot-config")
	lootCreds := filepath.Join(dir, "loot-credentials")
	os.WriteFile(envCfg, []byte("[profile env-one]\nregion = us-east-1\n"), 0o600)
	os.WriteFile(envCreds, []byte("[env-two]\naws_access_key_id = A\naws_secret_access_key = s\n"), 0o600)
	os.WriteFile(lootCfg, []byte("[profile loot-one]\nregion = us-east-1\n"), 0o600)
	os.WriteFile(lootCreds, []byte("[loot-two]\naws_access_key_id = B\naws_secret_access_key = s\n"), 0o600)

	run := func(env map[string]string, words ...string) string {
		var stdout, stderr bytes.Buffer
		code := Run(Options{
			Args:   append([]string{completeCommand}, words...),
			Stdout: &stdout,
			Stderr: &stderr,
			Env:    func(k string) string { return env[k] },
		})
		if code != ExitOK {
			t.Fatalf("%v: exit %d: %s", words, code, stderr.String())
		}
		return stdout.String()
	}
	env := map[string]string{"AWS_CONFIG_FILE": envCfg, "AWS_SHARED_CREDENTIALS_FILE": envCreds}

	// Environment (or ~/.aws) files when nothing is named on the line.
	if got := run(env, "1", ""); got != "env-one\nenv-two\n" {
		t.Errorf("env files: %q", got)
	}
	// Files named on the line replace the defaults entirely.
	if got := run(env, "5", "-config-file", lootCfg, "-credentials-file", lootCreds, ""); got != "loot-one\nloot-two\n" {
		t.Errorf("both flags: %q", got)
	}
	if got := run(env, "3", "-credentials-file", lootCreds, "loot"); got != "loot-two\n" {
		t.Errorf("credentials flag only: %q", got)
	}
	if got := run(env, "2", "-config-file="+lootCfg, ""); got != "loot-one\n" {
		t.Errorf("flag=value form: %q", got)
	}
	// Flags after the cursor word are irrelevant; flags before it are not
	// disturbed by an unrelated pending value.
	if got := run(env, "4", "-config-file", lootCfg, "-p", "loot"); got != "loot-one\n" {
		t.Errorf("-p after -config-file: %q", got)
	}
	// An unreadable file yields no candidates rather than an error.
	if got := run(env, "3", "-config-file", dir, ""); got != "" {
		t.Errorf("unreadable file: %q", got)
	}
}

func TestCompletionFlag(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	for shell, marker := range map[string]string{
		"bash":       "complete -F _awsume_complete awsume",
		"zsh":        "compdef _awsume awsume",
		"fish":       "complete -c awsume -f -a '(__awsume_complete)'",
		"powershell": "Register-ArgumentCompleter -Native -CommandName 'awsume'",
		"pwsh":       "Register-ArgumentCompleter",
	} {
		if code := h.run("-completion", shell); code != ExitOK {
			t.Errorf("%s: exit %d: %s", shell, code, h.stderr.String())
		} else if out := h.stdout.String(); !strings.Contains(out, marker) || !strings.Contains(out, completeCommand) {
			t.Errorf("%s: script missing %q or %q:\n%s", shell, marker, completeCommand, out)
		}
	}
	// The bash script must stay loadable by zsh's bashcompinit: zsh reads a
	// bare name after the second colon in ${arr[@]:off:len} as a history
	// modifier ("unrecognized modifier `C'"), so the length must be $-prefixed.
	if strings.Contains(bashCompletion, ":COMP_CWORD}") {
		t.Errorf("bash script uses zsh-hostile ${arr[@]:off:COMP_CWORD}")
	}
	if code := h.run("-completion", "ksh"); code != ExitUsage || !strings.Contains(h.stderr.String(), "bash, zsh, fish, powershell") {
		t.Errorf("unknown shell: exit %d: %s", code, h.stderr.String())
	}
	if code := h.run("-h"); code != ExitOK || !strings.Contains(h.stderr.String(), "-completion <shell>") {
		t.Errorf("usage should mention -completion: %s", h.stderr.String())
	}
}
