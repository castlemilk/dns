// Package config holds the `deephost` CLI's on-disk settings and the rule for
// combining them with flags and the environment.
//
// The file lives at ~/.config/deephost/config.json, mode 0600 in a 0700
// directory, and holds the control-plane URL and the operator credential. The
// credential is the single secret this program owns: Config, Resolved and the
// error values here never render it, so a stray %v, a slog attribute or a
// panic cannot spill it into a log.
//
// Precedence is flag > environment (DEEPHOST_API_URL, DEEPHOST_API_TOKEN) > file,
// with a built-in default for the URL only. There is no default credential.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DefaultAPIURL is where a developer's control plane listens, and the only
	// value that needs no configuration at all.
	DefaultAPIURL = "http://127.0.0.1:8080"

	// EnvAPIURL and EnvToken are the two environment variables the CLI reads.
	EnvAPIURL = "DEEPHOST_API_URL"
	EnvToken  = "DEEPHOST_API_TOKEN" //nolint:gosec // the name of a variable, not a credential

	// Redacted is what stands in for the credential wherever a value is
	// rendered.
	Redacted = "[redacted]"

	// DirMode and FileMode are the permissions the credential is kept under.
	DirMode  fs.FileMode = 0o700
	FileMode fs.FileMode = 0o600

	// maxFileBytes caps what Load will read, so a corrupt or hostile file
	// cannot exhaust memory.
	maxFileBytes = 1 << 20

	// maxTokenBytes matches the bound dnsctl already applies to a bearer token.
	maxTokenBytes = 4096
)

// Config is the persisted settings file.
type Config struct {
	APIURL string `json:"api_url,omitempty"`
	Token  string `json:"token,omitempty"`
}

// String renders the config without the credential. It exists so that printing
// a Config by accident is harmless.
func (c Config) String() string {
	token := ""
	if c.Token != "" {
		token = Redacted
	}
	return fmt.Sprintf("config{api_url:%q token:%q}", c.APIURL, token)
}

// LogValue keeps the credential out of structured logs for the same reason.
func (c Config) LogValue() slog.Value {
	token := ""
	if c.Token != "" {
		token = Redacted
	}
	return slog.GroupValue(slog.String("api_url", c.APIURL), slog.String("token", token))
}

var (
	_ fmt.Stringer   = Config{}
	_ slog.LogValuer = Config{}
)

// Origin names where a resolved value came from, so `deephost auth status` can
// tell an operator which of the three inputs is actually in force.
type Origin string

const (
	OriginDefault Origin = "default"
	OriginFile    Origin = "file"
	OriginEnv     Origin = "environment"
	OriginFlag    Origin = "flag"
	OriginUnset   Origin = "unset"
)

// Flags carries the two command-line overrides. An empty string means the flag
// was not given.
type Flags struct {
	APIURL string
	Token  string
}

// Resolved is the effective configuration together with the provenance of each
// field. It embeds Config, so it inherits the redacting String and LogValue.
type Resolved struct {
	Config

	APIURLFrom Origin
	TokenFrom  Origin
}

// Resolve applies flag > environment > file, defaulting only the URL. getenv
// may be nil, in which case the process environment is read. Resolve neither
// validates nor normalises: call Validate on the result when the values are
// about to be used.
func Resolve(flags Flags, getenv func(string) string, stored Config) Resolved {
	if getenv == nil {
		getenv = os.Getenv
	}
	resolved := Resolved{APIURLFrom: OriginDefault, TokenFrom: OriginUnset}
	resolved.APIURL = DefaultAPIURL

	if value := strings.TrimSpace(stored.APIURL); value != "" {
		resolved.APIURL, resolved.APIURLFrom = value, OriginFile
	}
	if value := strings.TrimSpace(getenv(EnvAPIURL)); value != "" {
		resolved.APIURL, resolved.APIURLFrom = value, OriginEnv
	}
	if value := strings.TrimSpace(flags.APIURL); value != "" {
		resolved.APIURL, resolved.APIURLFrom = value, OriginFlag
	}

	// The credential is trimmed but never inspected further here; a value that
	// is present at a higher precedence wins even if it is malformed, so an
	// operator is told about the input they actually supplied.
	if value := strings.TrimSpace(stored.Token); value != "" {
		resolved.Token, resolved.TokenFrom = value, OriginFile
	}
	if value := strings.TrimSpace(getenv(EnvToken)); value != "" {
		resolved.Token, resolved.TokenFrom = value, OriginEnv
	}
	if value := strings.TrimSpace(flags.Token); value != "" {
		resolved.Token, resolved.TokenFrom = value, OriginFlag
	}
	return resolved
}

// Validate checks the resolved URL and, when one is present, the credential.
// It returns the normalised URL. A missing credential is not an error here:
// read-only callers and `deephost auth login` both run without one.
func (r Resolved) Validate() (string, error) {
	base, err := ValidateURL("api_url", r.APIURL)
	if err != nil {
		return "", err
	}
	if r.Token != "" {
		if err := ValidateToken(r.Token); err != nil {
			return "", err
		}
	}
	return base, nil
}

// Path returns the settings file's location. getenv may be nil.
//
// XDG_CONFIG_HOME is honoured because ~/.config is only its default; when it is
// unset the path is exactly ~/.config/deephost/config.json.
func Path(getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	base := strings.TrimSpace(getenv("XDG_CONFIG_HOME"))
	if base == "" {
		home := strings.TrimSpace(getenv("HOME"))
		if home == "" {
			return "", errors.New("HOME: not set, so ~/.config/deephost/config.json cannot be located")
		}
		base = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(base) {
		return "", errors.New("XDG_CONFIG_HOME: must be an absolute path")
	}
	return filepath.Join(base, "deephost", "config.json"), nil
}

// Load reads path. A file that is not there is not an error: it yields the zero
// Config, which is what a first run sees.
//
// A file that other users can read is an error rather than a warning. The file
// holds an operator credential, and continuing would mean spending a secret
// this process knows is exposed.
func Load(path string) (Config, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Config{}, nil
	case err != nil:
		return Config{}, fmt.Errorf("config: %w", err)
	case !info.Mode().IsRegular():
		return Config{}, fmt.Errorf("config: %s is not a regular file", path)
	case info.Mode().Perm()&0o077 != 0:
		return Config{}, fmt.Errorf(
			"config: %s is readable by other users; run chmod 600 %s", path, path)
	case info.Size() > maxFileBytes:
		return Config{}, fmt.Errorf("config: %s exceeds the %d-byte limit", path, maxFileBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	var stored Config
	// Unknown keys are ignored rather than rejected: `deephost` and `deephost-mcp`
	// share this file, so a field one of them learns later must not break the
	// other. The decoder's own message can quote the offending input, which
	// here could be the credential, so it is deliberately dropped.
	if err := json.Unmarshal(raw, &stored); err != nil {
		return Config{}, fmt.Errorf("config: %s is not a valid JSON object", path)
	}
	stored.APIURL = strings.TrimSpace(stored.APIURL)
	stored.Token = strings.TrimSpace(stored.Token)
	return stored, nil
}

// Save writes cfg to path atomically: the directory is created 0700, the file
// is written 0600 under a temporary name and renamed into place, so a reader
// never sees a half-written credential and no window exists in which the file
// is world-readable.
func Save(path string, cfg Config) (resultErr error) {
	if cfg.Token != "" {
		if err := ValidateToken(cfg.Token); err != nil {
			return err
		}
	}
	if cfg.APIURL != "" {
		normalised, err := ValidateURL("api_url", cfg.APIURL)
		if err != nil {
			return err
		}
		cfg.APIURL = normalised
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, DirMode); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	// MkdirAll obeys the umask, so an inherited 0022 would leave 0755 behind.
	if err := os.Chmod(directory, DirMode); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	raw = append(raw, '\n')

	temporary, err := os.CreateTemp(directory, ".config-*.json")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	name := temporary.Name()
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, ignoreMissing(os.Remove(name)))
		}
	}()
	if err := temporary.Chmod(FileMode); err != nil {
		return errors.Join(fmt.Errorf("config: %w", err), temporary.Close())
	}
	if _, err := temporary.Write(raw); err != nil {
		return errors.Join(fmt.Errorf("config: %w", err), temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(fmt.Errorf("config: %w", err), temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// Delete removes the settings file. A file that is already gone is success, so
// `deephost auth logout` is idempotent.
func Delete(path string) error {
	if err := ignoreMissing(os.Remove(path)); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

func ignoreMissing(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ValidateToken checks the credential's shape without ever quoting it. A bearer
// token travels in an HTTP header, so anything outside printable ASCII would
// either be rejected or, worse, split the header.
func ValidateToken(token string) error {
	switch {
	case strings.TrimSpace(token) == "":
		return errors.New("token: is required")
	case len(token) > maxTokenBytes:
		return fmt.Errorf("token: must be at most %d bytes", maxTokenBytes)
	}
	for _, char := range token {
		if char <= 0x20 || char > 0x7e {
			return errors.New("token: must contain only printable ASCII")
		}
	}
	return nil
}

// ValidateURL normalises an absolute HTTP or HTTPS base URL, reporting failures
// as "field: message". Plain HTTP is allowed only against a loopback host,
// because everywhere else it would put the operator credential on the wire in
// clear text.
func ValidateURL(field, raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf(
			"%s: must be an absolute http or https URL with no userinfo, query or fragment", field)
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", fmt.Errorf("%s: must use https unless its host is localhost or a loopback address", field)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
