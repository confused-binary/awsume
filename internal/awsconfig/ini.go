// Package awsconfig reads the AWS shared configuration and credentials files
// (~/.aws/config and ~/.aws/credentials) using the same conventions as the
// AWS CLI: "[profile name]" sections in the config file, bare "[name]"
// sections in the credentials file, "[sso-session name]" and
// "[services name]" sections, and indented nested keys.
package awsconfig

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// File is a parsed INI style file. Section names keep their case; keys are
// lower-cased. Nested keys (indented "key = value" lines beneath a key whose
// value is empty) are flattened as "parent.child".
type File struct {
	sections map[string]map[string]string
	order    []string
}

// NewFile returns an empty File.
func NewFile() *File {
	return &File{sections: map[string]map[string]string{}}
}

// Load parses the file at path. A missing file or an empty path yields an
// empty File and a nil error because both AWS files are optional.
func Load(path string) (*File, error) {
	if path == "" {
		return NewFile(), nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewFile(), nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	parsed, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return parsed, nil
}

// Parse reads INI content from r.
func Parse(r io.Reader) (*File, error) {
	file := NewFile()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var (
		section   string
		parentKey string
		lineNo    int
	)
	for scanner.Scan() {
		lineNo++
		raw := scanner.Text()
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			if !strings.HasSuffix(trimmed, "]") {
				return nil, fmt.Errorf("line %d: malformed section header %q", lineNo, trimmed)
			}
			section = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			parentKey = ""
			file.ensure(section)
			continue
		}
		if section == "" {
			return nil, fmt.Errorf("line %d: key outside of a section", lineNo)
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key = value", lineNo)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", lineNo)
		}
		indented := line != trimmed && (line[0] == ' ' || line[0] == '\t')
		if indented && parentKey != "" {
			file.sections[section][parentKey+"."+key] = value
			continue
		}
		file.sections[section][key] = value
		if value == "" {
			parentKey = key
		} else {
			parentKey = ""
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return file, nil
}

func (f *File) ensure(section string) {
	if _, ok := f.sections[section]; !ok {
		f.sections[section] = map[string]string{}
		f.order = append(f.order, section)
	}
}

// Section returns a copy of the named section's values.
func (f *File) Section(name string) (map[string]string, bool) {
	if f == nil {
		return nil, false
	}
	s, ok := f.sections[name]
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out, true
}

// Sections returns section names in file order.
func (f *File) Sections() []string {
	if f == nil {
		return nil
	}
	return append([]string(nil), f.order...)
}

// SortedKeys returns the keys of m in sorted order; a small helper used by
// callers that print configuration.
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
