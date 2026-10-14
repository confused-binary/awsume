package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/confused-binary/awsume/internal/awscreds"
)

var tempCreds = awscreds.Credentials{AccessKeyID: "ASIA", SecretAccessKey: "sk", SessionToken: "tok"}

func TestGetSigninToken(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		if r.URL.Path != "/federation" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"SigninToken":"signin-token-value"}`))
	}))
	defer srv.Close()
	c := New(srv.URL+"/federation/", srv.Client())

	token, err := c.GetSigninToken(context.Background(), tempCreds, 43200)
	if err != nil {
		t.Fatal(err)
	}
	if token != "signin-token-value" {
		t.Errorf("token = %q", token)
	}
	if got.Get("Action") != "getSigninToken" || got.Get("SessionDuration") != "43200" {
		t.Errorf("query = %v", got)
	}
	var session map[string]string
	if err := json.Unmarshal([]byte(got.Get("Session")), &session); err != nil {
		t.Fatalf("session json: %v", err)
	}
	if session["sessionId"] != "ASIA" || session["sessionKey"] != "sk" || session["sessionToken"] != "tok" {
		t.Errorf("session = %v", session)
	}

	if _, err := c.GetSigninToken(context.Background(), tempCreds, 0); err != nil {
		t.Fatal(err)
	}
	if _, present := got["SessionDuration"]; present {
		t.Error("SessionDuration must be omitted when zero")
	}
}

func TestGetSigninTokenErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("SessionDuration") {
		case "43200":
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Invalid SessionDuration"))
		case "1":
			w.Write([]byte(`{"nope":true}`))
		case "2":
			w.Write([]byte(`not json`))
		default:
			w.Write([]byte(`{"SigninToken":"ok"}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	_, err := c.GetSigninToken(context.Background(), tempCreds, 43200)
	var fe *Error
	if !errors.As(err, &fe) || fe.Status != 400 || !strings.Contains(fe.Error(), "Invalid SessionDuration") {
		t.Errorf("err = %v", err)
	}
	if _, err := c.GetSigninToken(context.Background(), tempCreds, 1); err == nil {
		t.Error("expected missing token error")
	}
	if _, err := c.GetSigninToken(context.Background(), tempCreds, 2); err == nil {
		t.Error("expected decode error")
	}
	longLived := awscreds.Credentials{AccessKeyID: "AKIA", SecretAccessKey: "sk"}
	if _, err := c.GetSigninToken(context.Background(), longLived, 0); err == nil {
		t.Error("long lived credentials must be rejected before calling the endpoint")
	}
	if _, err := c.GetSigninToken(context.Background(), awscreds.Credentials{}, 0); err == nil {
		t.Error("empty credentials must be rejected")
	}
	if _, err := New("", nil).GetSigninToken(context.Background(), tempCreds, 0); err == nil {
		t.Error("empty endpoint must be rejected")
	}
}

func TestLoginURL(t *testing.T) {
	c := New("https://signin.aws.amazon.com/federation", nil)
	u := c.LoginURL("tok en", "awsume", "https://console.aws.amazon.com/ec2/home?region=us-east-1")
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "signin.aws.amazon.com" || parsed.Path != "/federation" {
		t.Errorf("url = %s", u)
	}
	q := parsed.Query()
	if q.Get("Action") != "login" || q.Get("Issuer") != "awsume" || q.Get("SigninToken") != "tok en" {
		t.Errorf("query = %v", q)
	}
	if q.Get("Destination") != "https://console.aws.amazon.com/ec2/home?region=us-east-1" {
		t.Errorf("destination = %q", q.Get("Destination"))
	}
	if !strings.Contains(u, "Destination=https%3A%2F%2Fconsole.aws.amazon.com%2Fec2%2Fhome%3Fregion%3Dus-east-1") {
		t.Errorf("destination not percent-encoded: %s", u)
	}
	if strings.Contains(c.LoginURL("t", "", "d"), "Issuer") {
		t.Error("empty issuer should be omitted")
	}
}
