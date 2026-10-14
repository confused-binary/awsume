package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/confused-binary/awsume/internal/awsconfig"
)

// fakeAWS simulates STS and the federation endpoint on one server.
type fakeAWS struct {
	t              *testing.T
	stsCalls       []url.Values
	federationCall []url.Values
	// denyFederationToken makes GetFederationToken fail with AccessDenied.
	denyFederationToken bool
	// maxSessionDuration rejects SessionDuration values above it.
	maxSessionDuration int
	identityARN        string
}

func (f *fakeAWS) handler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/sts/":
		data, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(data))
		f.stsCalls = append(f.stsCalls, form)
		if r.Header.Get("Authorization") == "" && form.Get("Action") != "AssumeRoleWithWebIdentity" {
			w.WriteHeader(403)
			io.WriteString(w, `<ErrorResponse><Error><Code>MissingAuthenticationToken</Code><Message>unsigned</Message></Error></ErrorResponse>`)
			return
		}
		switch form.Get("Action") {
		case "GetCallerIdentity":
			arn := f.identityARN
			if strings.Contains(r.Header.Get("Authorization"), "/us-gov-") {
				arn = strings.Replace(arn, "arn:aws:", "arn:aws-us-gov:", 1)
			}
			fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Arn>%s</Arn><UserId>AID</UserId><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`, arn)
		case "GetFederationToken":
			if f.denyFederationToken {
				w.WriteHeader(403)
				io.WriteString(w, `<ErrorResponse><Error><Code>AccessDenied</Code><Message>no gft</Message></Error></ErrorResponse>`)
				return
			}
			if form.Get("DurationSeconds") != "129600" {
				w.WriteHeader(400)
				io.WriteString(w, `<ErrorResponse><Error><Code>ValidationError</Code><Message>bad duration</Message></Error></ErrorResponse>`)
				return
			}
			io.WriteString(w, `<GetFederationTokenResponse><GetFederationTokenResult><Credentials><AccessKeyId>ASIAFED</AccessKeyId><SecretAccessKey>s</SecretAccessKey><SessionToken>fedtoken</SessionToken><Expiration>2030-01-01T00:00:00Z</Expiration></Credentials></GetFederationTokenResult></GetFederationTokenResponse>`)
		case "AssumeRole":
			io.WriteString(w, `<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>ASIAROLE</AccessKeyId><SecretAccessKey>s</SecretAccessKey><SessionToken>roletoken</SessionToken><Expiration>2030-01-01T00:00:00Z</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`)
		default:
			w.WriteHeader(400)
			io.WriteString(w, `<ErrorResponse><Error><Code>InvalidAction</Code><Message>nope</Message></Error></ErrorResponse>`)
		}
	case "/federation":
		q := r.URL.Query()
		f.federationCall = append(f.federationCall, q)
		var session map[string]string
		if err := json.Unmarshal([]byte(q.Get("Session")), &session); err != nil || session["sessionToken"] == "" {
			w.WriteHeader(400)
			io.WriteString(w, "bad session")
			return
		}
		if sd := q.Get("SessionDuration"); sd != "" {
			var n int
			fmt.Sscanf(sd, "%d", &n)
			if f.maxSessionDuration > 0 && n > f.maxSessionDuration {
				w.WriteHeader(400)
				io.WriteString(w, "SessionDuration too long")
				return
			}
		}
		fmt.Fprintf(w, `{"SigninToken":"TOKEN-%s"}`, session["sessionId"])
	default:
		w.WriteHeader(404)
	}
}

type harness struct {
	srv     *httptest.Server
	aws     *fakeAWS
	cfg     *awsconfig.Config
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	copied  []string
	opened  []string
	env     map[string]string
	stdin   string
	isTTY   bool
	copyErr error

	ssoCacheDir string
}

func newHarness(t *testing.T, config, credentials string) *harness {
	t.Helper()
	h := &harness{aws: &fakeAWS{t: t, identityARN: "arn:aws:iam::123456789012:user/alice"}, env: map[string]string{}}
	h.srv = httptest.NewServer(http.HandlerFunc(h.aws.handler))
	t.Cleanup(h.srv.Close)
	cfgFile, err := awsconfig.Parse(strings.NewReader(config))
	if err != nil {
		t.Fatal(err)
	}
	credFile, err := awsconfig.Parse(strings.NewReader(credentials))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = awsconfig.New(cfgFile, credFile)
	return h
}

func (h *harness) run(args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	base := []string{"-sts-endpoint", h.srv.URL + "/sts", "-signin-endpoint", h.srv.URL + "/federation", "-resolver", "native"}
	return Run(Options{
		Args:            append(base, args...),
		Stdin:           strings.NewReader(h.stdin),
		StdinIsTerminal: h.isTTY,
		Stdout:          &h.stdout,
		Stderr:          &h.stderr,
		Env:             func(k string) string { return h.env[k] },
		Version:         "test",
		HTTP:            h.srv.Client(),
		LookPath:        func(string) (string, error) { return "", errors.New("not found") },
		Clipboard: func(s string) error {
			h.copied = append(h.copied, s)
			return h.copyErr
		},
		Browser: func(u string) error {
			h.opened = append(h.opened, u)
			return nil
		},
		Now:         func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		LoadConfig:  func(string, string) (*awsconfig.Config, error) { return h.cfg, nil },
		SSOCacheDir: h.ssoCacheDir,
	})
}

const testConfig = `
[default]
region = us-east-1

[profile role]
role_arn = arn:aws:iam::123456789012:role/Dev
source_profile = default
region = eu-west-1

[profile gov]
region = us-gov-west-1
`

const testCredentials = `
[default]
aws_access_key_id = AKIADEFAULT
aws_secret_access_key = secret

[gov]
aws_access_key_id = AKIAGOV
aws_secret_access_key = secret

[temp]
aws_access_key_id = ASIATEMP
aws_secret_access_key = secret
aws_session_token = temptoken
`

func parseURL(t *testing.T, raw string) (*url.URL, url.Values) {
	t.Helper()
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		t.Fatalf("bad url %q: %v", raw, err)
	}
	return u, u.Query()
}

func TestUserKeysUseFederationToken(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	h.isTTY = true
	if code := h.run("-s", "ec2"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	u, q := parseURL(t, h.stdout.String())
	if u.Path != "/federation" || q.Get("Action") != "login" || q.Get("Issuer") != "awsume" || q.Get("SigninToken") != "TOKEN-ASIAFED" {
		t.Errorf("url = %s", u)
	}
	if q.Get("Destination") != "https://us-east-1.console.aws.amazon.com/ec2/home?region=us-east-1" {
		t.Errorf("destination = %s", q.Get("Destination"))
	}
	actions := []string{}
	for _, c := range h.aws.stsCalls {
		actions = append(actions, c.Get("Action")+":"+c.Get("DurationSeconds"))
	}
	if strings.Join(actions, " ") != "GetCallerIdentity: GetFederationToken:129600" {
		t.Errorf("sts calls = %v", actions)
	}
	if h.aws.stsCalls[1].Get("Policy") == "" || h.aws.stsCalls[1].Get("Name") != "awsume" {
		t.Errorf("gft call = %v", h.aws.stsCalls[1])
	}
	if len(h.aws.federationCall) != 1 || h.aws.federationCall[0].Get("SessionDuration") != "" {
		t.Errorf("federation calls = %v", h.aws.federationCall)
	}
}

func TestRoleProfileDirectWithSessionDuration(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	h.aws.identityARN = "arn:aws:sts::123456789012:assumed-role/Dev/sess"
	h.aws.maxSessionDuration = 3600
	if code := h.run("-json", "-r", "ap-southeast-2", "role"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	var rec record
	if err := json.Unmarshal(h.stdout.Bytes(), &rec); err != nil {
		t.Fatalf("json: %v: %s", err, h.stdout.String())
	}
	if rec.Profile != "role" || rec.Method != "direct" || rec.SessionDurationSeconds != 3600 || rec.Account != "123456789012" || rec.IdentityKind != "assumed-role" || rec.Region != "ap-southeast-2" || rec.Expires != "2026-01-01T01:00:00Z" || rec.CredentialSource != "assume-role" {
		t.Errorf("record = %+v", rec)
	}
	if !strings.HasPrefix(rec.Destination, "https://ap-southeast-2.console.aws.amazon.com/console/home?region=ap-southeast-2") {
		t.Errorf("destination = %s", rec.Destination)
	}
	durations := []string{}
	for _, c := range h.aws.federationCall {
		durations = append(durations, c.Get("SessionDuration"))
	}
	if strings.Join(durations, ",") != "43200,14400,3600" {
		t.Errorf("federation durations = %v", durations)
	}
	if h.aws.stsCalls[0].Get("Action") != "AssumeRole" || h.aws.stsCalls[0].Get("RoleArn") != "arn:aws:iam::123456789012:role/Dev" {
		t.Errorf("first sts call = %v", h.aws.stsCalls[0])
	}
}

func TestRoleArnFlagAfterDeniedFederationToken(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	h.aws.denyFederationToken = true
	if code := h.run("-role-arn", "arn:aws:iam::123456789012:role/Admin", "-external-id", "ext", "-no-identity-check", "-d", "https://console.aws.amazon.com/iam/home"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	_, q := parseURL(t, h.stdout.String())
	if q.Get("SigninToken") != "TOKEN-ASIAROLE" || q.Get("Destination") != "https://console.aws.amazon.com/iam/home" {
		t.Errorf("url query = %v", q)
	}
	var gft, ar int
	for _, c := range h.aws.stsCalls {
		switch c.Get("Action") {
		case "GetFederationToken":
			gft++
		case "AssumeRole":
			ar++
			if c.Get("ExternalId") != "ext" {
				t.Errorf("external id missing: %v", c)
			}
		case "GetCallerIdentity":
			t.Error("identity check should have been skipped")
		}
	}
	if gft != 1 || ar != 1 {
		t.Errorf("gft=%d assumeRole=%d", gft, ar)
	}
	if h.aws.federationCall[0].Get("SessionDuration") != "43200" {
		t.Errorf("first federation call = %v", h.aws.federationCall[0])
	}
}

func TestPipedProfilesCopyAndOpen(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	h.stdin = "default\n\n# comment\ngov\nmissing\n"
	h.aws.identityARN = "arn:aws:iam::123456789012:user/alice"
	code := h.run("-c", "-o")
	if code != ExitError {
		t.Fatalf("expected exit 1 because one profile is missing, got %d: %s", code, h.stderr.String())
	}
	// -c sends the URLs to the clipboard and prints nothing on stdout.
	if h.stdout.Len() != 0 {
		t.Errorf("stdout should be empty with -c, got %q", h.stdout.String())
	}
	if !strings.Contains(h.stderr.String(), `profile "missing" not found`) {
		t.Errorf("stderr = %s", h.stderr.String())
	}
	if len(h.opened) != 2 || len(h.copied) != 1 {
		t.Fatalf("opened=%v copied=%v", h.opened, h.copied)
	}
	copied := strings.Split(h.copied[0], "\n")
	if len(copied) != 2 || !strings.HasPrefix(copied[0], "http") || !strings.HasPrefix(copied[1], "http") {
		t.Errorf("clipboard = %q", h.copied[0])
	}
	// Without -c the URLs are printed as profile<TAB>url.
	h.copied = nil
	h.stdin = "default\ngov\n"
	if code := h.run(); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "default\thttp") || !strings.HasPrefix(lines[1], "gov\thttp") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
	// The gov profile must use the GovCloud console destination; the signin
	// endpoint is overridden in tests, so check the destination only.
	_, q := parseURL(t, strings.SplitN(lines[1], "\t", 2)[1])
	if q.Get("Destination") != "https://console.amazonaws-us-gov.com/console/home?region=us-gov-west-1" {
		t.Errorf("gov destination = %s", q.Get("Destination"))
	}
}

func TestEmptyStdinFallsBackToDefault(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	h.stdin = ""
	if code := h.run(); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.HasPrefix(h.stdout.String(), "http") || strings.Contains(h.stdout.String(), "\t") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
}

func TestEnvironmentCredentials(t *testing.T) {
	h := newHarness(t, "", "")
	h.isTTY = true
	h.env["AWS_ACCESS_KEY_ID"] = "ASIAENV"
	h.env["AWS_SECRET_ACCESS_KEY"] = "s"
	h.env["AWS_SESSION_TOKEN"] = "t"
	h.env["AWS_DEFAULT_REGION"] = "us-west-2"
	h.aws.identityARN = "arn:aws:sts::123456789012:assumed-role/Env/x"
	if code := h.run("-json"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	var rec record
	json.Unmarshal(h.stdout.Bytes(), &rec)
	if rec.Profile != "default" || rec.Method != "direct" || rec.SessionDurationSeconds != 43200 || rec.Region != "us-west-2" || rec.CredentialSource != "environment" {
		t.Errorf("record = %+v", rec)
	}
}

func TestSSOPortalFallback(t *testing.T) {
	cacheDir := t.TempDir()
	h := newHarness(t, `
[profile sso]
sso_session = corp
sso_account_id = 123456789012
sso_role_name = Admin
region = us-east-1

[sso-session corp]
sso_start_url = https://corp.awsapps.com/start
sso_region = us-east-1
`, "")
	h.isTTY = true
	h.ssoCacheDir = cacheDir
	// No SSO cache: the tool still returns a portal deep link with a warning.
	if code := h.run("-json", "-s", "s3", "sso"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	var rec record
	json.Unmarshal(h.stdout.Bytes(), &rec)
	if rec.Method != "sso-portal" || rec.Warning == "" {
		t.Errorf("record = %+v", rec)
	}
	u, _ := parseURL(t, rec.URL)
	q, err := url.ParseQuery(strings.TrimPrefix(u.Fragment, "/console?"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "corp.awsapps.com" || u.Path != "/start/" || q.Get("account_id") != "123456789012" || q.Get("role_name") != "Admin" || !strings.Contains(q.Get("destination"), "/s3/home") {
		t.Errorf("portal url = %s", rec.URL)
	}
	if !strings.Contains(h.stderr.String(), "warning") {
		t.Errorf("stderr = %s", h.stderr.String())
	}
	// Restricting methods away from sso-portal turns it into a failure.
	if code := h.run("-methods", "direct", "sso"); code != ExitError {
		t.Errorf("expected failure, got %d: %s", code, h.stdout.String())
	}
}

func TestUsageAndInformationalFlags(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	if code := h.run("-version"); code != ExitOK || h.stdout.String() != "awsume test\n" {
		t.Errorf("version: %d %q", code, h.stdout.String())
	}
	if code := h.run("-list-profiles"); code != ExitOK || h.stdout.String() != "default\ngov\nrole\ntemp\n" {
		t.Errorf("list-profiles: %d %q", code, h.stdout.String())
	}
	if code := h.run("-list-services"); code != ExitOK || !strings.Contains(h.stdout.String(), "ec2\n") {
		t.Errorf("list-services: %d", code)
	}
	if code := h.run("-h"); code != ExitOK || !strings.Contains(h.stderr.String(), "Usage:") {
		t.Errorf("help: %d %q", code, h.stderr.String())
	}
	if code := h.run("-bogus"); code != ExitUsage {
		t.Errorf("bogus flag: %d", code)
	}
	for _, args := range [][]string{
		{"-partition", "aws-iso"},
		{"-resolver", "magic"},
		{"-duration", "10"},
		{"-role-duration", "10"},
		{"-policy", "{not json"},
		{"-methods", "teleport"},
	} {
		if code := h.run(args...); code != ExitUsage {
			t.Errorf("%v: expected usage error, got %d", args, code)
		}
	}
	// Positional arguments may precede flags.
	h.aws.identityARN = "arn:aws:iam::123456789012:user/alice"
	if code := h.run("gov", "-no-identity-check", "-partition", "aws"); code != ExitOK {
		t.Fatalf("positional before flags: %d %s", code, h.stderr.String())
	}
	_, q := parseURL(t, h.stdout.String())
	if !strings.HasPrefix(q.Get("Destination"), "https://us-gov-west-1.console.aws.amazon.com/") {
		t.Errorf("forced partition destination = %s", q.Get("Destination"))
	}
}

func TestVerboseAndClipboardFailure(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	h.isTTY = true
	h.copyErr = errors.New("no clipboard")
	if code := h.run("-v", "-c", "temp"); code != ExitError {
		t.Fatalf("expected clipboard failure exit, got %d: %s", code, h.stderr.String())
	}
	errOut := h.stderr.String()
	for _, want := range []string{"trying direct", "success with", "copy to clipboard: no clipboard", "caller identity"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("verbose output missing %q:\n%s", want, errOut)
		}
	}
	if h.stdout.Len() != 0 {
		t.Errorf("stdout must stay empty with -c even when the clipboard fails: %q", h.stdout.String())
	}
	// -json together with -c still prints the record.
	h.copyErr = nil
	if code := h.run("-c", "-json", "temp"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	var rec record
	if err := json.Unmarshal(h.stdout.Bytes(), &rec); err != nil || rec.URL == "" || len(h.copied) != 2 {
		t.Errorf("json with clipboard: err=%v stdout=%q copied=%d", err, h.stdout.String(), len(h.copied))
	}
}

func TestMFAPromptAndFlag(t *testing.T) {
	cfg := `
[profile mfa]
role_arn = arn:aws:iam::123456789012:role/Dev
source_profile = default
mfa_serial = arn:aws:iam::123456789012:mfa/alice
`
	h := newHarness(t, cfg, testCredentials)
	h.aws.identityARN = "arn:aws:sts::123456789012:assumed-role/Dev/x"
	// Piped stdin without -mfa-token cannot prompt.
	h.stdin = "mfa\n"
	if code := h.run(); code != ExitError || !strings.Contains(h.stderr.String(), "-mfa-token") {
		t.Errorf("expected mfa error, got %d: %s", code, h.stderr.String())
	}
	if code := h.run("-mfa-token", "654321"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if h.aws.stsCalls[0].Get("TokenCode") != "654321" || h.aws.stsCalls[0].Get("SerialNumber") == "" {
		t.Errorf("assume role call = %v", h.aws.stsCalls[0])
	}
	// Interactive prompt reads the code from the terminal.
	h.isTTY = true
	h.stdin = "111222\n"
	h.aws.stsCalls = nil
	if code := h.run("mfa"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if h.aws.stsCalls[0].Get("TokenCode") != "111222" || !strings.Contains(h.stderr.String(), "MFA code for") {
		t.Errorf("prompted call = %v stderr=%s", h.aws.stsCalls[0], h.stderr.String())
	}
}

func TestARNFlag(t *testing.T) {
	h := newHarness(t, testConfig, testCredentials)
	h.isTTY = true
	h.aws.identityARN = "arn:aws:sts::123456789012:assumed-role/Dev/x"
	// The ARN's region (ap-northeast-1) wins over the profile region and -r.
	if code := h.run("-json", "-r", "us-west-2", "-s", "s3", "temp", "-arn", "arn:aws:ec2:ap-northeast-1:123456789012:instance/i-0abc"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	var rec record
	json.Unmarshal(h.stdout.Bytes(), &rec)
	if rec.Destination != "https://ap-northeast-1.console.aws.amazon.com/ec2/home?region=ap-northeast-1#InstanceDetails:instanceId=i-0abc" || rec.Region != "ap-northeast-1" {
		t.Errorf("record = %+v", rec)
	}
	// Global resource with no region: no region in the destination.
	if code := h.run("-json", "temp", "-arn", "arn:aws:iam::123456789012:user/alice"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	json.Unmarshal(h.stdout.Bytes(), &rec)
	if rec.Destination != "https://console.aws.amazon.com/iam/home#/users/details/alice" {
		t.Errorf("iam destination = %s", rec.Destination)
	}
	// S3 bucket has no region in the ARN: the -r/profile region is used.
	if code := h.run("-json", "-r", "us-west-2", "temp", "-arn", "arn:aws:s3:::my-bucket"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	json.Unmarshal(h.stdout.Bytes(), &rec)
	if rec.Destination != "https://us-west-2.console.aws.amazon.com/s3/buckets/my-bucket?tab=objects&region=us-west-2" {
		t.Errorf("s3 destination = %s", rec.Destination)
	}
	// Without any region at all the S3 URL is region-less.
	if code := h.run("-json", "temp", "-arn", "arn:aws:s3:::my-bucket"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	json.Unmarshal(h.stdout.Bytes(), &rec)
	if rec.Destination != "https://console.aws.amazon.com/s3/buckets/my-bucket?tab=objects" {
		t.Errorf("s3 destination without region = %s", rec.Destination)
	}
	// GovCloud ARN switches the partition.
	if code := h.run("-json", "-no-identity-check", "temp", "-arn", "arn:aws-us-gov:ec2:us-gov-west-1:123456789012:volume/vol-1"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	json.Unmarshal(h.stdout.Bytes(), &rec)
	if rec.Partition != "aws-us-gov" || rec.Destination != "https://console.amazonaws-us-gov.com/ec2/home?region=us-gov-west-1#VolumeDetails:volumeId=vol-1" {
		t.Errorf("gov record = %+v", rec)
	}
	// Usage errors.
	for _, args := range [][]string{
		{"temp", "-arn", "not-an-arn"},
		{"temp", "-arn", "arn:aws:ec2:us-east-1:1:instance/i-1", "-d", "https://example.com"},
		{"temp", "-arn", "arn:aws-iso:ec2:us-iso-east-1:1:instance/i-1"},
	} {
		if code := h.run(args...); code != ExitUsage {
			t.Errorf("%v: expected usage error, got %d: %s", args, code, h.stderr.String())
		}
	}
}

func TestLoadConfigError(t *testing.T) {
	code := Run(Options{
		Args:   []string{"-p", "x"},
		Stderr: io.Discard,
		LoadConfig: func(string, string) (*awsconfig.Config, error) {
			return nil, errors.New("cannot parse config")
		},
	})
	if code != ExitUsage {
		t.Errorf("exit = %d", code)
	}
}

func TestConfigFileFlagsOverrideEnv(t *testing.T) {
	dir := t.TempDir()
	envCfg := filepath.Join(dir, "env-config")
	envCreds := filepath.Join(dir, "env-credentials")
	flagCfg := filepath.Join(dir, "flag-config")
	flagCreds := filepath.Join(dir, "flag-credentials")
	os.WriteFile(envCfg, []byte("[profile x]\nregion = us-east-1\n"), 0o600)
	os.WriteFile(envCreds, []byte("[x]\naws_access_key_id = ASIAENV\naws_secret_access_key = s\naws_session_token = t\n"), 0o600)
	os.WriteFile(flagCfg, []byte("[profile x]\nregion = eu-north-1\n"), 0o600)
	os.WriteFile(flagCreds, []byte("[x]\naws_access_key_id = ASIAFLAG\naws_secret_access_key = s\naws_session_token = t\n"), 0o600)
	h := newHarness(t, "", "")
	h.aws.identityARN = "arn:aws:sts::123456789012:assumed-role/X/y"

	run := func(env map[string]string, args ...string) string {
		h.stdout.Reset()
		h.stderr.Reset()
		base := []string{"-sts-endpoint", h.srv.URL + "/sts", "-signin-endpoint", h.srv.URL + "/federation", "-resolver", "native", "-no-identity-check", "x"}
		code := Run(Options{
			Args:            append(base, args...),
			Stdin:           strings.NewReader(""),
			StdinIsTerminal: true,
			Stdout:          &h.stdout,
			Stderr:          &h.stderr,
			Env:             func(k string) string { return env[k] },
			HTTP:            h.srv.Client(),
		})
		if code != ExitOK {
			t.Fatalf("%v: exit %d: %s", args, code, h.stderr.String())
		}
		_, q := parseURL(t, h.stdout.String())
		return q.Get("SigninToken") + " " + q.Get("Destination")
	}

	// The alias AWS_SHARED_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE are honoured.
	got := run(map[string]string{"AWS_SHARED_CONFIG_FILE": envCfg, "AWS_SHARED_CREDENTIALS_FILE": envCreds})
	if got != "TOKEN-ASIAENV https://us-east-1.console.aws.amazon.com/console/home?region=us-east-1" {
		t.Errorf("env files: %s", got)
	}
	// Flags win over the environment.
	got = run(map[string]string{"AWS_CONFIG_FILE": envCfg, "AWS_SHARED_CREDENTIALS_FILE": envCreds}, "-config-file", flagCfg, "-credentials-file", flagCreds)
	if got != "TOKEN-ASIAFLAG https://eu-north-1.console.aws.amazon.com/console/home?region=eu-north-1" {
		t.Errorf("flag files: %s", got)
	}
	// A credentials file alone: the environment/default config file is NOT
	// merged in, so the region from env-config must not appear.
	got = run(map[string]string{"AWS_CONFIG_FILE": envCfg}, "-credentials-file", flagCreds)
	if got != "TOKEN-ASIAFLAG https://console.aws.amazon.com/console/home" {
		t.Errorf("credentials flag only: %s", got)
	}
	// A config file alone: credentials from the environment file are ignored,
	// so a profile without keys in the config file fails.
	h.stdout.Reset()
	h.stderr.Reset()
	code := Run(Options{
		Args:   []string{"-config-file", flagCfg, "-no-stdin", "-resolver", "native", "x"},
		Env:    func(k string) string { return map[string]string{"AWS_SHARED_CREDENTIALS_FILE": envCreds}[k] },
		Stderr: &h.stderr, Stdout: &h.stdout,
	})
	if code != ExitError || !strings.Contains(h.stderr.String(), "no credential configuration") {
		t.Errorf("config flag only: exit %d: %s", code, h.stderr.String())
	}
	// A missing file named by flag is reported with its path.
	h.stdout.Reset()
	h.stderr.Reset()
	code = Run(Options{
		Args:   []string{"-credentials-file", filepath.Join(dir, "absent"), "-config-file", filepath.Join(dir, "absent"), "-no-stdin", "-resolver", "native", "x"},
		Stderr: &h.stderr, Stdout: &h.stdout,
	})
	if code != ExitError || !strings.Contains(h.stderr.String(), filepath.Join(dir, "absent")) {
		t.Errorf("missing files: exit %d: %s", code, h.stderr.String())
	}
}

func TestRealConfigFilesThroughEnv(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config")
	os.WriteFile(cfgPath, []byte("[profile x]\nregion = us-east-2\naws_access_key_id = ASIA\naws_secret_access_key = s\naws_session_token = t\n"), 0o600)
	h := newHarness(t, "", "")
	h.isTTY = true
	h.env["AWS_CONFIG_FILE"] = cfgPath
	h.env["AWS_SHARED_CREDENTIALS_FILE"] = filepath.Join(dir, "nope")
	h.aws.identityARN = "arn:aws:sts::123456789012:assumed-role/X/y"
	code := Run(Options{
		Args:            []string{"-sts-endpoint", h.srv.URL + "/sts", "-signin-endpoint", h.srv.URL + "/federation", "-resolver", "native", "x"},
		Stdin:           strings.NewReader(""),
		StdinIsTerminal: true,
		Stdout:          &h.stdout,
		Stderr:          &h.stderr,
		Env:             func(k string) string { return h.env[k] },
		HTTP:            h.srv.Client(),
	})
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	_, q := parseURL(t, h.stdout.String())
	if q.Get("Destination") != "https://us-east-2.console.aws.amazon.com/console/home?region=us-east-2" {
		t.Errorf("destination = %s", q.Get("Destination"))
	}
}
