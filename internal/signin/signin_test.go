package signin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/confused-binary/awsume/internal/awscreds"
	"github.com/confused-binary/awsume/internal/sts"
)

type fakeSTS struct {
	gftCalls   []int
	gftErr     func(duration int) error
	roleCalls  []sts.AssumeRoleInput
	roleErr    error
	issuedKeys int
}

func (f *fakeSTS) GetFederationToken(_ context.Context, _ awscreds.Credentials, name, policy string, d int) (awscreds.Credentials, error) {
	f.gftCalls = append(f.gftCalls, d)
	if name == "" || policy == "" {
		return awscreds.Credentials{}, errors.New("name and policy required")
	}
	if f.gftErr != nil {
		if err := f.gftErr(d); err != nil {
			return awscreds.Credentials{}, err
		}
	}
	f.issuedKeys++
	return awscreds.Credentials{AccessKeyID: fmt.Sprintf("GFT%d", d), SecretAccessKey: "s", SessionToken: "t", Expiration: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeSTS) AssumeRole(_ context.Context, _ awscreds.Credentials, in sts.AssumeRoleInput) (awscreds.Credentials, error) {
	f.roleCalls = append(f.roleCalls, in)
	if f.roleErr != nil {
		return awscreds.Credentials{}, f.roleErr
	}
	return awscreds.Credentials{AccessKeyID: "ROLE", SecretAccessKey: "s", SessionToken: "t"}, nil
}

type fakeFederation struct {
	calls []call
	// reject returns an error for the given credentials/duration.
	reject func(creds awscreds.Credentials, d int) error
}

type call struct {
	key      string
	duration int
}

func (f *fakeFederation) GetSigninToken(_ context.Context, creds awscreds.Credentials, d int) (string, error) {
	f.calls = append(f.calls, call{creds.AccessKeyID, d})
	if f.reject != nil {
		if err := f.reject(creds, d); err != nil {
			return "", err
		}
	}
	return "token-" + creds.AccessKeyID, nil
}

func (f *fakeFederation) LoginURL(token, issuer, destination string) string {
	return "https://signin.test/federation?Action=login&Issuer=" + issuer + "&Destination=" + destination + "&SigninToken=" + token
}

var (
	userCreds = awscreds.Credentials{AccessKeyID: "AKIA", SecretAccessKey: "s"}
	roleCreds = awscreds.Credentials{AccessKeyID: "ASIA", SecretAccessKey: "s", SessionToken: "t", Expiration: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
)

func methodsOf(attempts []Attempt) string {
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		parts = append(parts, fmt.Sprintf("%s:%d", a.Method, a.Duration))
	}
	return strings.Join(parts, " ")
}

func TestClassify(t *testing.T) {
	cases := []struct {
		arn   string
		token bool
		want  Kind
	}{
		{"arn:aws:iam::123456789012:root", false, KindRoot},
		{"arn:aws:iam::123456789012:user/alice", false, KindUser},
		{"arn:aws:iam::123456789012:user/alice", true, KindUserSession},
		{"arn:aws:sts::123456789012:assumed-role/Dev/sess", true, KindRole},
		{"arn:aws-us-gov:sts::123456789012:federated-user/bob", true, KindFederatedUser},
		{"", false, KindUser},
		{"", true, KindUnknown},
		{"garbage", true, KindUnknown},
	}
	for _, tc := range cases {
		if got := Classify(tc.arn, tc.token); got != tc.want {
			t.Errorf("Classify(%q, %v) = %s, want %s", tc.arn, tc.token, got, tc.want)
		}
	}
	if KindUnknown.String() != "unknown" || KindRole.String() != "assumed-role" {
		t.Error("unexpected String()")
	}
}

func TestPlanOrdering(t *testing.T) {
	// IAM user keys with a role to assume: federation-token 36h first, then
	// interleaved by duration, with the direct method absent (no token).
	got := methodsOf(Plan(Options{Credentials: userCreds, Kind: KindUser, RoleARN: "arn:aws:iam::1:role/R"}))
	want := "federation-token:129600 federation-token:43200 assume-role:43200 federation-token:14400 assume-role:14400 federation-token:3600 assume-role:3600 federation-token:900 assume-role:900 assume-role:0"
	if got != want {
		t.Errorf("user+role plan =\n%s\nwant\n%s", got, want)
	}

	// Root: federation token capped at one hour.
	got = methodsOf(Plan(Options{Credentials: userCreds, Kind: KindRoot}))
	if got != "federation-token:3600 federation-token:900" {
		t.Errorf("root plan = %s", got)
	}

	// Role credentials: direct with SessionDuration ladder, then lifetime.
	got = methodsOf(Plan(Options{Credentials: roleCreds, Kind: KindRole}))
	if got != "direct:43200 direct:14400 direct:3600 direct:900 direct:0" {
		t.Errorf("role plan = %s", got)
	}

	// Federated user credentials: SessionDuration is not allowed.
	got = methodsOf(Plan(Options{Credentials: roleCreds, Kind: KindFederatedUser}))
	if got != "direct:0" {
		t.Errorf("federated plan = %s", got)
	}

	// Unknown kind with a token: no GetFederationToken attempts.
	got = methodsOf(Plan(Options{Credentials: roleCreds, Kind: KindUnknown}))
	if strings.Contains(got, MethodFederationToken) {
		t.Errorf("unknown+token plan must not call GetFederationToken: %s", got)
	}
	// Unknown kind without a token behaves like a user.
	got = methodsOf(Plan(Options{Credentials: userCreds, Kind: KindUnknown}))
	if !strings.HasPrefix(got, "federation-token:129600") || strings.Contains(got, "direct") {
		t.Errorf("unknown no-token plan = %s", got)
	}

	// Max duration cap inserts the cap and drops everything longer.
	got = methodsOf(Plan(Options{Credentials: roleCreds, Kind: KindRole, MaxDuration: 7200}))
	if got != "direct:7200 direct:3600 direct:900 direct:0" {
		t.Errorf("capped plan = %s", got)
	}

	// SSO portal last, and method filtering.
	sso := &SSOPortal{StartURL: "https://x.awsapps.com/start", AccountID: "1", RoleName: "r"}
	got = methodsOf(Plan(Options{Credentials: roleCreds, Kind: KindRole, SSO: sso}))
	if !strings.HasSuffix(got, "direct:0 sso-portal:-1") {
		t.Errorf("sso plan = %s", got)
	}
	got = methodsOf(Plan(Options{Credentials: roleCreds, Kind: KindRole, SSO: sso, Methods: []string{"SSO-Portal"}}))
	if got != "sso-portal:-1" {
		t.Errorf("filtered plan = %s", got)
	}
	// No credentials and no SSO: empty plan.
	if got := Plan(Options{}); len(got) != 0 {
		t.Errorf("empty plan = %v", got)
	}
	// No credentials but SSO: only the portal.
	if got := methodsOf(Plan(Options{SSO: sso})); got != "sso-portal:-1" {
		t.Errorf("sso-only plan = %s", got)
	}
}

func TestExecuteFederationTokenLadder(t *testing.T) {
	stsc := &fakeSTS{gftErr: func(d int) error {
		if d > 3600 {
			return &sts.Error{Code: "ValidationError", Message: "too long", Status: 400}
		}
		return nil
	}}
	fed := &fakeFederation{}
	var logs []string
	res, err := Execute(context.Background(), Options{
		Credentials: userCreds, Kind: KindUser, STS: stsc, Federation: fed,
		Issuer: "awsume", Destination: "https://console.aws.amazon.com/",
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != MethodFederationToken || res.SessionDuration != 3600 || !strings.Contains(res.URL, "SigninToken=token-GFT3600") {
		t.Errorf("res = %+v", res)
	}
	if fmt.Sprint(stsc.gftCalls) != "[129600 43200 14400 3600]" {
		t.Errorf("gft calls = %v", stsc.gftCalls)
	}
	if len(fed.calls) != 1 || fed.calls[0].duration != 0 {
		t.Errorf("federation must be called once without SessionDuration: %v", fed.calls)
	}
	if res.Expires.Year() != 2030 {
		t.Errorf("expires = %v", res.Expires)
	}
	if len(logs) == 0 {
		t.Error("expected verbose logs")
	}
}

func TestExecuteAccessDeniedFallsThroughToRole(t *testing.T) {
	stsc := &fakeSTS{gftErr: func(int) error {
		return &sts.Error{Code: "AccessDenied", Message: "nope", Status: 403}
	}}
	fed := &fakeFederation{reject: func(c awscreds.Credentials, d int) error {
		if d > 3600 {
			return errors.New("session too long for chained role")
		}
		return nil
	}}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	res, err := Execute(context.Background(), Options{
		Credentials: userCreds, Kind: KindUser, STS: stsc, Federation: fed,
		RoleARN: "arn:aws:iam::1:role/R", ExternalID: "x", SessionName: "sess", RoleDuration: 1800,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != MethodAssumeRole || res.SessionDuration != 3600 || res.Credentials.AccessKeyID != "ROLE" {
		t.Errorf("res = %+v", res)
	}
	if len(stsc.gftCalls) != 1 {
		t.Errorf("GetFederationToken must stop after an auth error: %v", stsc.gftCalls)
	}
	if len(stsc.roleCalls) != 1 || stsc.roleCalls[0].ExternalID != "x" || stsc.roleCalls[0].DurationSeconds != 1800 || stsc.roleCalls[0].SessionName != "sess" {
		t.Errorf("role calls = %+v", stsc.roleCalls)
	}
	if fmt.Sprint(fed.calls) != "[{ROLE 43200} {ROLE 14400} {ROLE 3600}]" {
		t.Errorf("federation calls = %v", fed.calls)
	}
	if !res.Expires.Equal(now.Add(time.Hour)) {
		t.Errorf("expires = %v", res.Expires)
	}
}

func TestExecuteDirectAndLifetime(t *testing.T) {
	fed := &fakeFederation{reject: func(c awscreds.Credentials, d int) error {
		if d > 0 {
			return errors.New("SessionDuration not allowed")
		}
		return nil
	}}
	res, err := Execute(context.Background(), Options{Credentials: roleCreds, Kind: KindRole, Federation: fed})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != MethodDirect || res.SessionDuration != 0 || !res.Expires.Equal(roleCreds.Expiration) {
		t.Errorf("res = %+v", res)
	}
	if len(fed.calls) != 5 {
		t.Errorf("calls = %v", fed.calls)
	}
}

func TestExecuteAllFailAndSSOFallback(t *testing.T) {
	fed := &fakeFederation{reject: func(awscreds.Credentials, int) error { return errors.New("boom") }}
	stsc := &fakeSTS{roleErr: errors.New("cannot assume")}
	_, err := Execute(context.Background(), Options{Credentials: roleCreds, Kind: KindRole, Federation: fed, STS: stsc, RoleARN: "arn:aws:iam::1:role/R"})
	var all *AllFailedError
	if !errors.As(err, &all) {
		t.Fatalf("err = %v", err)
	}
	if len(stsc.roleCalls) != 1 {
		t.Errorf("role must be assumed once, got %d", len(stsc.roleCalls))
	}
	skipped := 0
	for _, f := range all.Failures {
		if strings.HasPrefix(f.Err.Error(), "skipped:") {
			skipped++
		}
	}
	if skipped != 4 {
		t.Errorf("expected 4 skipped assume-role attempts, got %d in %v", skipped, all)
	}
	if !strings.Contains(err.Error(), "every sign-in method failed") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("message = %s", err)
	}

	res, err := Execute(context.Background(), Options{
		Credentials: roleCreds, Kind: KindRole, Federation: fed,
		Destination: "https://console.aws.amazon.com/ec2/home?region=us-east-1",
		SSO:         &SSOPortal{StartURL: "https://corp.awsapps.com/start/", AccountID: "123456789012", RoleName: "Admin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://corp.awsapps.com/start/#/console?account_id=123456789012&destination=https%3A%2F%2Fconsole.aws.amazon.com%2Fec2%2Fhome%3Fregion%3Dus-east-1&role_name=Admin"
	if res.URL != want || res.Method != MethodSSOPortal {
		t.Errorf("sso url = %s", res.URL)
	}
}

func TestExecuteEdgeCases(t *testing.T) {
	if _, err := Execute(context.Background(), Options{}); err == nil {
		t.Error("expected error for empty plan")
	}
	if _, err := Execute(context.Background(), Options{Credentials: roleCreds, Kind: KindRole}); err == nil {
		t.Error("expected error without federation client")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Execute(ctx, Options{Credentials: roleCreds, Kind: KindRole, Federation: &fakeFederation{}}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled err = %v", err)
	}
	// Missing STS client for GetFederationToken records a failure rather than panicking.
	_, err := Execute(context.Background(), Options{Credentials: userCreds, Kind: KindUser, Federation: &fakeFederation{}})
	if err == nil || !strings.Contains(err.Error(), "sts client is required") {
		t.Errorf("err = %v", err)
	}
	a := Attempt{Method: MethodDirect, Duration: 3600}
	if a.String() != "direct (1h0m0s)" || (Attempt{Method: MethodDirect}).String() != "direct (credential lifetime)" || (Attempt{Method: MethodSSOPortal, Duration: -1}).String() != "sso-portal" {
		t.Errorf("Attempt.String = %q", a.String())
	}
	if got := capLadder([]int{100, 50}, 10); len(got) != 0 {
		t.Errorf("cap below minimum = %v", got)
	}
}
