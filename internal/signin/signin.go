// Package signin plans and executes the different ways of turning AWS
// credentials into a console sign-in URL, trying the option with the longest
// console session first and stopping at the first success.
//
// The methods, in the order AWS allows them to be combined:
//
//   - federation-token: long-lived IAM user keys cannot be used with the
//     federation endpoint directly, so sts:GetFederationToken (up to 36 h, or
//     1 h for the root user) is called first and the resulting temporary
//     credentials are exchanged for a sign-in token. The console session
//     lasts as long as the federation token. This is what pacu and
//     aws_consoler do for IAM user keys.
//   - assume-role: credentials obtained with sts:AssumeRole can be exchanged
//     for a sign-in token with a SessionDuration of up to 12 h, independent
//     of the credential lifetime. When a role ARN is supplied the tool
//     assumes it and uses this method.
//   - direct: temporary credentials that are already role credentials (an
//     assume-role profile, SSO role credentials, instance or container
//     credentials) are sent to the federation endpoint directly, first with
//     SessionDuration values and finally without one (session equals the
//     credential lifetime). This is the method used by the aws-console tool
//     and by the aws CLI export-credentials shell snippet.
//   - sso-portal: for IAM Identity Center profiles, a deep link into the
//     access portal that opens the console if the browser already has a
//     portal session. It needs no API call and is used as the last resort.
package signin

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/confused-binary/awsume/internal/awscreds"
	"github.com/confused-binary/awsume/internal/federation"
	"github.com/confused-binary/awsume/internal/sts"
)

// Method names.
const (
	MethodFederationToken = "federation-token"
	MethodAssumeRole      = "assume-role"
	MethodDirect          = "direct"
	MethodSSOPortal       = "sso-portal"
)

// Kind classifies the principal behind a set of credentials.
type Kind int

const (
	// KindUnknown means the identity could not be determined.
	KindUnknown Kind = iota
	// KindUser is an IAM user with long-lived keys.
	KindUser
	// KindRoot is the account root user with long-lived keys.
	KindRoot
	// KindUserSession is an IAM user with sts:GetSessionToken credentials.
	KindUserSession
	// KindRole is an assumed role (AssumeRole, SSO, instance, container).
	KindRole
	// KindFederatedUser is a GetFederationToken principal.
	KindFederatedUser
)

func (k Kind) String() string {
	switch k {
	case KindUser:
		return "iam-user"
	case KindRoot:
		return "root"
	case KindUserSession:
		return "iam-user-session"
	case KindRole:
		return "assumed-role"
	case KindFederatedUser:
		return "federated-user"
	}
	return "unknown"
}

// Classify derives the Kind from a caller identity ARN and whether the
// credentials carry a session token. An empty ARN yields a guess based on
// the token alone.
func Classify(arn string, hasToken bool) Kind {
	resource := ""
	if parts := strings.SplitN(arn, ":", 6); len(parts) == 6 {
		resource = parts[5]
	}
	switch {
	case resource == "root":
		return KindRoot
	case strings.HasPrefix(resource, "user/"):
		if hasToken {
			return KindUserSession
		}
		return KindUser
	case strings.HasPrefix(resource, "assumed-role/"):
		return KindRole
	case strings.HasPrefix(resource, "federated-user/"):
		return KindFederatedUser
	case arn == "":
		if hasToken {
			return KindUnknown
		}
		return KindUser
	}
	return KindUnknown
}

// DefaultPolicy grants the federated user full access; GetFederationToken
// returns credentials with no permissions when no policy is supplied.
const DefaultPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`

// Duration ladders, longest first. Each value is attempted in turn so that
// the first success yields the longest session the identity permits.
var (
	federationTokenLadder = []int{sts.MaxFederationTokenDuration, 43200, 14400, 3600, 900}
	sessionDurationLadder = []int{federation.MaxSessionDuration, 14400, 3600, 900}
)

// STSAPI is the subset of the STS client used here.
type STSAPI interface {
	GetFederationToken(ctx context.Context, creds awscreds.Credentials, name, policy string, durationSeconds int) (awscreds.Credentials, error)
	AssumeRole(ctx context.Context, creds awscreds.Credentials, in sts.AssumeRoleInput) (awscreds.Credentials, error)
}

// FederationAPI is the subset of the federation client used here.
type FederationAPI interface {
	GetSigninToken(ctx context.Context, creds awscreds.Credentials, sessionDuration int) (string, error)
	LoginURL(token, issuer, destination string) string
}

// SSOPortal describes the IAM Identity Center portal link inputs.
type SSOPortal struct {
	StartURL  string
	AccountID string
	RoleName  string
}

// Options configures a sign-in attempt.
type Options struct {
	Credentials awscreds.Credentials
	Kind        Kind
	STS         STSAPI
	Federation  FederationAPI
	// Issuer is placed in the login URL (shown in the console as the
	// identity broker).
	Issuer string
	// Destination is the console URL to land on.
	Destination string
	// RoleARN, when set, is assumed before federating.
	RoleARN      string
	ExternalID   string
	SessionName  string
	MFASerial    string
	MFAToken     string
	RoleDuration int
	// MaxDuration caps every duration ladder (0 means no cap).
	MaxDuration int
	// Policy is the GetFederationToken policy; empty uses DefaultPolicy.
	Policy string
	// FederationName is the GetFederationToken Name; empty uses "awsume".
	FederationName string
	// SSO enables the access portal fallback.
	SSO *SSOPortal
	// Methods, when non-empty, restricts the methods that may be attempted.
	Methods []string
	// Logf receives verbose diagnostics; nil discards them.
	Logf func(format string, args ...any)
	// Now supplies the current time; nil uses time.Now.
	Now func() time.Time
}

// Attempt is one planned way of producing a URL.
type Attempt struct {
	Method string
	// Duration is the console session length in seconds the attempt asks
	// for; 0 means "the lifetime of the credentials"; -1 means unknown and
	// browser-dependent (sso-portal).
	Duration int
}

func (a Attempt) String() string {
	switch {
	case a.Duration > 0:
		return fmt.Sprintf("%s (%s)", a.Method, (time.Duration(a.Duration) * time.Second).String())
	case a.Duration == 0:
		return a.Method + " (credential lifetime)"
	}
	return a.Method
}

// Result is a successful sign-in URL.
type Result struct {
	URL     string
	Method  string
	Attempt Attempt
	// SessionDuration is the requested console session in seconds; 0 when
	// it follows the credential lifetime.
	SessionDuration int
	// Expires is the best known expiry of the console session; zero when
	// unknown.
	Expires time.Time
	// Credentials are the temporary credentials that were federated; empty
	// for sso-portal.
	Credentials awscreds.Credentials
}

// AllFailedError is returned when every attempt fails.
type AllFailedError struct {
	Failures []Failure
}

// Failure records one failed attempt.
type Failure struct {
	Attempt Attempt
	Err     error
}

func (e *AllFailedError) Error() string {
	if len(e.Failures) == 0 {
		return "no sign-in method is applicable to these credentials"
	}
	var b strings.Builder
	b.WriteString("every sign-in method failed:")
	for _, f := range e.Failures {
		b.WriteString("\n  ")
		b.WriteString(f.Attempt.String())
		b.WriteString(": ")
		b.WriteString(f.Err.Error())
	}
	return b.String()
}

func capLadder(ladder []int, max int) []int {
	if max <= 0 {
		return append([]int(nil), ladder...)
	}
	out := make([]int, 0, len(ladder)+1)
	included := false
	for _, d := range ladder {
		if d <= max {
			if d == max {
				included = true
			}
			out = append(out, d)
		}
	}
	if !included && max >= sts.MinDuration {
		out = append([]int{max}, out...)
	}
	return out
}

// Plan returns the attempts for o ordered by expected session length,
// longest first. Equal durations keep the order federation-token,
// assume-role, direct so that methods needing fewer calls run first.
func Plan(o Options) []Attempt {
	hasToken := o.Credentials.IsTemporary()
	hasCreds := o.Credentials.Validate() == nil
	var attempts []Attempt

	if hasCreds && (o.Kind == KindUser || o.Kind == KindRoot || (o.Kind == KindUnknown && !hasToken)) {
		ladder := federationTokenLadder
		if o.Kind == KindRoot {
			ladder = capLadder(ladder, sts.MaxRootFederationDuration)
		}
		for _, d := range capLadder(ladder, o.MaxDuration) {
			attempts = append(attempts, Attempt{Method: MethodFederationToken, Duration: d})
		}
	}
	if hasCreds && o.RoleARN != "" {
		for _, d := range capLadder(sessionDurationLadder, o.MaxDuration) {
			attempts = append(attempts, Attempt{Method: MethodAssumeRole, Duration: d})
		}
		attempts = append(attempts, Attempt{Method: MethodAssumeRole, Duration: 0})
	}
	if hasCreds && hasToken {
		if o.Kind != KindFederatedUser {
			for _, d := range capLadder(sessionDurationLadder, o.MaxDuration) {
				attempts = append(attempts, Attempt{Method: MethodDirect, Duration: d})
			}
		}
		attempts = append(attempts, Attempt{Method: MethodDirect, Duration: 0})
	}
	if o.SSO != nil && o.SSO.StartURL != "" {
		attempts = append(attempts, Attempt{Method: MethodSSOPortal, Duration: -1})
	}

	if len(o.Methods) > 0 {
		allowed := map[string]bool{}
		for _, m := range o.Methods {
			allowed[strings.ToLower(strings.TrimSpace(m))] = true
		}
		filtered := attempts[:0]
		for _, a := range attempts {
			if allowed[a.Method] {
				filtered = append(filtered, a)
			}
		}
		attempts = filtered
	}

	sort.SliceStable(attempts, func(i, j int) bool {
		return attempts[i].Duration > attempts[j].Duration
	})
	return attempts
}

// Execute runs the plan and returns the first successful URL.
func Execute(ctx context.Context, o Options) (*Result, error) {
	attempts := Plan(o)
	if len(attempts) == 0 {
		return nil, &AllFailedError{}
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	if o.Federation == nil {
		return nil, errors.New("signin: federation client is required")
	}

	var (
		failures  []Failure
		dead      = map[string]error{}
		roleCreds awscreds.Credentials
		roleTried bool
		roleErr   error
	)
	for _, a := range attempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err, isDead := dead[a.Method]; isDead {
			failures = append(failures, Failure{Attempt: a, Err: fmt.Errorf("skipped: %w", err)})
			continue
		}
		logf("trying %s", a)
		var (
			res *Result
			err error
		)
		switch a.Method {
		case MethodFederationToken:
			res, err = o.federationToken(ctx, a, now())
			var se *sts.Error
			if errors.As(err, &se) && se.IsAuthError() {
				dead[a.Method] = err
			}
		case MethodAssumeRole:
			if !roleTried {
				roleTried = true
				roleCreds, roleErr = o.assumeRole(ctx)
				if roleErr != nil {
					dead[a.Method] = roleErr
				}
			}
			if roleErr != nil {
				err = roleErr
			} else {
				res, err = o.federate(ctx, roleCreds, a, now())
			}
		case MethodDirect:
			res, err = o.federate(ctx, o.Credentials, a, now())
		case MethodSSOPortal:
			res, err = o.ssoPortal(a)
		default:
			err = fmt.Errorf("unknown method %q", a.Method)
		}
		if err == nil {
			logf("success with %s", a)
			return res, nil
		}
		logf("%s failed: %v", a, err)
		failures = append(failures, Failure{Attempt: a, Err: err})
	}
	return nil, &AllFailedError{Failures: failures}
}

func (o Options) federationToken(ctx context.Context, a Attempt, now time.Time) (*Result, error) {
	if o.STS == nil {
		return nil, errors.New("sts client is required")
	}
	policy := o.Policy
	if policy == "" {
		policy = DefaultPolicy
	}
	name := o.FederationName
	if name == "" {
		name = "awsume"
	}
	creds, err := o.STS.GetFederationToken(ctx, o.Credentials, name, policy, a.Duration)
	if err != nil {
		return nil, err
	}
	token, err := o.Federation.GetSigninToken(ctx, creds, 0)
	if err != nil {
		return nil, err
	}
	expires := creds.Expiration
	if expires.IsZero() {
		expires = now.Add(time.Duration(a.Duration) * time.Second)
	}
	return &Result{
		URL:             o.Federation.LoginURL(token, o.Issuer, o.Destination),
		Method:          a.Method,
		Attempt:         a,
		SessionDuration: a.Duration,
		Expires:         expires,
		Credentials:     creds,
	}, nil
}

func (o Options) assumeRole(ctx context.Context) (awscreds.Credentials, error) {
	if o.STS == nil {
		return awscreds.Credentials{}, errors.New("sts client is required")
	}
	duration := o.RoleDuration
	if duration <= 0 {
		duration = 3600
	}
	return o.STS.AssumeRole(ctx, o.Credentials, sts.AssumeRoleInput{
		RoleARN:         o.RoleARN,
		SessionName:     o.SessionName,
		ExternalID:      o.ExternalID,
		SerialNumber:    o.MFASerial,
		TokenCode:       o.MFAToken,
		DurationSeconds: duration,
	})
}

func (o Options) federate(ctx context.Context, creds awscreds.Credentials, a Attempt, now time.Time) (*Result, error) {
	token, err := o.Federation.GetSigninToken(ctx, creds, a.Duration)
	if err != nil {
		return nil, err
	}
	res := &Result{
		URL:             o.Federation.LoginURL(token, o.Issuer, o.Destination),
		Method:          a.Method,
		Attempt:         a,
		SessionDuration: a.Duration,
		Credentials:     creds,
	}
	if a.Duration > 0 {
		res.Expires = now.Add(time.Duration(a.Duration) * time.Second)
	} else {
		res.Expires = creds.Expiration
	}
	return res, nil
}

func (o Options) ssoPortal(a Attempt) (*Result, error) {
	if o.SSO == nil || o.SSO.StartURL == "" {
		return nil, errors.New("no sso start url")
	}
	q := url.Values{}
	q.Set("account_id", o.SSO.AccountID)
	q.Set("role_name", o.SSO.RoleName)
	if o.Destination != "" {
		q.Set("destination", o.Destination)
	}
	base := strings.TrimRight(o.SSO.StartURL, "/")
	return &Result{
		URL:     base + "/#/console?" + q.Encode(),
		Method:  a.Method,
		Attempt: a,
	}, nil
}
