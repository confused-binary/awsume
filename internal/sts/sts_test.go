package sts

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/confused-binary/awsume/internal/awscreds"
)

var testCreds = awscreds.Credentials{AccessKeyID: "AKIATEST", SecretAccessKey: "secret"}

type capture struct {
	form   url.Values
	header http.Header
}

func newServer(t *testing.T, status int, body string, c *capture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(data))
		if err != nil {
			t.Errorf("bad form body: %v", err)
		}
		c.form = form
		c.header = r.Header.Clone()
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
}

const assumeRoleResponse = `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleResult>
    <AssumedRoleUser><Arn>arn:aws:sts::123456789012:assumed-role/Dev/s</Arn><AssumedRoleId>AROA:s</AssumedRoleId></AssumedRoleUser>
    <Credentials>
      <AccessKeyId>ASIAROLE</AccessKeyId>
      <SecretAccessKey>rolesecret</SecretAccessKey>
      <SessionToken>roletoken</SessionToken>
      <Expiration>2030-01-02T03:04:05Z</Expiration>
    </Credentials>
  </AssumeRoleResult>
</AssumeRoleResponse>`

func TestAssumeRole(t *testing.T) {
	var c capture
	srv := newServer(t, 200, assumeRoleResponse, &c)
	defer srv.Close()
	client := New(srv.URL, "us-east-1", srv.Client())
	client.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	creds, err := client.AssumeRole(context.Background(), testCreds, AssumeRoleInput{
		RoleARN: "arn:aws:iam::123456789012:role/Dev", SessionName: "my session!", ExternalID: "ext",
		SerialNumber: "arn:aws:iam::123456789012:mfa/me", TokenCode: "123456", DurationSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessKeyID != "ASIAROLE" || creds.SessionToken != "roletoken" || creds.Source != "assume-role" {
		t.Errorf("creds = %+v", creds)
	}
	if creds.Expiration.Year() != 2030 {
		t.Errorf("expiration not parsed: %v", creds.Expiration)
	}
	want := map[string]string{
		"Action": "AssumeRole", "Version": apiVersion, "RoleArn": "arn:aws:iam::123456789012:role/Dev",
		"RoleSessionName": "my-session-", "ExternalId": "ext", "SerialNumber": "arn:aws:iam::123456789012:mfa/me",
		"TokenCode": "123456", "DurationSeconds": "3600",
	}
	for k, v := range want {
		if c.form.Get(k) != v {
			t.Errorf("form %s = %q, want %q", k, c.form.Get(k), v)
		}
	}
	auth := c.header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIATEST/20260101/us-east-1/sts/aws4_request") {
		t.Errorf("Authorization = %q", auth)
	}
	if c.header.Get("Content-Type") != contentType {
		t.Errorf("content type = %q", c.header.Get("Content-Type"))
	}
}

func TestAssumeRoleWithWebIdentityUnsigned(t *testing.T) {
	var c capture
	srv := newServer(t, 200, strings.ReplaceAll(assumeRoleResponse, "AssumeRole", "AssumeRoleWithWebIdentity"), &c)
	defer srv.Close()
	client := New(srv.URL, "us-east-1", srv.Client())
	creds, err := client.AssumeRoleWithWebIdentity(context.Background(), AssumeRoleWithWebIdentityInput{
		RoleARN: "arn:aws:iam::123456789012:role/Web", WebIdentityToken: "jwt", DurationSeconds: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	if creds.Source != "web-identity" || creds.AccessKeyID != "ASIAROLE" {
		t.Errorf("creds = %+v", creds)
	}
	if c.header.Get("Authorization") != "" {
		t.Error("web identity call must be unsigned")
	}
	if c.form.Get("WebIdentityToken") != "jwt" || !strings.HasPrefix(c.form.Get("RoleSessionName"), "awsume-") {
		t.Errorf("form = %v", c.form)
	}
	if _, err := client.AssumeRoleWithWebIdentity(context.Background(), AssumeRoleWithWebIdentityInput{}); err == nil {
		t.Error("expected validation error")
	}
}

func TestGetFederationTokenAndCallerIdentity(t *testing.T) {
	var c capture
	srv := newServer(t, 200, `<GetFederationTokenResponse><GetFederationTokenResult><Credentials>
<AccessKeyId>ASIAFED</AccessKeyId><SecretAccessKey>fs</SecretAccessKey><SessionToken>ft</SessionToken><Expiration>2030-01-01T00:00:00Z</Expiration>
</Credentials><FederatedUser><Arn>arn:aws:sts::1:federated-user/x</Arn></FederatedUser></GetFederationTokenResult></GetFederationTokenResponse>`, &c)
	defer srv.Close()
	client := New(srv.URL, "us-east-1", srv.Client())
	creds, err := client.GetFederationToken(context.Background(), testCreds, "name with spaces that is far too long to be accepted", `{"Version":"2012-10-17"}`, 129600)
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessKeyID != "ASIAFED" || creds.Source != "federation-token" {
		t.Errorf("creds = %+v", creds)
	}
	if n := c.form.Get("Name"); len(n) != 32 || strings.Contains(n, " ") {
		t.Errorf("Name = %q", n)
	}
	if c.form.Get("DurationSeconds") != "129600" || c.form.Get("Policy") != `{"Version":"2012-10-17"}` {
		t.Errorf("form = %v", c.form)
	}

	srv2 := newServer(t, 200, `<GetCallerIdentityResponse><GetCallerIdentityResult>
<Arn>arn:aws:iam::123456789012:user/me</Arn><UserId>AIDA</UserId><Account>123456789012</Account>
</GetCallerIdentityResult></GetCallerIdentityResponse>`, &c)
	defer srv2.Close()
	client = New(srv2.URL, "", srv2.Client())
	id, err := client.GetCallerIdentity(context.Background(), testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if id.Account != "123456789012" || id.ARN != "arn:aws:iam::123456789012:user/me" || id.UserID != "AIDA" {
		t.Errorf("identity = %+v", id)
	}
	if !strings.Contains(c.header.Get("Authorization"), "/us-east-1/sts/") {
		t.Errorf("empty region should sign with us-east-1: %q", c.header.Get("Authorization"))
	}
}

func TestErrors(t *testing.T) {
	var c capture
	srv := newServer(t, 403, `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>User is not authorized</Message></Error><RequestId>abc</RequestId></ErrorResponse>`, &c)
	defer srv.Close()
	client := New(srv.URL, "us-east-1", srv.Client())
	_, err := client.GetFederationToken(context.Background(), testCreds, "n", "", 0)
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("expected *Error, got %T %v", err, err)
	}
	if se.Code != "AccessDenied" || se.Status != 403 || !se.IsAuthError() || se.Action != "GetFederationToken" {
		t.Errorf("error = %+v", se)
	}
	if !strings.Contains(se.Error(), "User is not authorized") {
		t.Errorf("Error() = %q", se.Error())
	}

	srv2 := newServer(t, 400, `<ErrorResponse><Error><Code>ValidationError</Code><Message>DurationSeconds too large</Message></Error></ErrorResponse>`, &c)
	defer srv2.Close()
	client = New(srv2.URL, "us-east-1", srv2.Client())
	_, err = client.GetFederationToken(context.Background(), testCreds, "n", "", 129600)
	if !errors.As(err, &se) || se.IsAuthError() || se.Code != "ValidationError" {
		t.Errorf("validation error = %v", err)
	}

	srv3 := newServer(t, 502, "<html>bad gateway</html>", &c)
	defer srv3.Close()
	client = New(srv3.URL, "us-east-1", srv3.Client())
	_, err = client.GetCallerIdentity(context.Background(), testCreds)
	if !errors.As(err, &se) || se.Code != "HTTP502" || !strings.Contains(se.Message, "bad gateway") {
		t.Errorf("non-xml error = %v", err)
	}

	client = New("", "us-east-1", nil)
	if _, err := client.GetCallerIdentity(context.Background(), testCreds); err == nil {
		t.Error("expected error for empty endpoint")
	}
	if _, err := client.AssumeRole(context.Background(), testCreds, AssumeRoleInput{}); err == nil {
		t.Error("expected error for empty role arn")
	}
}

func TestIncompleteCredentials(t *testing.T) {
	var c capture
	srv := newServer(t, 200, `<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>x</AccessKeyId></Credentials></AssumeRoleResult></AssumeRoleResponse>`, &c)
	defer srv.Close()
	client := New(srv.URL, "us-east-1", srv.Client())
	if _, err := client.AssumeRole(context.Background(), testCreds, AssumeRoleInput{RoleARN: "arn:aws:iam::1:role/r"}); err == nil {
		t.Error("expected error for incomplete credentials")
	}
}

func TestSanitizeName(t *testing.T) {
	if got := sanitizeName(""); got != "awsume" {
		t.Errorf("empty = %q", got)
	}
	if got := sanitizeName("ok.name@x,y=z-_"); got != "ok.name@x,y=z-_" {
		t.Errorf("valid chars altered: %q", got)
	}
	if got := sanitizeName("bad/name here"); got != "bad-name-here" {
		t.Errorf("sanitized = %q", got)
	}
}
