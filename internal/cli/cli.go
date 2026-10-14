// Package cli implements the awsume command line interface.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"awsume/internal/arn"
	"awsume/internal/awsconfig"
	"awsume/internal/browser"
	"awsume/internal/clipboard"
	"awsume/internal/console"
	"awsume/internal/endpoints"
	"awsume/internal/federation"
	"awsume/internal/resolve"
	"awsume/internal/signin"
	"awsume/internal/sts"
)

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Options wires the CLI to its environment; every field has a sensible
// default so tests can substitute fakes.
type Options struct {
	Args            []string
	Stdin           io.Reader
	StdinIsTerminal bool
	Stdout          io.Writer
	Stderr          io.Writer
	Env             func(string) string
	Version         string

	// HTTP overrides the HTTP client used for every API call.
	HTTP *http.Client
	// LookPath overrides executable lookup (aws CLI fallback).
	LookPath func(string) (string, error)
	// Clipboard overrides the clipboard writer.
	Clipboard func(text string) error
	// Browser overrides the browser launcher.
	Browser func(url string) error
	// Now overrides the clock.
	Now func() time.Time
	// LoadConfig overrides how the AWS config files are loaded; it receives
	// the resolved config and credentials file paths.
	LoadConfig func(configPath, credentialsPath string) (*awsconfig.Config, error)
	// SSOCacheDir overrides the IAM Identity Center token cache directory.
	SSOCacheDir string
}

type flags struct {
	profile         string
	configFile      string
	credentialsFile string
	region          string
	service         string
	destination     string
	arn             string
	clipboard       bool
	open            bool
	jsonOut         bool
	verbose         bool
	version         bool
	listProfiles    bool
	listServices    bool

	roleARN      string
	externalID   string
	sessionName  string
	mfaSerial    string
	mfaToken     string
	roleDuration int
	maxDuration  int

	partition       string
	stsEndpoint     string
	signinEndpoint  string
	consoleEndpoint string
	ssoEndpoint     string
	ssoOIDCEndpoint string

	issuer          string
	policy          string
	federationName  string
	resolver        string
	methods         string
	timeout         time.Duration
	noIdentityCheck bool
	noStdin         bool
}

// record is the JSON output document.
type record struct {
	Profile                string `json:"profile"`
	URL                    string `json:"url"`
	Method                 string `json:"method"`
	SessionDurationSeconds int    `json:"session_duration_seconds"`
	Expires                string `json:"expires,omitempty"`
	Destination            string `json:"destination"`
	Region                 string `json:"region,omitempty"`
	Partition              string `json:"partition"`
	Account                string `json:"account,omitempty"`
	ARN                    string `json:"arn,omitempty"`
	IdentityKind           string `json:"identity_kind"`
	CredentialSource       string `json:"credential_source,omitempty"`
	Warning                string `json:"warning,omitempty"`
}

func newFlagSet(f *flags, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("awsume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	str := func(p *string, long, short, def string) {
		fs.StringVar(p, long, def, "")
		if short != "" {
			fs.StringVar(p, short, def, "")
		}
	}
	boolean := func(p *bool, long, short string) {
		fs.BoolVar(p, long, false, "")
		if short != "" {
			fs.BoolVar(p, short, false, "")
		}
	}

	str(&f.profile, "profile", "p", "")
	str(&f.configFile, "config-file", "", "")
	str(&f.credentialsFile, "credentials-file", "", "")
	str(&f.region, "region", "r", "")
	str(&f.service, "service", "s", "")
	str(&f.destination, "destination", "d", "")
	str(&f.arn, "arn", "", "")
	boolean(&f.clipboard, "clipboard", "c")
	boolean(&f.open, "open", "o")
	boolean(&f.jsonOut, "json", "j")
	boolean(&f.verbose, "verbose", "v")
	boolean(&f.version, "version", "V")
	boolean(&f.listProfiles, "list-profiles", "")
	boolean(&f.listServices, "list-services", "")

	str(&f.roleARN, "role-arn", "", "")
	str(&f.externalID, "external-id", "", "")
	str(&f.sessionName, "session-name", "", "")
	str(&f.mfaSerial, "mfa-serial", "", "")
	str(&f.mfaToken, "mfa-token", "", "")
	fs.IntVar(&f.roleDuration, "role-duration", 3600, "")
	fs.IntVar(&f.maxDuration, "duration", 0, "")

	str(&f.partition, "partition", "", "")
	str(&f.stsEndpoint, "sts-endpoint", "", "")
	str(&f.signinEndpoint, "signin-endpoint", "", "")
	str(&f.consoleEndpoint, "console-endpoint", "", "")
	str(&f.ssoEndpoint, "sso-endpoint", "", "")
	str(&f.ssoOIDCEndpoint, "sso-oidc-endpoint", "", "")

	str(&f.issuer, "issuer", "", "awsume")
	str(&f.policy, "policy", "", "")
	str(&f.federationName, "federation-name", "", "awsume")
	str(&f.resolver, "resolver", "", "auto")
	str(&f.methods, "methods", "", "")
	fs.DurationVar(&f.timeout, "timeout", 2*time.Minute, "")
	boolean(&f.noIdentityCheck, "no-identity-check", "")
	boolean(&f.noStdin, "no-stdin", "")
	return fs
}

// parseArgs parses flags while allowing positional arguments (profile
// names) to appear anywhere on the command line.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// Run executes the CLI and returns the process exit code.
func Run(o Options) int {
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if o.Env == nil {
		o.Env = func(string) string { return "" }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Clipboard == nil {
		o.Clipboard = clipboard.Copy
	}
	if o.Browser == nil {
		o.Browser = browser.Open
	}
	if o.LoadConfig == nil {
		o.LoadConfig = awsconfig.LoadFiles
	}

	var f flags
	fs := newFlagSet(&f, o.Stderr)
	positional, err := parseArgs(fs, o.Args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if f.version {
		fmt.Fprintf(o.Stdout, "awsume %s\n", versionString(o.Version))
		return ExitOK
	}
	if f.listServices {
		for _, s := range console.KnownServices() {
			fmt.Fprintln(o.Stdout, s)
		}
		return ExitOK
	}

	app, err := newApp(o, &f)
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: %v\n", err)
		return ExitUsage
	}
	if f.listProfiles {
		for _, name := range app.config.ProfileNames() {
			fmt.Fprintln(o.Stdout, name)
		}
		return ExitOK
	}

	profiles, err := app.profiles(positional)
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: %v\n", err)
		return ExitError
	}

	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()

	exit := ExitOK
	var urls []string
	for _, name := range profiles {
		rec, err := app.generate(ctx, name)
		if err != nil {
			label := name
			if label == "" {
				label = "default credentials"
			}
			fmt.Fprintf(o.Stderr, "awsume: %s: %v\n", label, err)
			exit = ExitError
			continue
		}
		if rec.Warning != "" {
			fmt.Fprintf(o.Stderr, "awsume: %s: warning: %s\n", rec.Profile, rec.Warning)
		}
		urls = append(urls, rec.URL)
		// With -clipboard the URL goes to the clipboard instead of stdout;
		// an explicit -json record is still printed.
		if !f.clipboard || f.jsonOut {
			app.print(rec, len(profiles) > 1)
		}
		if f.open {
			if err := o.Browser(rec.URL); err != nil {
				fmt.Fprintf(o.Stderr, "awsume: open browser: %v\n", err)
				exit = ExitError
			}
		}
	}
	if f.clipboard && len(urls) > 0 {
		if err := o.Clipboard(strings.Join(urls, "\n")); err != nil {
			fmt.Fprintf(o.Stderr, "awsume: copy to clipboard: %v\n", err)
			exit = ExitError
		} else if f.verbose {
			fmt.Fprintln(o.Stderr, "awsume: url copied to clipboard")
		}
	}
	return exit
}

func versionString(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

type app struct {
	o        Options
	f        *flags
	config   *awsconfig.Config
	resolver *resolve.Resolver
	logf     func(string, ...any)
	methods  []string
	// resource is the parsed -arn value, if any.
	resource *arn.ARN
}

func newApp(o Options, f *flags) (*app, error) {
	logf := func(string, ...any) {}
	if f.verbose {
		logf = func(format string, args ...any) {
			fmt.Fprintf(o.Stderr, "awsume: "+format+"\n", args...)
		}
	}
	if f.partition != "" {
		if _, ok := endpoints.ByName(f.partition); !ok {
			return nil, fmt.Errorf("unknown partition %q (want one of %s)", f.partition, strings.Join(endpoints.Names(), ", "))
		}
	}
	mode, err := resolve.ParseMode(f.resolver)
	if err != nil {
		return nil, err
	}
	if f.maxDuration != 0 && f.maxDuration < sts.MinDuration {
		return nil, fmt.Errorf("-duration must be at least %d seconds", sts.MinDuration)
	}
	if f.roleDuration < sts.MinDuration {
		return nil, fmt.Errorf("-role-duration must be at least %d seconds", sts.MinDuration)
	}
	if f.policy != "" && !json.Valid([]byte(f.policy)) {
		return nil, errors.New("-policy must be a JSON policy document")
	}
	var resource *arn.ARN
	if f.arn != "" {
		if f.destination != "" {
			return nil, errors.New("-arn and -destination cannot be combined")
		}
		parsed, err := arn.Parse(f.arn)
		if err != nil {
			return nil, err
		}
		if f.partition == "" {
			if _, ok := endpoints.ByName(parsed.Partition); !ok {
				return nil, fmt.Errorf("unknown partition %q in -arn", parsed.Partition)
			}
		}
		resource = &parsed
	}
	var methods []string
	if f.methods != "" {
		known := map[string]bool{signin.MethodFederationToken: true, signin.MethodAssumeRole: true, signin.MethodDirect: true, signin.MethodSSOPortal: true}
		for _, m := range strings.Split(f.methods, ",") {
			m = strings.ToLower(strings.TrimSpace(m))
			if !known[m] {
				return nil, fmt.Errorf("unknown method %q in -methods", m)
			}
			methods = append(methods, m)
		}
	}
	// When either file is given explicitly only the named files are read;
	// the default locations are not merged in.
	configPath, credentialsPath := f.configFile, f.credentialsFile
	if configPath == "" && credentialsPath == "" {
		configPath, credentialsPath = awsconfig.DefaultPaths(o.Env)
	}
	cfg, err := o.LoadConfig(configPath, credentialsPath)
	if err != nil {
		return nil, err
	}
	logf("config file %s, credentials file %s", configPath, credentialsPath)
	overrides := endpoints.Overrides{
		STS:       f.stsEndpoint,
		Signin:    f.signinEndpoint,
		Console:   f.consoleEndpoint,
		SSOPortal: f.ssoEndpoint,
		SSOOIDC:   f.ssoOIDCEndpoint,
	}
	a := &app{o: o, f: f, config: cfg, logf: logf, methods: methods, resource: resource}
	a.resolver = &resolve.Resolver{
		Config:      cfg,
		Env:         o.Env,
		HTTP:        o.HTTP,
		Endpoints:   overrides,
		Partition:   f.partition,
		MFAToken:    a.mfaToken,
		Now:         o.Now,
		Logf:        logf,
		Mode:        mode,
		LookPath:    o.LookPath,
		UserAgent:   "awsume/" + versionString(o.Version),
		SSOCacheDir: o.SSOCacheDir,
	}
	return a, nil
}

// profiles decides which profiles to process: explicit flag/positional
// values, then lines piped on stdin, then the default resolution.
func (a *app) profiles(positional []string) ([]string, error) {
	var profiles []string
	if a.f.profile != "" {
		profiles = append(profiles, a.f.profile)
	}
	profiles = append(profiles, positional...)
	if len(profiles) > 0 {
		return profiles, nil
	}
	if a.o.Stdin != nil && !a.o.StdinIsTerminal && !a.f.noStdin {
		scanner := bufio.NewScanner(a.o.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			profiles = append(profiles, line)
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		if len(profiles) > 0 {
			a.logf("read %d profile name(s) from stdin", len(profiles))
			return profiles, nil
		}
	}
	return []string{""}, nil
}

func (a *app) mfaToken(serial string) (string, error) {
	if a.f.mfaToken != "" {
		return a.f.mfaToken, nil
	}
	if !a.o.StdinIsTerminal || a.o.Stdin == nil {
		return "", fmt.Errorf("an MFA code for %s is required; pass -mfa-token", serial)
	}
	fmt.Fprintf(a.o.Stderr, "MFA code for %s: ", serial)
	reader := bufio.NewReader(a.o.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read MFA code: %w", err)
	}
	code := strings.TrimSpace(line)
	if code == "" {
		return "", errors.New("empty MFA code")
	}
	return code, nil
}

// generate produces the console URL for one profile name ("" means the
// default credential resolution).
func (a *app) generate(ctx context.Context, name string) (*record, error) {
	f := a.f
	res, err := a.resolver.Resolve(ctx, name)
	var warning string
	if err != nil {
		// IAM Identity Center profiles can still get an access portal link.
		if res == nil || res.SSO == nil || res.SSO.StartURL == "" {
			return nil, err
		}
		if len(a.methods) > 0 && !contains(a.methods, signin.MethodSSOPortal) {
			return nil, err
		}
		warning = fmt.Sprintf("%v; returning an access portal link that only works when the browser already has an active IAM Identity Center session", err)
		a.logf("%s", warning)
	}

	region := f.region
	if region == "" {
		region = res.Region
	}
	partition := res.Partition
	if f.partition == "" && f.region != "" {
		partition = endpoints.ForRegion(f.region)
	}
	if a.resource != nil {
		// The resource decides where to land: its region (when it has one)
		// and its partition take precedence over the profile's.
		if a.resource.Region != "" {
			region = a.resource.Region
		}
		if f.partition == "" {
			if p, ok := endpoints.ByName(a.resource.Partition); ok {
				partition = p
			}
		}
	}

	var (
		identity sts.Identity
		kind     = signin.Classify("", res.Credentials.IsTemporary())
	)
	if !res.Credentials.IsZero() && !f.noIdentityCheck {
		client := a.resolver.STSClient(res.Profile, region)
		if id, err := client.GetCallerIdentity(ctx, res.Credentials); err != nil {
			a.logf("GetCallerIdentity failed, continuing with a guessed identity type: %v", err)
		} else {
			identity = id
			kind = signin.Classify(id.ARN, res.Credentials.IsTemporary())
			if f.partition == "" {
				if p, ok := endpoints.FromARN(id.ARN); ok {
					partition = p
				}
			}
			a.logf("caller identity: %s (%s)", id.ARN, kind)
		}
	}
	if res.Credentials.Source == "federation-token" {
		kind = signin.KindFederatedUser
	}

	er := endpoints.Resolver{Partition: partition, Region: region, Overrides: a.resolver.Endpoints, Env: a.o.Env}
	if res.Profile != nil {
		er.ProfileEndpoint = res.Profile.Get("endpoint_url")
		er.ProfileSTSEndpoint = a.config.ServiceEndpoint(res.Profile.Get("services"), "sts")
		er.STSRegionalEndpoints = res.Profile.Get("sts_regional_endpoints")
	}
	stsEndpoint, signingRegion := er.STS()
	stsClient := sts.New(stsEndpoint, signingRegion, a.o.HTTP)
	stsClient.Now = a.o.Now
	stsClient.UserAgent = "awsume/" + versionString(a.o.Version)
	fed := federation.New(er.Signin(), a.o.HTTP)
	fed.UserAgent = stsClient.UserAgent

	destination := f.destination
	switch {
	case destination != "":
	case a.resource != nil:
		destination = console.ResourceDestination(er.Console(), partition.RegionalConsoleHost, *a.resource, region)
	default:
		destination = console.Destination(er.Console(), partition.RegionalConsoleHost, region, f.service)
	}

	opts := signin.Options{
		Credentials:    res.Credentials,
		Kind:           kind,
		STS:            stsClient,
		Federation:     fed,
		Issuer:         f.issuer,
		Destination:    destination,
		RoleARN:        f.roleARN,
		ExternalID:     f.externalID,
		SessionName:    f.sessionName,
		MFASerial:      f.mfaSerial,
		RoleDuration:   f.roleDuration,
		MaxDuration:    f.maxDuration,
		Policy:         f.policy,
		FederationName: f.federationName,
		Methods:        a.methods,
		Logf:           a.logf,
		Now:            a.o.Now,
	}
	if f.mfaSerial != "" && f.roleARN != "" {
		code, err := a.mfaToken(f.mfaSerial)
		if err != nil {
			return nil, err
		}
		opts.MFAToken = code
	}
	if res.SSO != nil && res.SSO.StartURL != "" {
		opts.SSO = &signin.SSOPortal{StartURL: res.SSO.StartURL, AccountID: res.SSO.AccountID, RoleName: res.SSO.RoleName}
	}

	a.logf("credential chain: %s", strings.Join(res.Chain, " -> "))
	a.logf("sts endpoint %s, signin endpoint %s, destination %s", stsEndpoint, er.Signin(), destination)

	result, err := signin.Execute(ctx, opts)
	if err != nil {
		var all *signin.AllFailedError
		if errors.As(err, &all) && kind == signin.KindUserSession && f.roleARN == "" {
			return nil, fmt.Errorf("%w\n  hint: credentials from sts:GetSessionToken cannot be federated into the console; pass -role-arn to assume a role first", err)
		}
		return nil, err
	}
	rec := &record{
		Profile:                displayName(name, res.ProfileName),
		URL:                    result.URL,
		Method:                 result.Method,
		SessionDurationSeconds: result.SessionDuration,
		Destination:            destination,
		Region:                 region,
		Partition:              partition.ID,
		Account:                identity.Account,
		ARN:                    identity.ARN,
		IdentityKind:           kind.String(),
		CredentialSource:       res.Credentials.Source,
		Warning:                warning,
	}
	if !result.Expires.IsZero() {
		rec.Expires = result.Expires.UTC().Format(time.RFC3339)
	}
	if result.Method == signin.MethodSSOPortal && warning == "" {
		rec.Warning = "access portal link requires an active IAM Identity Center session in the browser"
	}
	return rec, nil
}

func displayName(requested, resolved string) string {
	if requested != "" {
		return requested
	}
	if resolved != "" {
		return resolved
	}
	return "default"
}

func (a *app) print(rec *record, multiple bool) {
	switch {
	case a.f.jsonOut:
		enc := json.NewEncoder(a.o.Stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(rec)
	case multiple:
		fmt.Fprintf(a.o.Stdout, "%s\t%s\n", rec.Profile, rec.URL)
	default:
		fmt.Fprintln(a.o.Stdout, rec.URL)
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
