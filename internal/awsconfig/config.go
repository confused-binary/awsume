package awsconfig

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Env is the signature of an environment lookup function; os.Getenv
// satisfies it.
type Env func(string) string

// Config is the merged view of the AWS config and credentials files.
type Config struct {
	ConfigPath      string
	CredentialsPath string
	config          *File
	credentials     *File
}

// Profile is a merged profile: values from the credentials file override
// values from the config file, mirroring the AWS CLI.
type Profile struct {
	Name   string
	Values map[string]string
}

// Get returns the value for key (case-insensitive) or "".
func (p *Profile) Get(key string) string {
	if p == nil {
		return ""
	}
	return p.Values[strings.ToLower(key)]
}

// Has reports whether key is set to a non-empty value.
func (p *Profile) Has(key string) bool {
	return p.Get(key) != ""
}

// DefaultPaths returns the config and credentials file paths, honouring
// AWS_CONFIG_FILE (or its alias AWS_SHARED_CONFIG_FILE) and
// AWS_SHARED_CREDENTIALS_FILE.
func DefaultPaths(env Env) (configPath, credentialsPath string) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	configPath = env("AWS_CONFIG_FILE")
	if configPath == "" {
		configPath = env("AWS_SHARED_CONFIG_FILE")
	}
	if configPath == "" {
		configPath = filepath.Join(home, ".aws", "config")
	}
	credentialsPath = env("AWS_SHARED_CREDENTIALS_FILE")
	if credentialsPath == "" {
		credentialsPath = filepath.Join(home, ".aws", "credentials")
	}
	return expandHome(configPath, home), expandHome(credentialsPath, home)
}

func expandHome(path, home string) string {
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// LoadDefault loads the files named by DefaultPaths.
func LoadDefault(env Env) (*Config, error) {
	configPath, credentialsPath := DefaultPaths(env)
	return LoadFiles(configPath, credentialsPath)
}

// LoadFiles loads the given config and credentials files. Either may be
// missing.
func LoadFiles(configPath, credentialsPath string) (*Config, error) {
	cfg, err := Load(configPath)
	if err != nil {
		return nil, err
	}
	creds, err := Load(credentialsPath)
	if err != nil {
		return nil, err
	}
	return &Config{
		ConfigPath:      configPath,
		CredentialsPath: credentialsPath,
		config:          cfg,
		credentials:     creds,
	}, nil
}

// New builds a Config from already parsed files; nil files are treated as
// empty.
func New(config, credentials *File) *Config {
	if config == nil {
		config = NewFile()
	}
	if credentials == nil {
		credentials = NewFile()
	}
	return &Config{config: config, credentials: credentials}
}

func configSectionName(profile string) string {
	if profile == "default" {
		return "default"
	}
	return "profile " + profile
}

// Profile returns the merged profile or false when neither file defines it.
func (c *Config) Profile(name string) (*Profile, bool) {
	if c == nil || name == "" {
		return nil, false
	}
	values := map[string]string{}
	found := false
	if s, ok := c.config.Section(configSectionName(name)); ok {
		found = true
		for k, v := range s {
			values[k] = v
		}
	}
	if s, ok := c.credentials.Section(name); ok {
		found = true
		for k, v := range s {
			values[k] = v
		}
	}
	if !found {
		return nil, false
	}
	return &Profile{Name: name, Values: values}, true
}

// ProfileNames lists every profile defined in either file in natural order
// (case-insensitive, numbers compared by value; see NaturalLess).
func (c *Config) ProfileNames() []string {
	if c == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, s := range c.config.Sections() {
		switch {
		case s == "default":
			seen["default"] = true
		case strings.HasPrefix(s, "profile "):
			seen[strings.TrimSpace(strings.TrimPrefix(s, "profile "))] = true
		}
	}
	for _, s := range c.credentials.Sections() {
		seen[s] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return NaturalLess(names[i], names[j]) })
	return names
}

// SSOSession returns the "[sso-session name]" section from the config file.
func (c *Config) SSOSession(name string) (map[string]string, bool) {
	if c == nil || name == "" {
		return nil, false
	}
	return c.config.Section("sso-session " + name)
}

// ServiceEndpoint returns the endpoint_url configured for service inside the
// "[services name]" section, or "" when none is set. The AWS CLI format is:
//
//	[services my-services]
//	sts =
//	  endpoint_url = https://sts.example.com
func (c *Config) ServiceEndpoint(servicesName, service string) string {
	if c == nil || servicesName == "" {
		return ""
	}
	s, ok := c.config.Section("services " + servicesName)
	if !ok {
		return ""
	}
	return s[strings.ToLower(service)+".endpoint_url"]
}
