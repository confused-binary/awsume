package awsconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleConfig = `# comment
[default]
region = us-east-1
output = json

[profile dev]
region=eu-west-1
role_arn = arn:aws:iam::111111111111:role/Dev
source_profile = default
services = my-services

[profile sso-user]
sso_session = corp
sso_account_id = 222222222222
sso_role_name = ReadOnly

[sso-session corp]
sso_start_url = https://corp.awsapps.com/start
sso_region = us-west-2
sso_registration_scopes = sso:account:access

[services my-services]
sts =
  endpoint_url = https://sts.example.test
s3 =
  endpoint_url = https://s3.example.test
`

const sampleCredentials = `[default]
aws_access_key_id = AKIADEFAULT
aws_secret_access_key = secretdefault

[static]
aws_access_key_id = AKIASTATIC
aws_secret_access_key = secretstatic
aws_session_token = tokenstatic
`

func TestParseNestedAndComments(t *testing.T) {
	f, err := Parse(strings.NewReader(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := f.Section("services my-services")
	if !ok {
		t.Fatal("services section missing")
	}
	if got := s["sts.endpoint_url"]; got != "https://sts.example.test" {
		t.Errorf("nested sts endpoint = %q", got)
	}
	if got := s["s3.endpoint_url"]; got != "https://s3.example.test" {
		t.Errorf("nested s3 endpoint = %q", got)
	}
	if got := f.Sections(); len(got) != 5 || got[0] != "default" {
		t.Errorf("sections = %v", got)
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"key = value\n", "[broken\n", "[s]\nnovalue\n", "[s]\n= v\n"} {
		if _, err := Parse(strings.NewReader(bad)); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(f.Sections()) != 0 {
		t.Fatal("expected empty file")
	}
}

func loadSample(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config")
	credPath := filepath.Join(dir, "credentials")
	if err := os.WriteFile(cfgPath, []byte(sampleConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath, []byte(sampleCredentials), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFiles(cfgPath, credPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestProfileMerge(t *testing.T) {
	cfg := loadSample(t)
	p, ok := cfg.Profile("default")
	if !ok {
		t.Fatal("default profile missing")
	}
	if p.Get("region") != "us-east-1" || p.Get("AWS_ACCESS_KEY_ID") != "AKIADEFAULT" {
		t.Errorf("merged default profile = %v", p.Values)
	}
	dev, ok := cfg.Profile("dev")
	if !ok || dev.Get("role_arn") == "" || dev.Get("region") != "eu-west-1" {
		t.Errorf("dev profile = %v", dev)
	}
	static, ok := cfg.Profile("static")
	if !ok || !static.Has("aws_session_token") {
		t.Errorf("credentials-only profile not found: %v", static)
	}
	if _, ok := cfg.Profile("missing"); ok {
		t.Error("unexpected profile")
	}
	if _, ok := cfg.Profile(""); ok {
		t.Error("empty name must not resolve")
	}
}

func TestProfileNamesSSOAndServices(t *testing.T) {
	cfg := loadSample(t)
	names := cfg.ProfileNames()
	want := []string{"default", "dev", "sso-user", "static"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("ProfileNames = %v, want %v", names, want)
	}
	sess, ok := cfg.SSOSession("corp")
	if !ok || sess["sso_start_url"] != "https://corp.awsapps.com/start" || sess["sso_region"] != "us-west-2" {
		t.Errorf("sso session = %v", sess)
	}
	if got := cfg.ServiceEndpoint("my-services", "STS"); got != "https://sts.example.test" {
		t.Errorf("ServiceEndpoint = %q", got)
	}
	if got := cfg.ServiceEndpoint("my-services", "ec2"); got != "" {
		t.Errorf("unexpected endpoint %q", got)
	}
	if got := cfg.ServiceEndpoint("", "sts"); got != "" {
		t.Errorf("unexpected endpoint %q", got)
	}
}

func TestDefaultPaths(t *testing.T) {
	env := func(k string) string {
		switch k {
		case "AWS_CONFIG_FILE":
			return "/tmp/cfg"
		case "AWS_SHARED_CREDENTIALS_FILE":
			return "~/creds"
		}
		return ""
	}
	cfg, creds := DefaultPaths(env)
	if cfg != "/tmp/cfg" {
		t.Errorf("config path = %q", cfg)
	}
	if strings.HasPrefix(creds, "~") || !strings.HasSuffix(creds, "creds") {
		t.Errorf("credentials path = %q", creds)
	}
	cfg, _ = DefaultPaths(func(k string) string {
		if k == "AWS_SHARED_CONFIG_FILE" {
			return "/tmp/alias"
		}
		return ""
	})
	if cfg != "/tmp/alias" {
		t.Errorf("AWS_SHARED_CONFIG_FILE alias ignored: %q", cfg)
	}
	cfg, _ = DefaultPaths(func(k string) string {
		switch k {
		case "AWS_CONFIG_FILE":
			return "/tmp/official"
		case "AWS_SHARED_CONFIG_FILE":
			return "/tmp/alias"
		}
		return ""
	})
	if cfg != "/tmp/official" {
		t.Errorf("AWS_CONFIG_FILE must win over the alias: %q", cfg)
	}
	cfg, creds = DefaultPaths(func(string) string { return "" })
	if !strings.HasSuffix(cfg, filepath.Join(".aws", "config")) || !strings.HasSuffix(creds, filepath.Join(".aws", "credentials")) {
		t.Errorf("default paths = %q %q", cfg, creds)
	}
}

func TestProfileNamesNaturalOrder(t *testing.T) {
	cfg := newConfigFrom(t, "[profile zeta]\nregion = a\n[profile Beta]\nregion = a\n[default]\nregion = a\n[profile alpha]\nregion = a\n[profile beta]\nregion = a\n[profile acct10]\nregion = a\n[profile acct2]\nregion = a\n", "[Alpha]\naws_access_key_id = x\n[acct1]\naws_access_key_id = x\n")
	got := strings.Join(cfg.ProfileNames(), ",")
	if got != "acct1,acct2,acct10,Alpha,alpha,Beta,beta,default,zeta" {
		t.Errorf("ProfileNames = %s", got)
	}
}

func TestNaturalLess(t *testing.T) {
	ordered := []string{"a", "A1", "a2", "a02", "a10", "a10b", "a10c", "b", "x-9", "x-10", "x-100", "z"}
	for i := 0; i < len(ordered); i++ {
		for j := 0; j < len(ordered); j++ {
			got := NaturalLess(ordered[i], ordered[j])
			if got != (i < j) {
				t.Errorf("NaturalLess(%q, %q) = %v, want %v", ordered[i], ordered[j], got, i < j)
			}
		}
	}
	if NaturalLess("same", "same") || !NaturalLess("Same", "same") {
		t.Error("tiebreak must be total and stable")
	}
}

func newConfigFrom(t *testing.T, config, credentials string) *Config {
	t.Helper()
	c, err := Parse(strings.NewReader(config))
	if err != nil {
		t.Fatal(err)
	}
	cr, err := Parse(strings.NewReader(credentials))
	if err != nil {
		t.Fatal(err)
	}
	return New(c, cr)
}

func TestNewNil(t *testing.T) {
	cfg := New(nil, nil)
	if _, ok := cfg.Profile("default"); ok {
		t.Fatal("empty config should have no profiles")
	}
	if names := cfg.ProfileNames(); len(names) != 0 {
		t.Fatalf("unexpected names %v", names)
	}
}
