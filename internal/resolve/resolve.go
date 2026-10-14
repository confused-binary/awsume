// Package resolve turns an AWS CLI profile name (or the environment) into a
// set of credentials, following the same provider chain as the AWS CLI:
// static keys, role assumption via source_profile/credential_source, web
// identity, IAM Identity Center (SSO), credential_process, container and
// instance metadata credentials. When the native chain cannot handle a
// profile and the AWS CLI is installed, "aws configure export-credentials" is
// used as a fallback.
package resolve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/confused-binary/awsume/internal/awsconfig"
	"github.com/confused-binary/awsume/internal/awscreds"
	"github.com/confused-binary/awsume/internal/endpoints"
	"github.com/confused-binary/awsume/internal/sts"
)

// Mode selects how credentials are resolved.
type Mode string

const (
	// ModeAuto tries the native resolver and falls back to the AWS CLI.
	ModeAuto Mode = "auto"
	// ModeNative only uses the built-in resolver.
	ModeNative Mode = "native"
	// ModeAWSCLI only uses "aws configure export-credentials".
	ModeAWSCLI Mode = "awscli"
)

// ParseMode validates a mode string.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeAuto, "":
		return ModeAuto, nil
	case ModeNative:
		return ModeNative, nil
	case ModeAWSCLI:
		return ModeAWSCLI, nil
	}
	return "", fmt.Errorf("unknown resolver mode %q (want auto, native or awscli)", s)
}

const defaultAssumeRoleDuration = 3600

// Resolver resolves credentials for profiles.
type Resolver struct {
	Config *awsconfig.Config
	// Env looks up environment variables; nil means no environment.
	Env func(string) string
	// HTTP is the client for STS, SSO and metadata calls; nil uses a client
	// with a sensible timeout.
	HTTP *http.Client
	// Endpoints holds explicit endpoint overrides.
	Endpoints endpoints.Overrides
	// Partition forces a partition by name; empty infers it from the region.
	Partition string
	// MFAToken is asked for a one time code when a profile requires MFA.
	MFAToken func(serial string) (string, error)
	// Now supplies the current time; nil uses time.Now.
	Now func() time.Time
	// Logf receives verbose diagnostics; nil discards them.
	Logf func(format string, args ...any)
	// SSOCacheDir overrides the SSO token cache directory (~/.aws/sso/cache).
	SSOCacheDir string
	// Mode selects native, awscli or auto resolution.
	Mode Mode
	// LookPath finds executables; nil uses exec.LookPath.
	LookPath func(file string) (string, error)
	// UserAgent is sent on API calls.
	UserAgent string
}

// SSOInfo describes an IAM Identity Center profile. It is populated even
// when fetching role credentials fails, so callers can still build an access
// portal link.
type SSOInfo struct {
	StartURL    string
	Region      string
	AccountID   string
	RoleName    string
	SessionName string
}

// Result is the outcome of resolving a profile.
type Result struct {
	Credentials awscreds.Credentials
	// ProfileName is the profile that was resolved; "" for environment
	// credentials.
	ProfileName string
	// Profile is the merged profile; nil for environment credentials.
	Profile *awsconfig.Profile
	// Region is the profile's configured region (or the environment's).
	Region string
	// Partition is the partition inferred from the region or forced by the
	// caller.
	Partition endpoints.Partition
	// SSO is set for IAM Identity Center profiles.
	SSO *SSOInfo
	// Chain lists, in order, the steps taken to obtain the credentials.
	Chain []string
}

// ProfileNotFoundError is returned when a profile does not exist.
type ProfileNotFoundError struct {
	Name            string
	ConfigPath      string
	CredentialsPath string
}

func (e *ProfileNotFoundError) Error() string {
	if e.ConfigPath == "" && e.CredentialsPath == "" {
		return fmt.Sprintf("profile %q not found", e.Name)
	}
	return fmt.Sprintf("profile %q not found in %s or %s", e.Name, e.ConfigPath, e.CredentialsPath)
}

// ErrAWSCLINotFound is returned when the aws executable is not on PATH.
var ErrAWSCLINotFound = errors.New("aws cli executable not found on PATH")

// ErrAWSCLIUnsupported is returned when the installed aws executable does
// not implement "configure export-credentials" (AWS CLI v1 or an old v2).
var ErrAWSCLIUnsupported = errors.New("installed aws cli does not support \"configure export-credentials\" (requires AWS CLI v2.11 or newer)")

func (r *Resolver) env(key string) string {
	if r.Env == nil {
		return ""
	}
	return r.Env(key)
}

func (r *Resolver) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resolver) httpClient() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (r *Resolver) lookPath(file string) (string, error) {
	if r.LookPath != nil {
		return r.LookPath(file)
	}
	return exec.LookPath(file)
}

func (r *Resolver) config() *awsconfig.Config {
	if r.Config == nil {
		return awsconfig.New(nil, nil)
	}
	return r.Config
}

func (r *Resolver) partitionFor(region string) endpoints.Partition {
	if r.Partition != "" {
		if p, ok := endpoints.ByName(r.Partition); ok {
			return p
		}
	}
	return endpoints.ForRegion(region)
}

func (r *Resolver) regionFor(p *awsconfig.Profile) string {
	if region := p.Get("region"); region != "" {
		return region
	}
	return r.envRegion()
}

func (r *Resolver) envRegion() string {
	if region := r.env("AWS_REGION"); region != "" {
		return region
	}
	return r.env("AWS_DEFAULT_REGION")
}

// STSClient builds an STS client for the profile (nil allowed) and region,
// honouring every endpoint override.
func (r *Resolver) STSClient(p *awsconfig.Profile, region string) *sts.Client {
	er := endpoints.Resolver{
		Partition:            r.partitionFor(region),
		Region:               region,
		Overrides:            r.Endpoints,
		Env:                  r.Env,
		ProfileEndpoint:      p.Get("endpoint_url"),
		ProfileSTSEndpoint:   r.config().ServiceEndpoint(p.Get("services"), "sts"),
		STSRegionalEndpoints: firstNonEmpty(p.Get("sts_regional_endpoints"), r.env("AWS_STS_REGIONAL_ENDPOINTS")),
	}
	endpoint, signingRegion := er.STS()
	c := sts.New(endpoint, signingRegion, r.httpClient())
	c.Now = r.Now
	c.UserAgent = r.UserAgent
	return c
}

// Resolve resolves credentials for profileName. An empty name uses the
// environment (AWS_ACCESS_KEY_ID etc.), then AWS_PROFILE, then "default".
//
// For IAM Identity Center profiles a non-nil Result with SSO populated may be
// returned together with an error, so that callers can still offer an access
// portal link.
func (r *Resolver) Resolve(ctx context.Context, profileName string) (*Result, error) {
	name := strings.TrimSpace(profileName)
	if name == "" {
		if res, ok := r.fromEnvironment(); ok {
			r.logf("using credentials from the environment")
			return res, nil
		}
		name = firstNonEmpty(r.env("AWS_PROFILE"), r.env("AWS_DEFAULT_PROFILE"), "default")
	}

	mode := r.Mode
	if mode == "" {
		mode = ModeAuto
	}
	var (
		nativeErr error
		partial   *Result
	)
	if mode != ModeAWSCLI {
		res, err := r.resolveProfile(ctx, name, nil, true)
		if err == nil {
			return res, nil
		}
		nativeErr = err
		partial = res
		r.logf("native resolution of profile %q failed: %v", name, err)
		if mode == ModeNative {
			return partial, nativeErr
		}
	}

	cliRes, cliErr := r.viaAWSCLI(ctx, name)
	if cliErr == nil {
		if partial != nil && partial.SSO != nil {
			cliRes.SSO = partial.SSO
		}
		return cliRes, nil
	}
	if nativeErr == nil {
		return nil, cliErr
	}
	if errors.Is(cliErr, ErrAWSCLINotFound) || errors.Is(cliErr, ErrAWSCLIUnsupported) {
		r.logf("aws cli fallback unavailable: %v", cliErr)
		return partial, nativeErr
	}
	return partial, fmt.Errorf("%w (aws cli fallback also failed: %v)", nativeErr, cliErr)
}

// resolveProfile is the native provider chain. visited guards against
// source_profile cycles; allowRole is false when resolving a profile as its
// own source so that static keys win over role_arn.
func (r *Resolver) resolveProfile(ctx context.Context, name string, visited map[string]bool, allowRole bool) (*Result, error) {
	if allowRole {
		if visited[name] {
			return nil, fmt.Errorf("profile %q: source_profile chain forms a cycle", name)
		}
		next := make(map[string]bool, len(visited)+1)
		for k, v := range visited {
			next[k] = v
		}
		next[name] = true
		visited = next
	}

	cfg := r.config()
	p, ok := cfg.Profile(name)
	if !ok {
		if name == "default" {
			if res, err := r.fromContainerOrInstance(ctx); err == nil {
				return res, nil
			}
		}
		return nil, &ProfileNotFoundError{Name: name, ConfigPath: cfg.ConfigPath, CredentialsPath: cfg.CredentialsPath}
	}

	region := r.regionFor(p)
	res := &Result{ProfileName: name, Profile: p, Region: region, Partition: r.partitionFor(region)}

	switch {
	case allowRole && p.Has("role_arn") && (p.Has("source_profile") || p.Has("credential_source")):
		return r.assumeRoleProfile(ctx, res, visited)
	case allowRole && p.Has("role_arn") && p.Has("web_identity_token_file"):
		return r.webIdentityProfile(ctx, res)
	case p.Has("sso_session") || p.Has("sso_start_url"):
		return r.ssoProfile(ctx, res)
	case p.Has("aws_access_key_id") || p.Has("aws_secret_access_key"):
		creds := awscreds.Credentials{
			AccessKeyID:     p.Get("aws_access_key_id"),
			SecretAccessKey: p.Get("aws_secret_access_key"),
			SessionToken:    firstNonEmpty(p.Get("aws_session_token"), p.Get("aws_security_token")),
			Source:          "static",
		}
		if err := creds.Validate(); err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}
		res.Credentials = creds
		res.Chain = append(res.Chain, "static credentials from profile "+name)
		return res, nil
	case p.Has("credential_process"):
		return r.processProfile(ctx, res)
	case allowRole && p.Has("role_arn"):
		return nil, fmt.Errorf("profile %q: role_arn requires source_profile, credential_source or web_identity_token_file", name)
	}

	if name == "default" {
		if res, err := r.fromContainerOrInstance(ctx); err == nil {
			return res, nil
		}
	}
	return nil, fmt.Errorf("profile %q has no credential configuration (static keys, role_arn, sso or credential_process)", name)
}

// fromEnvironment builds a Result from AWS_* environment variables.
func (r *Resolver) fromEnvironment() (*Result, bool) {
	creds := awscreds.Credentials{
		AccessKeyID:     r.env("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: r.env("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    firstNonEmpty(r.env("AWS_SESSION_TOKEN"), r.env("AWS_SECURITY_TOKEN")),
		Source:          "environment",
	}
	if creds.Validate() != nil {
		return nil, false
	}
	if exp := r.env("AWS_CREDENTIAL_EXPIRATION"); exp != "" {
		if t, err := time.Parse(time.RFC3339, exp); err == nil {
			creds.Expiration = t
		}
	}
	region := r.envRegion()
	return &Result{
		Credentials: creds,
		Region:      region,
		Partition:   r.partitionFor(region),
		Chain:       []string{"credentials from environment variables"},
	}, true
}

func (r *Resolver) assumeRoleProfile(ctx context.Context, res *Result, visited map[string]bool) (*Result, error) {
	p := res.Profile
	var (
		source *Result
		err    error
	)
	switch {
	case p.Has("source_profile"):
		src := p.Get("source_profile")
		if src == res.ProfileName {
			source, err = r.resolveProfile(ctx, src, visited, false)
		} else {
			source, err = r.resolveProfile(ctx, src, visited, true)
		}
		if err != nil {
			return nil, fmt.Errorf("profile %q: resolve source_profile %q: %w", res.ProfileName, src, err)
		}
	default:
		source, err = r.fromCredentialSource(ctx, p.Get("credential_source"))
		if err != nil {
			return nil, fmt.Errorf("profile %q: credential_source: %w", res.ProfileName, err)
		}
	}

	stsRegion := firstNonEmpty(res.Region, source.Region)
	in := sts.AssumeRoleInput{
		RoleARN:         p.Get("role_arn"),
		SessionName:     p.Get("role_session_name"),
		ExternalID:      p.Get("external_id"),
		DurationSeconds: defaultAssumeRoleDuration,
	}
	if d := p.Get("duration_seconds"); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil || n < sts.MinDuration {
			return nil, fmt.Errorf("profile %q: invalid duration_seconds %q", res.ProfileName, d)
		}
		in.DurationSeconds = n
	}
	if serial := p.Get("mfa_serial"); serial != "" {
		if r.MFAToken == nil {
			return nil, fmt.Errorf("profile %q requires MFA (%s) but no token provider is configured", res.ProfileName, serial)
		}
		code, err := r.MFAToken(serial)
		if err != nil {
			return nil, fmt.Errorf("profile %q: MFA token: %w", res.ProfileName, err)
		}
		in.SerialNumber = serial
		in.TokenCode = strings.TrimSpace(code)
	}

	r.logf("assuming role %s for profile %q", in.RoleARN, res.ProfileName)
	client := r.STSClient(p, stsRegion)
	creds, err := client.AssumeRole(ctx, source.Credentials, in)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", res.ProfileName, err)
	}
	res.Credentials = creds
	res.Chain = append(append(res.Chain, source.Chain...), "assume role "+in.RoleARN)
	if res.Region == "" {
		res.Region = source.Region
		res.Partition = r.partitionFor(res.Region)
	}
	return res, nil
}

func (r *Resolver) fromCredentialSource(ctx context.Context, source string) (*Result, error) {
	switch strings.ToLower(source) {
	case "environment":
		res, ok := r.fromEnvironment()
		if !ok {
			return nil, errors.New("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are not set")
		}
		return res, nil
	case "ec2instancemetadata":
		return r.fromInstanceMetadata(ctx)
	case "ecscontainer":
		return r.fromContainer(ctx)
	}
	return nil, fmt.Errorf("unsupported credential_source %q", source)
}

func (r *Resolver) webIdentityProfile(ctx context.Context, res *Result) (*Result, error) {
	p := res.Profile
	return r.webIdentity(ctx, res, p.Get("role_arn"), p.Get("web_identity_token_file"), p.Get("role_session_name"), p)
}

func (r *Resolver) webIdentity(ctx context.Context, res *Result, roleARN, tokenFile, sessionName string, p *awsconfig.Profile) (*Result, error) {
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read web identity token file: %w", err)
	}
	client := r.STSClient(p, res.Region)
	creds, err := client.AssumeRoleWithWebIdentity(ctx, sts.AssumeRoleWithWebIdentityInput{
		RoleARN:          roleARN,
		SessionName:      sessionName,
		WebIdentityToken: strings.TrimSpace(string(token)),
		DurationSeconds:  defaultAssumeRoleDuration,
	})
	if err != nil {
		return nil, err
	}
	res.Credentials = creds
	res.Chain = append(res.Chain, "assume role with web identity "+roleARN)
	return res, nil
}

func (r *Resolver) fromContainerOrInstance(ctx context.Context) (*Result, error) {
	if r.env("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" || r.env("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "" {
		return r.fromContainer(ctx)
	}
	if r.env("AWS_ROLE_ARN") != "" && r.env("AWS_WEB_IDENTITY_TOKEN_FILE") != "" {
		region := r.envRegion()
		res := &Result{Region: region, Partition: r.partitionFor(region)}
		return r.webIdentity(ctx, res, r.env("AWS_ROLE_ARN"), r.env("AWS_WEB_IDENTITY_TOKEN_FILE"), r.env("AWS_ROLE_SESSION_NAME"), nil)
	}
	return r.fromInstanceMetadata(ctx)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
