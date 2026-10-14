// Package awscreds defines the credential value type shared by every other
// package in the tool. It intentionally has no dependencies.
package awscreds

import "time"

// Credentials is a set of AWS credentials. SessionToken is empty for
// long-lived IAM user or root credentials and non-empty for temporary
// credentials issued by STS, SSO, IMDS or a container credential provider.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Expiration is the zero time when the credentials do not expire or the
	// expiry is unknown.
	Expiration time.Time
	// Source is a short human readable description of where the credentials
	// came from (for example "static", "assume-role", "sso").
	Source string
}

// IsZero reports whether no credentials are set.
func (c Credentials) IsZero() bool {
	return c.AccessKeyID == "" && c.SecretAccessKey == ""
}

// IsTemporary reports whether the credentials carry a session token.
func (c Credentials) IsTemporary() bool {
	return c.SessionToken != ""
}

// Validate returns an error when the credentials are structurally incomplete.
func (c Credentials) Validate() error {
	switch {
	case c.AccessKeyID == "" && c.SecretAccessKey == "":
		return ErrMissing
	case c.AccessKeyID == "":
		return ErrMissingAccessKey
	case c.SecretAccessKey == "":
		return ErrMissingSecretKey
	}
	return nil
}

// Expired reports whether the credentials have an expiry in the past
// relative to now.
func (c Credentials) Expired(now time.Time) bool {
	return !c.Expiration.IsZero() && !now.Before(c.Expiration)
}
