// Package sts is a minimal AWS Security Token Service client built on the
// Query API (form encoded POST, XML responses) and SigV4, with no
// dependencies outside the standard library.
package sts

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/confused-binary/awsume/internal/awscreds"
	"github.com/confused-binary/awsume/internal/sigv4"
)

const (
	apiVersion  = "2011-06-15"
	serviceName = "sts"
	contentType = "application/x-www-form-urlencoded; charset=utf-8"
	// MaxResponseBytes bounds how much of a response body is read.
	MaxResponseBytes = 1 << 20
)

// Duration limits documented by AWS.
const (
	MinDuration                = 900
	MaxAssumeRoleDuration      = 43200
	MaxRoleChainingDuration    = 3600
	MaxFederationTokenDuration = 129600
	MaxRootFederationDuration  = 3600
)

// Client calls one STS endpoint.
type Client struct {
	// Endpoint is the full URL of the STS endpoint, e.g. https://sts.amazonaws.com.
	Endpoint string
	// Region is the SigV4 signing region.
	Region string
	// HTTP is the client used for requests; nil uses http.DefaultClient.
	HTTP *http.Client
	// Now supplies the signing time; nil uses time.Now.
	Now func() time.Time
	// UserAgent is sent with each request when set.
	UserAgent string
}

// New returns a client for endpoint signing in region.
func New(endpoint, region string, httpClient *http.Client) *Client {
	return &Client{Endpoint: endpoint, Region: region, HTTP: httpClient}
}

// Error is an error response returned by STS.
type Error struct {
	Status  int
	Code    string
	Message string
	// Action is the STS action that failed.
	Action string
}

func (e *Error) Error() string {
	return fmt.Sprintf("sts %s failed: %s (%s, HTTP %d)", e.Action, e.Message, e.Code, e.Status)
}

// IsAuthError reports whether the error indicates that the credentials used
// to call STS are invalid or not permitted, as opposed to a problem with one
// request parameter. Callers use it to decide whether retrying the same
// action with different parameters makes sense.
func (e *Error) IsAuthError() bool {
	switch e.Code {
	case "AccessDenied", "AccessDeniedException", "InvalidClientTokenId", "SignatureDoesNotMatch",
		"ExpiredToken", "ExpiredTokenException", "AuthFailure", "UnrecognizedClientException",
		"IncompleteSignature", "MissingAuthenticationToken", "InvalidIdentityToken", "InvalidAccessKeyId":
		return true
	}
	return e.Status == http.StatusForbidden || e.Status == http.StatusUnauthorized
}

// Identity is the result of GetCallerIdentity.
type Identity struct {
	Account string
	ARN     string
	UserID  string
}

// AssumeRoleInput holds AssumeRole parameters.
type AssumeRoleInput struct {
	RoleARN         string
	SessionName     string
	ExternalID      string
	SerialNumber    string
	TokenCode       string
	DurationSeconds int
	Policy          string
	// SourceIdentity is optional and passed through when set.
	SourceIdentity string
}

// AssumeRoleWithWebIdentityInput holds AssumeRoleWithWebIdentity parameters.
type AssumeRoleWithWebIdentityInput struct {
	RoleARN          string
	SessionName      string
	WebIdentityToken string
	DurationSeconds  int
	Policy           string
}

type credentialsXML struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

func (c credentialsXML) toCredentials(source string) (awscreds.Credentials, error) {
	out := awscreds.Credentials{
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
		SessionToken:    c.SessionToken,
		Source:          source,
	}
	if c.Expiration != "" {
		t, err := time.Parse(time.RFC3339, c.Expiration)
		if err != nil {
			return out, fmt.Errorf("parse credential expiration %q: %w", c.Expiration, err)
		}
		out.Expiration = t
	}
	if err := out.Validate(); err != nil {
		return out, fmt.Errorf("sts returned incomplete credentials: %w", err)
	}
	return out, nil
}

type errorResponse struct {
	Error struct {
		Type    string `xml:"Type"`
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
	RequestID string `xml:"RequestId"`
}

// AssumeRole calls sts:AssumeRole with creds.
func (c *Client) AssumeRole(ctx context.Context, creds awscreds.Credentials, in AssumeRoleInput) (awscreds.Credentials, error) {
	if in.RoleARN == "" {
		return awscreds.Credentials{}, errors.New("sts: AssumeRole requires a role ARN")
	}
	params := url.Values{}
	params.Set("RoleArn", in.RoleARN)
	params.Set("RoleSessionName", defaultSessionName(in.SessionName))
	setOptional(params, "ExternalId", in.ExternalID)
	setOptional(params, "SerialNumber", in.SerialNumber)
	setOptional(params, "TokenCode", in.TokenCode)
	setOptional(params, "Policy", in.Policy)
	setOptional(params, "SourceIdentity", in.SourceIdentity)
	if in.DurationSeconds > 0 {
		params.Set("DurationSeconds", strconv.Itoa(in.DurationSeconds))
	}
	var out struct {
		Result struct {
			Credentials credentialsXML `xml:"Credentials"`
		} `xml:"AssumeRoleResult"`
	}
	if err := c.call(ctx, "AssumeRole", params, &creds, &out); err != nil {
		return awscreds.Credentials{}, err
	}
	return out.Result.Credentials.toCredentials("assume-role")
}

// AssumeRoleWithWebIdentity calls the unsigned AssumeRoleWithWebIdentity
// action.
func (c *Client) AssumeRoleWithWebIdentity(ctx context.Context, in AssumeRoleWithWebIdentityInput) (awscreds.Credentials, error) {
	if in.RoleARN == "" || in.WebIdentityToken == "" {
		return awscreds.Credentials{}, errors.New("sts: AssumeRoleWithWebIdentity requires a role ARN and a token")
	}
	params := url.Values{}
	params.Set("RoleArn", in.RoleARN)
	params.Set("RoleSessionName", defaultSessionName(in.SessionName))
	params.Set("WebIdentityToken", in.WebIdentityToken)
	setOptional(params, "Policy", in.Policy)
	if in.DurationSeconds > 0 {
		params.Set("DurationSeconds", strconv.Itoa(in.DurationSeconds))
	}
	var out struct {
		Result struct {
			Credentials credentialsXML `xml:"Credentials"`
		} `xml:"AssumeRoleWithWebIdentityResult"`
	}
	if err := c.call(ctx, "AssumeRoleWithWebIdentity", params, nil, &out); err != nil {
		return awscreds.Credentials{}, err
	}
	return out.Result.Credentials.toCredentials("web-identity")
}

// GetFederationToken calls sts:GetFederationToken. policy must be an IAM
// policy document; without one the federated user has no permissions.
func (c *Client) GetFederationToken(ctx context.Context, creds awscreds.Credentials, name, policy string, durationSeconds int) (awscreds.Credentials, error) {
	params := url.Values{}
	params.Set("Name", sanitizeName(name))
	setOptional(params, "Policy", policy)
	if durationSeconds > 0 {
		params.Set("DurationSeconds", strconv.Itoa(durationSeconds))
	}
	var out struct {
		Result struct {
			Credentials credentialsXML `xml:"Credentials"`
		} `xml:"GetFederationTokenResult"`
	}
	if err := c.call(ctx, "GetFederationToken", params, &creds, &out); err != nil {
		return awscreds.Credentials{}, err
	}
	return out.Result.Credentials.toCredentials("federation-token")
}

// GetCallerIdentity returns the identity behind creds.
func (c *Client) GetCallerIdentity(ctx context.Context, creds awscreds.Credentials) (Identity, error) {
	var out struct {
		Result struct {
			Account string `xml:"Account"`
			ARN     string `xml:"Arn"`
			UserID  string `xml:"UserId"`
		} `xml:"GetCallerIdentityResult"`
	}
	if err := c.call(ctx, "GetCallerIdentity", url.Values{}, &creds, &out); err != nil {
		return Identity{}, err
	}
	return Identity{Account: out.Result.Account, ARN: out.Result.ARN, UserID: out.Result.UserID}, nil
}

// call performs one Query API request. When creds is nil the request is sent
// unsigned.
func (c *Client) call(ctx context.Context, action string, params url.Values, creds *awscreds.Credentials, out any) error {
	if c.Endpoint == "" {
		return errors.New("sts: endpoint not configured")
	}
	params.Set("Action", action)
	params.Set("Version", apiVersion)
	body := []byte(params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sts: build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/xml")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if creds != nil {
		now := time.Now
		if c.Now != nil {
			now = c.Now
		}
		region := c.Region
		if region == "" {
			region = "us-east-1"
		}
		if err := sigv4.Sign(req, body, *creds, region, serviceName, now()); err != nil {
			return fmt.Errorf("sts: sign %s: %w", action, err)
		}
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sts %s: %w", action, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	if err != nil {
		return fmt.Errorf("sts %s: read response: %w", action, err)
	}
	if resp.StatusCode != http.StatusOK {
		return parseError(action, resp.StatusCode, data)
	}
	if err := xml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("sts %s: decode response: %w", action, err)
	}
	return nil
}

func parseError(action string, status int, data []byte) error {
	var er errorResponse
	if err := xml.Unmarshal(data, &er); err == nil && (er.Error.Code != "" || er.Error.Message != "") {
		return &Error{Status: status, Code: er.Error.Code, Message: er.Error.Message, Action: action}
	}
	msg := strings.TrimSpace(string(data))
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &Error{Status: status, Code: "HTTP" + strconv.Itoa(status), Message: msg, Action: action}
}

func setOptional(v url.Values, key, value string) {
	if value != "" {
		v.Set(key, value)
	}
}

func defaultSessionName(name string) string {
	if name == "" {
		return "awsume-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	return sanitizeName(name)
}

// sanitizeName restricts a name to the character set AWS accepts for
// session and federated user names and the documented length limits.
func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '=', r == ',', r == '.', r == '@', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := b.String()
	if s == "" {
		s = "awsume"
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}
