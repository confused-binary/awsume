package sigv4

import (
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"awsume/internal/awscreds"
)

var testCreds = awscreds.Credentials{
	AccessKeyID:     "AKIDEXAMPLE",
	SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
}

var testTime = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

const postFormSignature = "e1dc04bae03b462b450279120fc854bc659e207d00882270d4ea5be0decbd57f"

// TestGetVanilla reproduces the "get-vanilla" case from the AWS SigV4 test
// suite.
func TestGetVanilla(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err := Sign(req, nil, testCreds, "us-east-1", "service", testTime); err != nil {
		t.Fatal(err)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
	}
	if req.Header.Get("X-Amz-Date") != "20150830T123600Z" {
		t.Errorf("X-Amz-Date = %q", req.Header.Get("X-Amz-Date"))
	}
}

// TestPostFormWithToken is a vector computed independently with a reference
// implementation (python hmac/hashlib) for a POST form body with a session
// token.
func TestPostFormWithToken(t *testing.T) {
	body := "Action=GetCallerIdentity&Version=2011-06-15"
	req, _ := http.NewRequest(http.MethodPost, "https://sts.us-east-1.amazonaws.com/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	c := testCreds
	c.SessionToken = "FwoGZXIvYXdzEXAMPLE"
	if err := Sign(req, []byte(body), c, "us-east-1", "sts", testTime); err != nil {
		t.Fatal(err)
	}
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "SignedHeaders=content-type;host;x-amz-date;x-amz-security-token,") {
		t.Errorf("unexpected signed headers in %q", auth)
	}
	if req.Header.Get("X-Amz-Security-Token") != c.SessionToken {
		t.Error("session token header not set")
	}
	const want = "Signature=" + postFormSignature
	if !strings.HasSuffix(auth, want) {
		t.Errorf("Authorization = %q, want suffix %q", auth, want)
	}
}

func TestSigningKeyVector(t *testing.T) {
	// From the AWS "Calculate the signature" documentation example.
	key := SigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20150830", "us-east-1", "iam")
	if got := hex.EncodeToString(key); got != "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9" {
		t.Errorf("SigningKey = %s", got)
	}
}

func TestCanonicalHelpers(t *testing.T) {
	if got := canonicalURI(""); got != "/" {
		t.Errorf("canonicalURI(\"\") = %q", got)
	}
	if got := canonicalURI("/a b/c~d"); got != "/a%20b/c~d" {
		t.Errorf("canonicalURI = %q", got)
	}
	q := url.Values{"b": {"2", "1"}, "a": {"x y"}, "é": {"*"}}
	if got := canonicalQuery(q); got != "%C3%A9=%2A&a=x%20y&b=1&b=2" {
		t.Errorf("canonicalQuery = %q", got)
	}
	h := http.Header{}
	h.Set("X-Amz-Target", "  a   b ")
	h.Set("User-Agent", "ignored")
	canon, signed := canonicalHeaders(h, "example.com")
	if canon != "host:example.com\nx-amz-target:a b\n" || signed != "host;x-amz-target" {
		t.Errorf("canonicalHeaders = %q %q", canon, signed)
	}
}

func TestSignValidation(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err := Sign(req, nil, awscreds.Credentials{}, "us-east-1", "sts", testTime); err == nil {
		t.Error("expected error for empty credentials")
	}
	if err := Sign(req, nil, testCreds, "", "sts", testTime); err == nil {
		t.Error("expected error for empty region")
	}
}
