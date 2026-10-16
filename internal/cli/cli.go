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
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"awsume/internal/arn"
	"awsume/internal/autobrowser"
	"awsume/internal/awsconfig"
	"awsume/internal/browser"
	"awsume/internal/clipboard"
	"awsume/internal/console"
	"awsume/internal/daemon"
	"awsume/internal/endpoints"
	"awsume/internal/federation"
	"awsume/internal/proxy"
	"awsume/internal/resolve"
	"awsume/internal/signin"
	"awsume/internal/sts"
	"awsume/internal/webdriver"
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
	// Environ lists the whole environment, for handing a browser session
	// to the daemon; nil uses os.Environ.
	Environ func() []string
	Version string

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
	// StartDriver overrides how the -auto-browser browser is launched.
	StartDriver func(ctx context.Context, cfg webdriver.Config) (BrowserDriver, error)
	// Executable overrides the path of this program (-daemon start/install).
	Executable func() (string, error)
	// Detach overrides how the daemon is started in the background.
	Detach func(exe string, args []string, logPath string) (int, error)
	// RunCommand overrides how systemctl/launchctl are run by -daemon
	// install and uninstall.
	RunCommand func(name string, args ...string) error

	// browserPoll and browserSleep let tests run the -auto-browser loop
	// without waiting.
	browserPoll  time.Duration
	browserSleep func(ctx context.Context, d time.Duration) error
	// environ, when set, is the environment of the client whose browser
	// session the daemon is running; it is given to the driver and to
	// credential subprocesses instead of the daemon's own.
	environ []string
	// defaultLookPath records that LookPath was not overridden, so the
	// daemon may replace it with one that searches the client's PATH.
	defaultLookPath bool
	// serviceGOOS overrides the platform -daemon install acts for.
	serviceGOOS string
}

// BrowserDriver is a browser session that can be driven and closed.
type BrowserDriver interface {
	autobrowser.Driver
	Quit() error
}

func startDriver(ctx context.Context, cfg webdriver.Config) (BrowserDriver, error) {
	return webdriver.Start(ctx, cfg)
}

// browserProfileDir is where the -auto-browser profile for browser is kept
// between runs, so the console remembers client-side preferences such as a
// dismissed navigation tour. It lives under awsume's cache directory (see
// daemon.CacheDir) and is never shared with the daily browser. Empty when no
// cache directory can be determined; the browser then gets a throwaway
// profile.
func browserProfileDir(browser string, env func(string) string) string {
	base := daemon.CacheDir(env)
	if base == "" {
		return ""
	}
	return filepath.Join(base, "browser", browser)
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
	completion      string

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

	proxy         string
	insecure      bool
	autoBrowser   string
	refresh       time.Duration
	chromedriver  string
	geckodriver   string
	browserBinary string
	daemon        string
	noDaemon      bool
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

	// expires is Expires as a time; hosts are the sign-in and console
	// hosts involved, for -auto-browser.
	expires time.Time
	hosts   []string
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
	str(&f.completion, "completion", "", "")

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

	str(&f.proxy, "proxy", "", "")
	boolean(&f.insecure, "insecure", "k")
	str(&f.autoBrowser, "auto-browser", "ab", "")
	fs.DurationVar(&f.refresh, "refresh", 5*time.Minute, "")
	str(&f.chromedriver, "chromedriver", "", "")
	str(&f.geckodriver, "geckodriver", "", "")
	str(&f.browserBinary, "browser-binary", "", "")
	str(&f.daemon, "daemon", "", "")
	boolean(&f.noDaemon, "no-daemon", "")
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
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
		o.defaultLookPath = true
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
	if o.StartDriver == nil {
		o.StartDriver = startDriver
	}
	if o.Executable == nil {
		o.Executable = os.Executable
	}
	if o.Detach == nil {
		o.Detach = daemon.Detach
	}
	if o.RunCommand == nil {
		o.RunCommand = runCommand
	}

	if len(o.Args) > 0 && o.Args[0] == completeCommand {
		return complete(o, o.Args[1:])
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
	if f.completion != "" {
		script, err := completionScript(f.completion)
		if err != nil {
			fmt.Fprintf(o.Stderr, "awsume: %v\n", err)
			return ExitUsage
		}
		fmt.Fprint(o.Stdout, script)
		return ExitOK
	}

	if f.daemon != "" {
		return daemonCommand(o, &f)
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
	if app.browser != "" {
		if len(profiles) != 1 {
			fmt.Fprintf(o.Stderr, "awsume: -auto-browser works with exactly one profile, got %d\n", len(profiles))
			return ExitUsage
		}
		// A running daemon takes the session over, so this process can
		// exit and the terminal stays free.
		if sock := daemon.SocketPath(o.Env); !f.noDaemon && sock != "" && daemon.Running(sock) {
			return app.handOff(profiles[0], sock)
		}
		return app.autoBrowser(profiles[0])
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
	// proxy is the parsed -proxy value, if any.
	proxy *proxy.Proxy
	// browser is the normalised -auto-browser value, if any.
	browser string
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
	px, err := proxy.Parse(f.proxy)
	if err != nil {
		return nil, err
	}
	if px != nil || f.insecure {
		// Every API call, including the ones the resolver makes, goes
		// through the proxy.
		o.HTTP = proxy.Client(px, f.insecure, 30*time.Second)
		if px != nil {
			logf("proxy %s", px)
		}
	}
	var browserName, defaultSessionName string
	if f.autoBrowser != "" {
		browserName, err = webdriver.NormalizeBrowser(f.autoBrowser)
		if err != nil {
			return nil, fmt.Errorf("-auto-browser: %w", err)
		}
		if f.refresh < time.Minute {
			return nil, errors.New("-refresh must be at least 1m")
		}
		// Keep the identity stable across refreshes so the console does
		// not see a different session name every quarter hour.
		if f.sessionName == "" {
			f.sessionName = "awsume"
		}
		defaultSessionName = f.sessionName
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
	a := &app{o: o, f: f, config: cfg, logf: logf, methods: methods, resource: resource, proxy: px, browser: browserName}
	a.resolver = &resolve.Resolver{
		Config:             cfg,
		Env:                o.Env,
		HTTP:               o.HTTP,
		Endpoints:          overrides,
		Partition:          f.partition,
		MFAToken:           a.mfaToken,
		Now:                o.Now,
		Logf:               logf,
		Mode:               mode,
		LookPath:           o.LookPath,
		UserAgent:          "awsume/" + versionString(o.Version),
		SSOCacheDir:        o.SSOCacheDir,
		SubprocessEnv:      proxy.Env(px),
		Environ:            o.environ,
		DefaultSessionName: defaultSessionName,
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
		rec.expires = result.Expires
	}
	rec.hosts = hostsOf(er.Signin(), er.Console(), destination, result.URL)
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

// hostsOf returns the host names of the given URLs, without duplicates.
func hostsOf(urls ...string) []string {
	var hosts []string
	seen := map[string]bool{}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || seen[u.Hostname()] {
			continue
		}
		seen[u.Hostname()] = true
		hosts = append(hosts, u.Hostname())
	}
	return hosts
}

// firstLogin generates the URL for name and delivers it the way the plain
// command would (print, clipboard, open). It returns the record and an
// exit code; the record is nil when generation failed.
func (a *app) firstLogin(name string) (*record, int) {
	o, f := a.o, a.f
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	rec, err := a.generate(ctx, name)
	cancel()
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: %s: %v\n", displayName(name, ""), err)
		return nil, ExitError
	}
	if rec.Warning != "" {
		fmt.Fprintf(o.Stderr, "awsume: %s: warning: %s\n", rec.Profile, rec.Warning)
	}
	if !f.clipboard || f.jsonOut {
		a.print(rec, false)
	}
	if f.clipboard {
		if err := o.Clipboard(rec.URL); err != nil {
			fmt.Fprintf(o.Stderr, "awsume: copy to clipboard: %v\n", err)
		}
	}
	if f.open {
		if err := o.Browser(rec.URL); err != nil {
			fmt.Fprintf(o.Stderr, "awsume: open browser: %v\n", err)
		}
	}
	return rec, ExitOK
}

// startBrowser launches the -auto-browser browser.
func (a *app) startBrowser() (BrowserDriver, error) {
	o, f := a.o, a.f
	cfg := webdriver.Config{
		Browser:      a.browser,
		BinaryPath:   f.browserBinary,
		ProfileDir:   browserProfileDir(a.browser, o.Env),
		Proxy:        a.proxy,
		Insecure:     f.insecure,
		LookPath:     o.LookPath,
		Logf:         a.logf,
		StartTimeout: f.timeout,
		Env:          o.environ,
	}
	switch a.browser {
	case webdriver.Chrome:
		cfg.DriverPath = f.chromedriver
	case webdriver.Firefox:
		cfg.DriverPath = f.geckodriver
	}
	if f.verbose {
		cfg.DriverOutput = o.Stderr
	}
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	drv, err := o.StartDriver(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", a.browser, err)
	}
	return drv, nil
}

// sessionHooks lets the daemon follow a session's progress.
type sessionHooks struct {
	refreshed func()
	noted     func(string)
}

// runSession opens login in drv and keeps the console session refreshed
// until the browser is closed or ctx is cancelled, then quits the browser
// and returns the exit code.
func (a *app) runSession(ctx context.Context, drv BrowserDriver, name string, hosts []string, login autobrowser.Login, hooks sessionHooks) int {
	o, f := a.o, a.f
	sess := &autobrowser.Session{
		Driver: drv,
		Generate: func(ctx context.Context) (autobrowser.Login, error) {
			rec, err := a.generate(ctx, name)
			if err != nil {
				return autobrowser.Login{}, err
			}
			return autobrowser.Login{URL: rec.URL, Expires: rec.expires}, nil
		},
		Interval: f.refresh,
		Hosts:    hosts,
		Logf:     a.logf,
		Notify: func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			fmt.Fprintf(o.Stderr, "awsume: %s\n", msg)
			if hooks.noted != nil {
				hooks.noted(msg)
			}
		},
		Now:         o.Now,
		Poll:        o.browserPoll,
		Sleep:       o.browserSleep,
		CallTimeout: f.timeout,
	}
	if hooks.refreshed != nil {
		sess.Refreshed = func(autobrowser.Login) { hooks.refreshed() }
	}
	err := sess.Run(ctx, login)
	switch {
	case errors.Is(err, autobrowser.ErrBrowserClosed):
		fmt.Fprintln(o.Stderr, "awsume: browser closed")
		_ = drv.Quit()
		return ExitOK
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(o.Stderr, "awsume: interrupted; closing the browser")
		_ = drv.Quit()
		return ExitOK
	default:
		fmt.Fprintf(o.Stderr, "awsume: %v\n", err)
		_ = drv.Quit()
		return ExitError
	}
}

// autoBrowser generates the URL for name, opens it in the chosen browser
// and keeps the console session refreshed until the browser is closed or
// the process is interrupted.
func (a *app) autoBrowser(name string) int {
	o, f := a.o, a.f
	rec, code := a.firstLogin(name)
	if code != ExitOK {
		return code
	}
	drv, err := a.startBrowser()
	if err != nil {
		fmt.Fprintf(o.Stderr, "awsume: %v\n", err)
		return ExitError
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(o.Stderr, "awsume: %s is open; the console session is refreshed every %s. Close the browser or press Ctrl-C to stop.\n", a.browser, f.refresh)
	fmt.Fprintln(o.Stderr, "awsume: hint: with the daemon running (awsume -daemon start) this command returns at once and the daemon keeps the browser going")
	return a.runSession(sigCtx, drv, name, rec.hosts, autobrowser.Login{URL: rec.URL, Expires: rec.expires}, sessionHooks{})
}
