package resolve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"awsume/internal/awscreds"
)

// processOutput is the JSON document produced by credential_process and by
// "aws configure export-credentials --format process".
type processOutput struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration"`
}

func (o processOutput) toCredentials(source string) (awscreds.Credentials, error) {
	if o.Version != 0 && o.Version != 1 {
		return awscreds.Credentials{}, fmt.Errorf("unsupported credential process output version %d", o.Version)
	}
	creds := awscreds.Credentials{
		AccessKeyID:     o.AccessKeyID,
		SecretAccessKey: o.SecretAccessKey,
		SessionToken:    o.SessionToken,
		Source:          source,
	}
	if err := creds.Validate(); err != nil {
		return creds, err
	}
	if o.Expiration != "" {
		t, err := time.Parse(time.RFC3339, o.Expiration)
		if err != nil {
			return creds, fmt.Errorf("parse Expiration %q: %w", o.Expiration, err)
		}
		creds.Expiration = t
	}
	return creds, nil
}

func parseProcessOutput(data []byte, source string) (awscreds.Credentials, error) {
	var out processOutput
	if err := json.Unmarshal(bytes.TrimSpace(data), &out); err != nil {
		return awscreds.Credentials{}, fmt.Errorf("decode credential JSON: %w", err)
	}
	return out.toCredentials(source)
}

// runShell executes a command line through the platform shell, the way the
// AWS CLI runs credential_process.
func runShell(ctx context.Context, command string, base, env []string) ([]byte, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	return runCmd(cmd, base, env)
}

// runCmd runs cmd with extra environment variables appended to base (the
// current environment when base is nil).
func runCmd(cmd *exec.Cmd, base, env []string) ([]byte, error) {
	if base != nil || len(env) > 0 {
		if base == nil {
			base = os.Environ()
		}
		cmd.Env = append(append([]string(nil), base...), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, err
		}
		if len(msg) > 400 {
			msg = msg[:400] + "..."
		}
		return nil, fmt.Errorf("%w: %s", err, msg)
	}
	return stdout.Bytes(), nil
}

func (r *Resolver) processProfile(ctx context.Context, res *Result) (*Result, error) {
	command := res.Profile.Get("credential_process")
	r.logf("running credential_process for profile %q", res.ProfileName)
	out, err := runShell(ctx, command, r.Environ, r.SubprocessEnv)
	if err != nil {
		return nil, fmt.Errorf("profile %q: credential_process: %w", res.ProfileName, err)
	}
	creds, err := parseProcessOutput(out, "credential-process")
	if err != nil {
		return nil, fmt.Errorf("profile %q: credential_process: %w", res.ProfileName, err)
	}
	res.Credentials = creds
	res.Chain = append(res.Chain, "credential_process from profile "+res.ProfileName)
	return res, nil
}

// viaAWSCLI runs "aws configure export-credentials" for the profile.
func (r *Resolver) viaAWSCLI(ctx context.Context, profile string) (*Result, error) {
	path, err := r.lookPath("aws")
	if err != nil {
		return nil, ErrAWSCLINotFound
	}
	args := []string{"configure", "export-credentials", "--format", "process"}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	r.logf("running %s %s", path, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, path, args...)
	out, err := runCmd(cmd, r.Environ, r.SubprocessEnv)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "Invalid choice") || strings.Contains(msg, "usage: aws") {
			return nil, ErrAWSCLIUnsupported
		}
		return nil, fmt.Errorf("aws configure export-credentials: %w", err)
	}
	creds, err := parseProcessOutput(out, "aws-cli")
	if err != nil {
		return nil, fmt.Errorf("aws configure export-credentials: %w", err)
	}
	res := &Result{Credentials: creds, ProfileName: profile, Chain: []string{"aws configure export-credentials --profile " + profile}}
	if p, ok := r.config().Profile(profile); ok {
		res.Profile = p
		res.Region = r.regionFor(p)
	} else {
		res.Region = r.envRegion()
	}
	res.Partition = r.partitionFor(res.Region)
	return res, nil
}
