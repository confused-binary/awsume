package resolve

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"awsume/internal/awscreds"
	"awsume/internal/endpoints"
)

// ssoCache mirrors the JSON written by "aws sso login" into
// ~/.aws/sso/cache/<sha1>.json.
type ssoCache struct {
	StartURL              string `json:"startUrl,omitempty"`
	Region                string `json:"region,omitempty"`
	AccessToken           string `json:"accessToken,omitempty"`
	ExpiresAt             string `json:"expiresAt,omitempty"`
	ClientID              string `json:"clientId,omitempty"`
	ClientSecret          string `json:"clientSecret,omitempty"`
	RegistrationExpiresAt string `json:"registrationExpiresAt,omitempty"`
	RefreshToken          string `json:"refreshToken,omitempty"`
}

// ErrSSOLoginRequired is wrapped when no usable SSO token is cached.
var ErrSSOLoginRequired = errors.New("sso session is not logged in or has expired")

const ssoExpiryMargin = 60 * time.Second

func (r *Resolver) ssoCacheDir() string {
	if r.SSOCacheDir != "" {
		return r.SSOCacheDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".aws", "sso", "cache")
}

// SSOCachePath returns the token cache file for an sso-session name or a
// legacy start URL.
func SSOCachePath(cacheDir, key string) string {
	sum := sha1.Sum([]byte(key))
	return filepath.Join(cacheDir, hex.EncodeToString(sum[:])+".json")
}

func (r *Resolver) ssoProfile(ctx context.Context, res *Result) (*Result, error) {
	p := res.Profile
	info := &SSOInfo{AccountID: p.Get("sso_account_id"), RoleName: p.Get("sso_role_name")}
	var cacheKey string
	if sess := p.Get("sso_session"); sess != "" {
		section, ok := r.config().SSOSession(sess)
		if !ok {
			return nil, fmt.Errorf("profile %q: sso-session %q is not defined", res.ProfileName, sess)
		}
		info.SessionName = sess
		info.StartURL = section["sso_start_url"]
		info.Region = section["sso_region"]
		cacheKey = sess
	} else {
		info.StartURL = p.Get("sso_start_url")
		info.Region = p.Get("sso_region")
		cacheKey = info.StartURL
	}
	res.SSO = info
	if info.StartURL == "" || info.Region == "" {
		return res, fmt.Errorf("profile %q: sso_start_url and sso_region are required", res.ProfileName)
	}
	if info.AccountID == "" || info.RoleName == "" {
		return res, fmt.Errorf("profile %q: sso_account_id and sso_role_name are required", res.ProfileName)
	}

	token, err := r.ssoAccessToken(ctx, cacheKey, info)
	if err != nil {
		return res, fmt.Errorf("profile %q: %w", res.ProfileName, err)
	}
	creds, err := r.ssoRoleCredentials(ctx, token, info)
	if err != nil {
		return res, fmt.Errorf("profile %q: %w", res.ProfileName, err)
	}
	res.Credentials = creds
	res.Chain = append(res.Chain, fmt.Sprintf("sso role credentials for %s in account %s", info.RoleName, info.AccountID))
	return res, nil
}

func parseSSOTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Older CLI versions wrote "2006-01-02T15:04:05UTC".
	return time.Parse("2006-01-02T15:04:05UTC", s)
}

// ssoAccessToken loads the cached access token, refreshing it through the
// OIDC endpoint when it has expired and a refresh token is available.
func (r *Resolver) ssoAccessToken(ctx context.Context, cacheKey string, info *SSOInfo) (string, error) {
	path := SSOCachePath(r.ssoCacheDir(), cacheKey)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: no token cache at %s; run \"aws sso login\"", ErrSSOLoginRequired, path)
		}
		return "", fmt.Errorf("read sso token cache: %w", err)
	}
	var cache ssoCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return "", fmt.Errorf("decode sso token cache %s: %w", path, err)
	}
	expires, err := parseSSOTime(cache.ExpiresAt)
	if cache.AccessToken != "" && err == nil && r.now().Add(ssoExpiryMargin).Before(expires) {
		return cache.AccessToken, nil
	}
	if cache.RefreshToken == "" || cache.ClientID == "" || cache.ClientSecret == "" {
		return "", fmt.Errorf("%w: cached token expired and cannot be refreshed; run \"aws sso login\"", ErrSSOLoginRequired)
	}
	if cache.RegistrationExpiresAt != "" {
		if regExp, err := parseSSOTime(cache.RegistrationExpiresAt); err == nil && !r.now().Before(regExp) {
			return "", fmt.Errorf("%w: client registration expired; run \"aws sso login\"", ErrSSOLoginRequired)
		}
	}
	r.logf("refreshing expired sso access token for %s", cacheKey)
	region := firstNonEmpty(cache.Region, info.Region)
	refreshed, err := r.ssoRefresh(ctx, region, cache)
	if err != nil {
		return "", fmt.Errorf("%w: refresh failed: %v", ErrSSOLoginRequired, err)
	}
	if updated, err := json.MarshalIndent(refreshed, "", "  "); err == nil {
		if err := os.WriteFile(path, updated, 0o600); err != nil {
			r.logf("could not update sso token cache %s: %v", path, err)
		}
	}
	return refreshed.AccessToken, nil
}

func (r *Resolver) ssoRefresh(ctx context.Context, region string, cache ssoCache) (ssoCache, error) {
	er := endpoints.Resolver{Partition: r.partitionFor(region), Overrides: r.Endpoints}
	body, _ := json.Marshal(map[string]string{
		"clientId":     cache.ClientID,
		"clientSecret": cache.ClientSecret,
		"grantType":    "refresh_token",
		"refreshToken": cache.RefreshToken,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, er.SSOOIDC(region)+"/token", bytes.NewReader(body))
	if err != nil {
		return cache, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return cache, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return cache, err
	}
	if resp.StatusCode != http.StatusOK {
		return cache, fmt.Errorf("sso-oidc CreateToken: HTTP %d: %s", resp.StatusCode, truncate(string(data)))
	}
	var out struct {
		AccessToken  string `json:"accessToken"`
		ExpiresIn    int    `json:"expiresIn"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return cache, fmt.Errorf("sso-oidc CreateToken: decode: %w", err)
	}
	if out.AccessToken == "" {
		return cache, errors.New("sso-oidc CreateToken: no accessToken in response")
	}
	cache.AccessToken = out.AccessToken
	cache.ExpiresAt = r.now().Add(time.Duration(out.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	if out.RefreshToken != "" {
		cache.RefreshToken = out.RefreshToken
	}
	return cache, nil
}

// ssoRoleCredentials calls the IAM Identity Center portal GetRoleCredentials
// API.
func (r *Resolver) ssoRoleCredentials(ctx context.Context, token string, info *SSOInfo) (awscreds.Credentials, error) {
	er := endpoints.Resolver{Partition: r.partitionFor(info.Region), Overrides: r.Endpoints}
	q := url.Values{}
	q.Set("account_id", info.AccountID)
	q.Set("role_name", info.RoleName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, er.SSOPortal(info.Region)+"/federation/credentials?"+q.Encode(), nil)
	if err != nil {
		return awscreds.Credentials{}, err
	}
	req.Header.Set("x-amz-sso_bearer_token", token)
	req.Header.Set("Accept", "application/json")
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return awscreds.Credentials{}, fmt.Errorf("sso GetRoleCredentials: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return awscreds.Credentials{}, fmt.Errorf("sso GetRoleCredentials: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return awscreds.Credentials{}, fmt.Errorf("%w: sso GetRoleCredentials: HTTP 401: %s", ErrSSOLoginRequired, truncate(string(data)))
		}
		return awscreds.Credentials{}, fmt.Errorf("sso GetRoleCredentials: HTTP %d: %s", resp.StatusCode, truncate(string(data)))
	}
	var out struct {
		RoleCredentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      int64  `json:"expiration"`
		} `json:"roleCredentials"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return awscreds.Credentials{}, fmt.Errorf("sso GetRoleCredentials: decode: %w", err)
	}
	creds := awscreds.Credentials{
		AccessKeyID:     out.RoleCredentials.AccessKeyID,
		SecretAccessKey: out.RoleCredentials.SecretAccessKey,
		SessionToken:    out.RoleCredentials.SessionToken,
		Source:          "sso",
	}
	if out.RoleCredentials.Expiration > 0 {
		creds.Expiration = time.UnixMilli(out.RoleCredentials.Expiration).UTC()
	}
	if err := creds.Validate(); err != nil {
		return creds, fmt.Errorf("sso GetRoleCredentials: %w", err)
	}
	return creds, nil
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
