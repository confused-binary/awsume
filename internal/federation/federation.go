// Package federation talks to the AWS sign-in federation endpoint to turn
// temporary credentials into a console sign-in token and build the final
// login URL. This is the mechanism documented by AWS under "Enabling custom
// identity broker access to the AWS console" and used by pacu,
// aws_consoler and aws-console.
package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/confused-binary/awsume/internal/awscreds"
)

// Session duration limits accepted by the federation endpoint for
// credentials obtained with AssumeRole.
const (
	MinSessionDuration = 900
	MaxSessionDuration = 43200
	maxResponseBytes   = 64 * 1024
)

// Client calls one federation endpoint.
type Client struct {
	// Endpoint is the full federation URL, e.g.
	// https://signin.aws.amazon.com/federation.
	Endpoint string
	// HTTP is the client used for requests; nil uses http.DefaultClient.
	HTTP *http.Client
	// UserAgent is sent with each request when set.
	UserAgent string
}

// New returns a client for the given endpoint.
func New(endpoint string, httpClient *http.Client) *Client {
	return &Client{Endpoint: strings.TrimRight(endpoint, "/"), HTTP: httpClient}
}

// Error is a non-success response from the federation endpoint.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	body := e.Body
	if body == "" {
		body = http.StatusText(e.Status)
	}
	return fmt.Sprintf("federation getSigninToken failed: HTTP %d: %s", e.Status, body)
}

// sessionJSON is the JSON document the endpoint expects in the Session
// parameter.
type sessionJSON struct {
	SessionID    string `json:"sessionId"`
	SessionKey   string `json:"sessionKey"`
	SessionToken string `json:"sessionToken"`
}

// GetSigninToken exchanges temporary credentials for a sign-in token.
// sessionDuration, when greater than zero, is sent as SessionDuration; it is
// only valid for credentials obtained with AssumeRole and must be omitted for
// GetFederationToken credentials.
func (c *Client) GetSigninToken(ctx context.Context, creds awscreds.Credentials, sessionDuration int) (string, error) {
	if c.Endpoint == "" {
		return "", errors.New("federation: endpoint not configured")
	}
	if err := creds.Validate(); err != nil {
		return "", err
	}
	if !creds.IsTemporary() {
		return "", errors.New("federation: the signin endpoint requires temporary credentials with a session token")
	}
	session, err := json.Marshal(sessionJSON{
		SessionID:    creds.AccessKeyID,
		SessionKey:   creds.SecretAccessKey,
		SessionToken: creds.SessionToken,
	})
	if err != nil {
		return "", fmt.Errorf("federation: encode session: %w", err)
	}
	q := url.Values{}
	q.Set("Action", "getSigninToken")
	if sessionDuration > 0 {
		q.Set("SessionDuration", strconv.Itoa(sessionDuration))
	}
	q.Set("Session", string(session))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("federation: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("federation getSigninToken: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("federation getSigninToken: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body := strings.TrimSpace(string(data))
		if len(body) > 300 {
			body = body[:300] + "..."
		}
		return "", &Error{Status: resp.StatusCode, Body: body}
	}
	var out struct {
		SigninToken string `json:"SigninToken"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("federation getSigninToken: decode response: %w", err)
	}
	if out.SigninToken == "" {
		return "", errors.New("federation getSigninToken: response did not contain a SigninToken")
	}
	return out.SigninToken, nil
}

// LoginURL builds the final console sign-in URL from a sign-in token.
func (c *Client) LoginURL(token, issuer, destination string) string {
	q := url.Values{}
	q.Set("Action", "login")
	if issuer != "" {
		q.Set("Issuer", issuer)
	}
	q.Set("Destination", destination)
	q.Set("SigninToken", token)
	return c.Endpoint + "?" + q.Encode()
}
