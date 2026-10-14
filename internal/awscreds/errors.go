package awscreds

import "errors"

var (
	// ErrMissing is returned when neither key component is present.
	ErrMissing = errors.New("no credentials available")
	// ErrMissingAccessKey is returned when only the secret key is present.
	ErrMissingAccessKey = errors.New("credentials are missing an access key id")
	// ErrMissingSecretKey is returned when only the access key is present.
	ErrMissingSecretKey = errors.New("credentials are missing a secret access key")
)
