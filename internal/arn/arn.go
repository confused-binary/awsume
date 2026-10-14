// Package arn parses Amazon Resource Names.
package arn

import (
	"errors"
	"fmt"
	"strings"
)

// ARN is a parsed Amazon Resource Name:
// arn:partition:service:region:account-id:resource.
type ARN struct {
	Partition string
	Service   string
	Region    string
	Account   string
	// Resource is everything after the account id, for example
	// "instance/i-0123456789abcdef0" or "function:my-function:1".
	Resource string
	// String is the original, trimmed ARN.
	String string
}

// ErrInvalid is wrapped by Parse errors.
var ErrInvalid = errors.New("invalid arn")

// Parse validates and splits an ARN.
func Parse(s string) (ARN, error) {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" {
		return ARN{}, fmt.Errorf("%w: %q (want arn:partition:service:region:account:resource)", ErrInvalid, s)
	}
	a := ARN{
		Partition: parts[1],
		Service:   strings.ToLower(parts[2]),
		Region:    parts[3],
		Account:   parts[4],
		Resource:  parts[5],
		String:    s,
	}
	if a.Partition == "" || a.Service == "" || a.Resource == "" {
		return ARN{}, fmt.Errorf("%w: %q has an empty partition, service or resource", ErrInvalid, s)
	}
	return a, nil
}

// Split returns the resource type and identifier. The separator is the
// first "/" or ":" in the resource, whichever comes first; a resource without
// a separator has an empty type.
func (a ARN) Split() (resourceType, id string) {
	slash := strings.IndexByte(a.Resource, '/')
	colon := strings.IndexByte(a.Resource, ':')
	switch {
	case slash < 0 && colon < 0:
		return "", a.Resource
	case slash < 0:
		return a.Resource[:colon], a.Resource[colon+1:]
	case colon < 0 || slash < colon:
		return a.Resource[:slash], a.Resource[slash+1:]
	default:
		return a.Resource[:colon], a.Resource[colon+1:]
	}
}
