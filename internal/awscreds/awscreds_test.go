package awscreds

import (
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		c    Credentials
		want error
	}{
		{"empty", Credentials{}, ErrMissing},
		{"no access key", Credentials{SecretAccessKey: "s"}, ErrMissingAccessKey},
		{"no secret", Credentials{AccessKeyID: "a"}, ErrMissingSecretKey},
		{"ok", Credentials{AccessKeyID: "a", SecretAccessKey: "s"}, nil},
	}
	for _, tc := range cases {
		if got := tc.c.Validate(); got != tc.want {
			t.Errorf("%s: Validate() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTemporaryAndExpired(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := Credentials{AccessKeyID: "a", SecretAccessKey: "s"}
	if c.IsTemporary() {
		t.Fatal("credentials without a token must not be temporary")
	}
	if c.Expired(now) {
		t.Fatal("credentials without expiry must never expire")
	}
	c.SessionToken = "tok"
	c.Expiration = now.Add(-time.Second)
	if !c.IsTemporary() || !c.Expired(now) {
		t.Fatal("expected temporary expired credentials")
	}
	c.Expiration = now.Add(time.Second)
	if c.Expired(now) {
		t.Fatal("credentials expiring in the future reported expired")
	}
	if !(Credentials{}).IsZero() {
		t.Fatal("zero credentials not reported as zero")
	}
}
