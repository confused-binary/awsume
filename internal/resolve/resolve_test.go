package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"awsume/internal/awsconfig"
	"awsume/internal/endpoints"
)

func TestMain(m *testing.M) {
	if os.Getenv("AWSUME_TEST_HELPER") == "1" {
		helperMain()
		return
	}
	os.Exit(m.Run())
}

// helperMain is executed when the test binary is re-invoked as a
// credential_process or as a fake aws CLI.
func helperMain() {
	args := os.Args[1:]
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	switch {
	case len(args) > 0 && args[0] == "fail":
		fmt.Fprintln(os.Stderr, "helper failure message")
		os.Exit(3)
	case len(args) > 0 && args[0] == "configure":
		profile := ""
		for i, a := range args {
			if a == "--profile" && i+1 < len(args) {
				profile = args[i+1]
			}
		}
		fmt.Printf(`{"Version":1,"AccessKeyId":"ASIACLI-%s","SecretAccessKey":"clisecret","SessionToken":"clitoken","Expiration":"2030-01-01T00:00:00Z"}`, profile)
	default:
		fmt.Print(`{"Version":1,"AccessKeyId":"ASIAPROC","SecretAccessKey":"procsecret","SessionToken":"proctoken","Expiration":"2030-01-01T00:00:00Z"}`)
	}
	os.Exit(0)
}

func helperCommand(extra string) string {
	return os.Args[0] + " -test.run=NONE -- " + extra
}

func newConfig(t *testing.T, config, credentials string) *awsconfig.Config {
	t.Helper()
	cfg, err := awsconfig.Parse(strings.NewReader(config))
	if err != nil {
		t.Fatal(err)
	}
	creds, err := awsconfig.Parse(strings.NewReader(credentials))
	if err != nil {
		t.Fatal(err)
	}
	return awsconfig.New(cfg, creds)
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func stsServer(t *testing.T, handler func(action string, form url.Values, r *http.Request) (int, string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(data))
		status, body := handler(form.Get("Action"), form, r)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
}

func roleResponse(result, keyID string) string {
	return fmt.Sprintf(`<%sResponse><%sResult><Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>s</SecretAccessKey><SessionToken>t</SessionToken><Expiration>2030-01-01T00:00:00Z</Expiration></Credentials></%sResult></%sResponse>`, result, result, keyID, result, result)
}

func TestStaticEnvAndNotFound(t *testing.T) {
	cfg := newConfig(t, "[profile cfgonly]\nregion = us-gov-west-1\n", "[static]\naws_access_key_id = AKIA\naws_secret_access_key = sk\n[broken]\naws_access_key_id = AKIA\n")
	r := &Resolver{Config: cfg, Mode: ModeNative, Env: envMap(map[string]string{"AWS_DEFAULT_REGION": "eu-west-1"})}
	res, err := r.Resolve(context.Background(), "static")
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials.AccessKeyID != "AKIA" || res.Credentials.Source != "static" || res.Region != "eu-west-1" || res.Partition.ID != "aws" {
		t.Errorf("result = %+v", res)
	}
	if _, err := r.Resolve(context.Background(), "broken"); err == nil {
		t.Error("expected error for incomplete static credentials")
	}
	if _, err := r.Resolve(context.Background(), "cfgonly"); err == nil || !strings.Contains(err.Error(), "no credential configuration") {
		t.Errorf("cfgonly err = %v", err)
	}
	var nf *ProfileNotFoundError
	if _, err := r.Resolve(context.Background(), "missing"); !errors.As(err, &nf) || nf.Name != "missing" {
		t.Errorf("missing err = %v", err)
	}

	env := map[string]string{"AWS_ACCESS_KEY_ID": "AKIAENV", "AWS_SECRET_ACCESS_KEY": "sk", "AWS_SESSION_TOKEN": "tok", "AWS_REGION": "cn-north-1", "AWS_CREDENTIAL_EXPIRATION": "2030-01-01T00:00:00Z"}
	r = &Resolver{Config: cfg, Mode: ModeNative, Env: envMap(env)}
	res, err = r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials.AccessKeyID != "AKIAENV" || res.Credentials.SessionToken != "tok" || res.Partition.ID != "aws-cn" || res.Credentials.Expiration.IsZero() {
		t.Errorf("env result = %+v", res)
	}
	r = &Resolver{Config: cfg, Mode: ModeNative, Env: envMap(map[string]string{"AWS_PROFILE": "static"})}
	if res, err := r.Resolve(context.Background(), ""); err != nil || res.ProfileName != "static" {
		t.Errorf("AWS_PROFILE result = %+v err %v", res, err)
	}
	r = &Resolver{Config: cfg, Mode: ModeNative, Partition: "aws-us-gov"}
	if res, err := r.Resolve(context.Background(), "static"); err != nil || res.Partition.ID != "aws-us-gov" {
		t.Errorf("forced partition result = %+v err %v", res, err)
	}
}

func TestAssumeRoleChain(t *testing.T) {
	var calls []url.Values
	var seenAuth []string
	srv := stsServer(t, func(action string, form url.Values, r *http.Request) (int, string) {
		calls = append(calls, form)
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		if action != "AssumeRole" {
			return 400, `<ErrorResponse><Error><Code>InvalidAction</Code><Message>bad</Message></Error></ErrorResponse>`
		}
		return 200, roleResponse("AssumeRole", "ASIA-"+form.Get("RoleSessionName"))
	})
	defer srv.Close()

	cfg := newConfig(t, `
[profile base]
region = us-west-2

[profile mid]
role_arn = arn:aws:iam::111:role/Mid
source_profile = base
role_session_name = midsession
external_id = ext-123
duration_seconds = 7200

[profile top]
role_arn = arn:aws:iam::222:role/Top
source_profile = mid
mfa_serial = arn:aws:iam::111:mfa/me
region = eu-central-1

[profile selfref]
role_arn = arn:aws:iam::333:role/Self
source_profile = selfref
aws_access_key_id = AKIASELF
aws_secret_access_key = sk

[profile loop-a]
role_arn = arn:aws:iam::1:role/A
source_profile = loop-b

[profile loop-b]
role_arn = arn:aws:iam::1:role/B
source_profile = loop-a

[profile badsource]
role_arn = arn:aws:iam::1:role/X
credential_source = Nope

[profile noduration]
role_arn = arn:aws:iam::1:role/X
source_profile = base
duration_seconds = abc
`, "[base]\naws_access_key_id = AKIABASE\naws_secret_access_key = sk\n")

	mfaCalls := 0
	r := &Resolver{
		Config:    cfg,
		Mode:      ModeNative,
		Endpoints: endpoints.Overrides{STS: srv.URL},
		HTTP:      srv.Client(),
		MFAToken: func(serial string) (string, error) {
			mfaCalls++
			if serial != "arn:aws:iam::111:mfa/me" {
				t.Errorf("serial = %s", serial)
			}
			return " 123456 ", nil
		},
	}
	res, err := r.Resolve(context.Background(), "top")
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials.AccessKeyID != "ASIA-awsume-"+res.Credentials.AccessKeyID[len("ASIA-awsume-"):] {
		t.Errorf("top creds = %+v", res.Credentials)
	}
	if len(calls) != 2 || mfaCalls != 1 {
		t.Fatalf("calls = %d mfa = %d", len(calls), mfaCalls)
	}
	mid, top := calls[0], calls[1]
	if mid.Get("RoleArn") != "arn:aws:iam::111:role/Mid" || mid.Get("RoleSessionName") != "midsession" || mid.Get("ExternalId") != "ext-123" || mid.Get("DurationSeconds") != "7200" {
		t.Errorf("mid call = %v", mid)
	}
	if top.Get("RoleArn") != "arn:aws:iam::222:role/Top" || top.Get("SerialNumber") != "arn:aws:iam::111:mfa/me" || top.Get("TokenCode") != "123456" || top.Get("DurationSeconds") != "3600" {
		t.Errorf("top call = %v", top)
	}
	if !strings.Contains(seenAuth[0], "Credential=AKIABASE/") || !strings.Contains(seenAuth[1], "Credential=ASIA-midsession/") {
		t.Errorf("signing credentials = %v", seenAuth)
	}
	if !strings.Contains(seenAuth[0], "/us-west-2/sts/") || !strings.Contains(seenAuth[1], "/eu-central-1/sts/") {
		t.Errorf("signing regions = %v", seenAuth)
	}
	if res.Region != "eu-central-1" || len(res.Chain) != 3 {
		t.Errorf("result = %+v", res)
	}

	calls = nil
	res, err = r.Resolve(context.Background(), "selfref")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !strings.Contains(seenAuth[len(seenAuth)-1], "Credential=AKIASELF/") {
		t.Errorf("self referencing source profile: calls=%d auth=%v", len(calls), seenAuth)
	}

	if _, err := r.Resolve(context.Background(), "loop-a"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("cycle err = %v", err)
	}
	if _, err := r.Resolve(context.Background(), "badsource"); err == nil || !strings.Contains(err.Error(), "unsupported credential_source") {
		t.Errorf("badsource err = %v", err)
	}
	if _, err := r.Resolve(context.Background(), "noduration"); err == nil || !strings.Contains(err.Error(), "duration_seconds") {
		t.Errorf("noduration err = %v", err)
	}
	r.MFAToken = nil
	if _, err := r.Resolve(context.Background(), "top"); err == nil || !strings.Contains(err.Error(), "MFA") {
		t.Errorf("no mfa provider err = %v", err)
	}
}

func TestAssumeRoleSTSError(t *testing.T) {
	srv := stsServer(t, func(action string, form url.Values, r *http.Request) (int, string) {
		return 403, `<ErrorResponse><Error><Code>AccessDenied</Code><Message>not allowed</Message></Error></ErrorResponse>`
	})
	defer srv.Close()
	cfg := newConfig(t, "[profile role]\nrole_arn = arn:aws:iam::1:role/R\ncredential_source = Environment\n", "")
	r := &Resolver{Config: cfg, Mode: ModeNative, Endpoints: endpoints.Overrides{STS: srv.URL}, HTTP: srv.Client(),
		Env: envMap(map[string]string{"AWS_ACCESS_KEY_ID": "AKIA", "AWS_SECRET_ACCESS_KEY": "sk"})}
	_, err := r.Resolve(context.Background(), "role")
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("err = %v", err)
	}
	r.Env = nil
	if _, err := r.Resolve(context.Background(), "role"); err == nil || !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
		t.Errorf("missing env err = %v", err)
	}
}

func TestWebIdentity(t *testing.T) {
	var got url.Values
	srv := stsServer(t, func(action string, form url.Values, r *http.Request) (int, string) {
		got = form
		if r.Header.Get("Authorization") != "" {
			t.Error("web identity must be unsigned")
		}
		return 200, roleResponse("AssumeRoleWithWebIdentity", "ASIAWEB")
	})
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("jwt-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := newConfig(t, "[profile web]\nrole_arn = arn:aws:iam::1:role/Web\nweb_identity_token_file = "+tokenFile+"\nrole_session_name = websess\n", "")
	r := &Resolver{Config: cfg, Mode: ModeNative, Endpoints: endpoints.Overrides{STS: srv.URL}, HTTP: srv.Client()}
	res, err := r.Resolve(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials.AccessKeyID != "ASIAWEB" || got.Get("WebIdentityToken") != "jwt-token" || got.Get("RoleSessionName") != "websess" {
		t.Errorf("res = %+v form = %v", res.Credentials, got)
	}

	// Environment driven web identity for the default profile.
	r = &Resolver{Config: awsconfig.New(nil, nil), Mode: ModeNative, Endpoints: endpoints.Overrides{STS: srv.URL}, HTTP: srv.Client(),
		Env: envMap(map[string]string{"AWS_ROLE_ARN": "arn:aws:iam::1:role/EnvWeb", "AWS_WEB_IDENTITY_TOKEN_FILE": tokenFile, "AWS_EC2_METADATA_DISABLED": "true"})}
	res, err = r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("RoleArn") != "arn:aws:iam::1:role/EnvWeb" || res.Credentials.Source != "web-identity" {
		t.Errorf("env web identity form = %v res = %+v", got, res.Credentials)
	}
}

func TestCredentialProcess(t *testing.T) {
	cfg := newConfig(t, "[profile proc]\ncredential_process = "+helperCommand("ok")+"\n[profile procfail]\ncredential_process = "+helperCommand("fail")+"\n", "")
	os.Setenv("AWSUME_TEST_HELPER", "1")
	defer os.Unsetenv("AWSUME_TEST_HELPER")
	r := &Resolver{Config: cfg, Mode: ModeNative}
	res, err := r.Resolve(context.Background(), "proc")
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials.AccessKeyID != "ASIAPROC" || res.Credentials.SessionToken != "proctoken" || res.Credentials.Expiration.Year() != 2030 || res.Credentials.Source != "credential-process" {
		t.Errorf("creds = %+v", res.Credentials)
	}
	_, err = r.Resolve(context.Background(), "procfail")
	if err == nil || !strings.Contains(err.Error(), "helper failure message") {
		t.Errorf("procfail err = %v", err)
	}
}

func TestParseProcessOutput(t *testing.T) {
	if _, err := parseProcessOutput([]byte(`{"Version":2,"AccessKeyId":"a","SecretAccessKey":"b"}`), "x"); err == nil {
		t.Error("expected version error")
	}
	if _, err := parseProcessOutput([]byte(`{"Version":1,"AccessKeyId":"a"}`), "x"); err == nil {
		t.Error("expected validation error")
	}
	if _, err := parseProcessOutput([]byte(`{"Version":1,"AccessKeyId":"a","SecretAccessKey":"b","Expiration":"soon"}`), "x"); err == nil {
		t.Error("expected expiration error")
	}
	if _, err := parseProcessOutput([]byte(`nope`), "x"); err == nil {
		t.Error("expected json error")
	}
	c, err := parseProcessOutput([]byte(" {\"AccessKeyId\":\"a\",\"SecretAccessKey\":\"b\"}\n"), "x")
	if err != nil || c.AccessKeyID != "a" || c.Source != "x" {
		t.Errorf("creds = %+v err %v", c, err)
	}
}

func TestAWSCLIFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper process invocation differs on windows")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "aws")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nAWSUME_TEST_HELPER=1 exec "+os.Args[0]+" -test.run=NONE -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := newConfig(t, "[profile weird]\nregion = ap-south-1\nlogin_session = something-native-does-not-understand\n", "")
	lookPath := func(file string) (string, error) {
		if file == "aws" {
			return script, nil
		}
		return "", errors.New("not found")
	}

	r := &Resolver{Config: cfg, LookPath: lookPath}
	res, err := r.Resolve(context.Background(), "weird")
	if err != nil {
		t.Fatalf("auto mode should fall back to the aws cli: %v", err)
	}
	if res.Credentials.AccessKeyID != "ASIACLI-weird" || res.Region != "ap-south-1" || res.Credentials.Source != "aws-cli" {
		t.Errorf("res = %+v", res)
	}

	r = &Resolver{Config: cfg, Mode: ModeAWSCLI, LookPath: lookPath}
	if res, err := r.Resolve(context.Background(), "weird"); err != nil || res.Credentials.AccessKeyID != "ASIACLI-weird" {
		t.Errorf("awscli mode res = %+v err %v", res, err)
	}

	r = &Resolver{Config: cfg, Mode: ModeNative, LookPath: lookPath}
	if _, err := r.Resolve(context.Background(), "weird"); err == nil {
		t.Error("native mode must not use the aws cli")
	}

	r = &Resolver{Config: cfg, LookPath: func(string) (string, error) { return "", exec.ErrNotFound }}
	_, err = r.Resolve(context.Background(), "weird")
	if err == nil || strings.Contains(err.Error(), "aws cli fallback") {
		t.Errorf("missing cli should return the native error only: %v", err)
	}
	r = &Resolver{Config: cfg, Mode: ModeAWSCLI, LookPath: func(string) (string, error) { return "", exec.ErrNotFound }}
	if _, err := r.Resolve(context.Background(), "weird"); !errors.Is(err, ErrAWSCLINotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeAuto, "AUTO": ModeAuto, "native": ModeNative, " awscli ": ModeAWSCLI} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q %v", in, got, err)
		}
	}
	if _, err := ParseMode("magic"); err == nil {
		t.Error("expected error")
	}
}

func TestMetadataProviders(t *testing.T) {
	imds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				t.Error("missing ttl header")
			}
			io.WriteString(w, "imds-token")
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
			if r.Header.Get("X-aws-ec2-metadata-token") != "imds-token" {
				t.Error("missing imds token")
			}
			io.WriteString(w, "my-role\n")
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/my-role":
			io.WriteString(w, `{"Code":"Success","AccessKeyId":"ASIAIMDS","SecretAccessKey":"s","Token":"t","Expiration":"2030-01-01T00:00:00Z"}`)
		case r.URL.Path == "/latest/meta-data/placement/region":
			io.WriteString(w, "us-east-2")
		default:
			w.WriteHeader(404)
		}
	}))
	defer imds.Close()
	r := &Resolver{Config: awsconfig.New(nil, nil), Mode: ModeNative, HTTP: imds.Client(),
		Env: envMap(map[string]string{"AWS_EC2_METADATA_SERVICE_ENDPOINT": imds.URL})}
	res, err := r.Resolve(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials.AccessKeyID != "ASIAIMDS" || res.Region != "us-east-2" || res.Credentials.Source != "instance-metadata" {
		t.Errorf("imds res = %+v", res)
	}

	container := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "secret-token" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `{"AccessKeyId":"ASIAECS","SecretAccessKey":"s","Token":"t","Expiration":"2030-01-01T00:00:00Z"}`)
	}))
	defer container.Close()
	r = &Resolver{Config: awsconfig.New(nil, nil), Mode: ModeNative, HTTP: container.Client(),
		Env: envMap(map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": container.URL + "/creds", "AWS_CONTAINER_AUTHORIZATION_TOKEN": "secret-token", "AWS_REGION": "us-west-1"})}
	res, err = r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials.AccessKeyID != "ASIAECS" || res.Region != "us-west-1" {
		t.Errorf("container res = %+v", res)
	}
	r.Env = envMap(map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": container.URL + "/creds"})
	if _, err := r.Resolve(context.Background(), ""); err == nil {
		t.Error("expected 401 failure without token")
	}
	r.Env = envMap(map[string]string{"AWS_EC2_METADATA_DISABLED": "true"})
	if _, err := r.Resolve(context.Background(), ""); err == nil {
		t.Error("expected failure when metadata disabled and nothing else configured")
	}
}

func ssoCacheFile(t *testing.T, dir, key string, cache ssoCache) string {
	t.Helper()
	path := SSOCachePath(dir, key)
	data, _ := json.Marshal(cache)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSSO(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	var portalCalls int
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		portalCalls++
		if r.URL.Path != "/federation/credentials" {
			w.WriteHeader(404)
			return
		}
		switch r.Header.Get("x-amz-sso_bearer_token") {
		case "valid-token", "refreshed-token":
		default:
			w.WriteHeader(401)
			io.WriteString(w, `{"message":"Session token not found or invalid"}`)
			return
		}
		q := r.URL.Query()
		if q.Get("account_id") != "222222222222" || q.Get("role_name") != "ReadOnly" {
			t.Errorf("query = %v", q)
		}
		fmt.Fprintf(w, `{"roleCredentials":{"accessKeyId":"ASIASSO","secretAccessKey":"s","sessionToken":"t","expiration":%d}}`, now.Add(time.Hour).UnixMilli())
	}))
	defer portal.Close()
	oidc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["grantType"] != "refresh_token" || body["refreshToken"] != "refresh-1" || body["clientId"] != "cid" || body["clientSecret"] != "csec" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		io.WriteString(w, `{"accessToken":"refreshed-token","expiresIn":3600,"refreshToken":"refresh-2","tokenType":"Bearer"}`)
	}))
	defer oidc.Close()

	cacheDir := t.TempDir()
	cfg := newConfig(t, `
[profile sso-new]
sso_session = corp
sso_account_id = 222222222222
sso_role_name = ReadOnly
region = us-west-2

[sso-session corp]
sso_start_url = https://corp.awsapps.com/start
sso_region = us-east-1

[profile sso-legacy]
sso_start_url = https://legacy.awsapps.com/start
sso_region = us-east-1
sso_account_id = 222222222222
sso_role_name = ReadOnly

[profile sso-incomplete]
sso_session = corp

[profile sso-nosession]
sso_session = nope
sso_account_id = 1
sso_role_name = r
`, "")
	r := &Resolver{Config: cfg, Mode: ModeNative, SSOCacheDir: cacheDir, HTTP: portal.Client(),
		Endpoints: endpoints.Overrides{SSOPortal: portal.URL, SSOOIDC: oidc.URL}, Now: func() time.Time { return now }}

	// No cache at all: partial result with SSO info and a login-required error.
	res, err := r.Resolve(context.Background(), "sso-new")
	if !errors.Is(err, ErrSSOLoginRequired) {
		t.Fatalf("expected login required, got %v", err)
	}
	if res == nil || res.SSO == nil || res.SSO.StartURL != "https://corp.awsapps.com/start" || res.SSO.AccountID != "222222222222" || res.SSO.RoleName != "ReadOnly" || res.SSO.SessionName != "corp" {
		t.Fatalf("partial result = %+v", res)
	}
	if !res.Credentials.IsZero() {
		t.Error("partial result must not carry credentials")
	}

	// Valid token in the sso-session keyed cache.
	ssoCacheFile(t, cacheDir, "corp", ssoCache{AccessToken: "valid-token", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Region: "us-east-1", StartURL: "https://corp.awsapps.com/start"})
	res, err = r.Resolve(context.Background(), "sso-new")
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials.AccessKeyID != "ASIASSO" || res.Credentials.Source != "sso" || res.Credentials.Expiration.Sub(now) != time.Hour || res.Region != "us-west-2" {
		t.Errorf("sso result = %+v", res)
	}

	// Expired token with refresh token: refreshed via OIDC and cache rewritten.
	path := ssoCacheFile(t, cacheDir, "corp", ssoCache{AccessToken: "old", ExpiresAt: now.Add(-time.Minute).Format(time.RFC3339), Region: "us-east-1",
		ClientID: "cid", ClientSecret: "csec", RefreshToken: "refresh-1", RegistrationExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339)})
	res, err = r.Resolve(context.Background(), "sso-new")
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials.AccessKeyID != "ASIASSO" {
		t.Errorf("refreshed result = %+v", res)
	}
	data, _ := os.ReadFile(path)
	var rewritten ssoCache
	json.Unmarshal(data, &rewritten)
	if rewritten.AccessToken != "refreshed-token" || rewritten.RefreshToken != "refresh-2" || rewritten.ExpiresAt != now.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("rewritten cache = %+v", rewritten)
	}

	// Expired token, no refresh material: login required.
	ssoCacheFile(t, cacheDir, "corp", ssoCache{AccessToken: "old", ExpiresAt: "2020-01-01T00:00:00UTC"})
	if _, err := r.Resolve(context.Background(), "sso-new"); !errors.Is(err, ErrSSOLoginRequired) {
		t.Errorf("expired err = %v", err)
	}
	// Expired registration: login required without calling OIDC.
	ssoCacheFile(t, cacheDir, "corp", ssoCache{AccessToken: "old", ExpiresAt: now.Add(-time.Minute).Format(time.RFC3339),
		ClientID: "cid", ClientSecret: "csec", RefreshToken: "refresh-1", RegistrationExpiresAt: now.Add(-time.Hour).Format(time.RFC3339)})
	if _, err := r.Resolve(context.Background(), "sso-new"); !errors.Is(err, ErrSSOLoginRequired) {
		t.Errorf("expired registration err = %v", err)
	}
	// Refresh rejected by OIDC.
	ssoCacheFile(t, cacheDir, "corp", ssoCache{AccessToken: "old", ExpiresAt: now.Add(-time.Minute).Format(time.RFC3339),
		ClientID: "cid", ClientSecret: "csec", RefreshToken: "bad"})
	if _, err := r.Resolve(context.Background(), "sso-new"); !errors.Is(err, ErrSSOLoginRequired) {
		t.Errorf("refresh rejected err = %v", err)
	}

	// Legacy profile keyed by start URL; portal rejects the token.
	ssoCacheFile(t, cacheDir, "https://legacy.awsapps.com/start", ssoCache{AccessToken: "revoked", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)})
	res, err = r.Resolve(context.Background(), "sso-legacy")
	if !errors.Is(err, ErrSSOLoginRequired) || res == nil || res.SSO == nil || res.SSO.StartURL != "https://legacy.awsapps.com/start" {
		t.Errorf("legacy res = %+v err = %v", res, err)
	}
	ssoCacheFile(t, cacheDir, "https://legacy.awsapps.com/start", ssoCache{AccessToken: "valid-token", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)})
	if res, err := r.Resolve(context.Background(), "sso-legacy"); err != nil || res.Credentials.AccessKeyID != "ASIASSO" {
		t.Errorf("legacy valid res = %+v err = %v", res, err)
	}

	if res, err := r.Resolve(context.Background(), "sso-incomplete"); err == nil || res == nil || res.SSO == nil {
		t.Errorf("incomplete: res = %+v err = %v", res, err)
	}
	if _, err := r.Resolve(context.Background(), "sso-nosession"); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Errorf("nosession err = %v", err)
	}
	if portalCalls == 0 {
		t.Error("portal never called")
	}
}

func TestSSOCachePath(t *testing.T) {
	got := SSOCachePath("cache", "corp")
	want := filepath.Join("cache", "ee0bfd2552fbd840c02cc48b6e823320543c450f.json")
	if got != want {
		t.Errorf("path = %s, want %s", got, want)
	}
}
