// Package endpoints knows the AWS partitions (commercial, GovCloud, China)
// and resolves the STS, federation signin, console and IAM Identity Center
// endpoints the tool talks to. Every endpoint can be overridden explicitly,
// through the AWS_ENDPOINT_URL* environment variables, or through the
// profile's endpoint_url / services configuration.
package endpoints

import (
	"strings"
)

// Partition describes one AWS partition.
type Partition struct {
	// ID is the partition identifier used in ARNs: aws, aws-cn, aws-us-gov.
	ID string
	// DNSSuffix is the suffix for regional service hostnames.
	DNSSuffix string
	// DefaultRegion is used for signing and endpoints when no region is known.
	DefaultRegion string
	// SigninURL is the federation endpoint, including the /federation path.
	SigninURL string
	// ConsoleURL is the console base URL with no trailing slash.
	ConsoleURL string
	// RegionalConsoleHost reports whether the console uses
	// "<region>.console..." host names (only the commercial partition does).
	RegionalConsoleHost bool
	// STSGlobalURL is the legacy global STS endpoint, if the partition has one.
	STSGlobalURL string
}

var partitions = []Partition{
	{
		ID:                  "aws",
		DNSSuffix:           "amazonaws.com",
		DefaultRegion:       "us-east-1",
		SigninURL:           "https://signin.aws.amazon.com/federation",
		ConsoleURL:          "https://console.aws.amazon.com",
		RegionalConsoleHost: true,
		STSGlobalURL:        "https://sts.amazonaws.com",
	},
	{
		ID:            "aws-us-gov",
		DNSSuffix:     "amazonaws.com",
		DefaultRegion: "us-gov-west-1",
		SigninURL:     "https://signin.amazonaws-us-gov.com/federation",
		ConsoleURL:    "https://console.amazonaws-us-gov.com",
	},
	{
		ID:            "aws-cn",
		DNSSuffix:     "amazonaws.com.cn",
		DefaultRegion: "cn-north-1",
		SigninURL:     "https://signin.amazonaws.cn/federation",
		ConsoleURL:    "https://console.amazonaws.cn",
	},
}

// Default returns the commercial partition.
func Default() Partition { return partitions[0] }

// Names lists the known partition identifiers.
func Names() []string {
	out := make([]string, 0, len(partitions))
	for _, p := range partitions {
		out = append(out, p.ID)
	}
	return out
}

// ByName looks a partition up by identifier.
func ByName(name string) (Partition, bool) {
	for _, p := range partitions {
		if p.ID == name {
			return p, true
		}
	}
	return Partition{}, false
}

// ForRegion infers the partition from a region name. Unknown or empty
// regions map to the commercial partition.
func ForRegion(region string) Partition {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return partitions[2]
	case strings.HasPrefix(region, "us-gov-"):
		return partitions[1]
	default:
		return partitions[0]
	}
}

// FromARN extracts the partition from an ARN such as
// "arn:aws-us-gov:iam::123456789012:user/x".
func FromARN(arn string) (Partition, bool) {
	parts := strings.SplitN(arn, ":", 3)
	if len(parts) < 3 || parts[0] != "arn" {
		return Partition{}, false
	}
	return ByName(parts[1])
}

// Overrides holds explicit endpoint overrides supplied by the user. Empty
// values mean "no override".
type Overrides struct {
	STS       string
	Signin    string
	Console   string
	SSOPortal string
	SSOOIDC   string
}

// Resolver computes the concrete endpoints for one profile.
type Resolver struct {
	Partition Partition
	// Region is the region used for STS; may be empty.
	Region    string
	Overrides Overrides
	// Env looks environment variables up; nil disables the AWS_ENDPOINT_URL
	// variables.
	Env func(string) string
	// ProfileEndpoint is the profile's global endpoint_url value, if any.
	ProfileEndpoint string
	// ProfileSTSEndpoint is the sts endpoint from the profile's services
	// section, if any.
	ProfileSTSEndpoint string
	// STSRegionalEndpoints mirrors the sts_regional_endpoints setting
	// ("regional" or "legacy"); empty means regional.
	STSRegionalEndpoints string
}

func (r Resolver) env(key string) string {
	if r.Env == nil {
		return ""
	}
	return r.Env(key)
}

// STS returns the STS endpoint URL and the region to sign requests with.
// Precedence: explicit override, AWS_ENDPOINT_URL_STS, profile services
// section, AWS_ENDPOINT_URL, profile endpoint_url, partition default.
func (r Resolver) STS() (endpoint, signingRegion string) {
	signingRegion = r.Region
	if signingRegion == "" {
		signingRegion = r.Partition.DefaultRegion
	}
	for _, candidate := range []string{
		r.Overrides.STS,
		r.env("AWS_ENDPOINT_URL_STS"),
		r.ProfileSTSEndpoint,
		r.env("AWS_ENDPOINT_URL"),
		r.ProfileEndpoint,
	} {
		if candidate != "" {
			return trimSlash(candidate), signingRegion
		}
	}
	useGlobal := r.STSRegionalEndpoints == "legacy" || r.Region == ""
	if useGlobal && r.Partition.STSGlobalURL != "" {
		return r.Partition.STSGlobalURL, r.Partition.DefaultRegion
	}
	return "https://sts." + signingRegion + "." + r.Partition.DNSSuffix, signingRegion
}

// Signin returns the federation endpoint including the /federation path.
func (r Resolver) Signin() string {
	if r.Overrides.Signin != "" {
		return trimSlash(r.Overrides.Signin)
	}
	return r.Partition.SigninURL
}

// Console returns the console base URL without a trailing slash.
func (r Resolver) Console() string {
	if r.Overrides.Console != "" {
		return trimSlash(r.Overrides.Console)
	}
	return r.Partition.ConsoleURL
}

// SSOPortal returns the IAM Identity Center portal endpoint used by
// GetRoleCredentials for the given SSO region.
func (r Resolver) SSOPortal(ssoRegion string) string {
	if r.Overrides.SSOPortal != "" {
		return trimSlash(r.Overrides.SSOPortal)
	}
	if ssoRegion == "" {
		ssoRegion = r.Partition.DefaultRegion
	}
	return "https://portal.sso." + ssoRegion + "." + ForRegion(ssoRegion).DNSSuffix
}

// SSOOIDC returns the IAM Identity Center OIDC endpoint used to refresh
// access tokens.
func (r Resolver) SSOOIDC(ssoRegion string) string {
	if r.Overrides.SSOOIDC != "" {
		return trimSlash(r.Overrides.SSOOIDC)
	}
	if ssoRegion == "" {
		ssoRegion = r.Partition.DefaultRegion
	}
	return "https://oidc." + ssoRegion + "." + ForRegion(ssoRegion).DNSSuffix
}

func trimSlash(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}
