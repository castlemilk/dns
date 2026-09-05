package config_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/simplecli/config"
)

// theSecret is the credential every test writes. No assertion may ever find it
// in a rendered string, so it is distinctive enough to grep for.
const theSecret = "tok_live_do_not_print_me"

func envFunc(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{
			name: "home",
			env:  map[string]string{"HOME": "/home/op"},
			want: "/home/op/.config/simple/config.json",
		},
		{
			name: "xdg overrides home",
			env:  map[string]string{"HOME": "/home/op", "XDG_CONFIG_HOME": "/elsewhere"},
			want: "/elsewhere/simple/config.json",
		},
		{
			name:    "no home",
			env:     map[string]string{},
			wantErr: "HOME: not set",
		},
		{
			name:    "relative xdg",
			env:     map[string]string{"XDG_CONFIG_HOME": "relative"},
			wantErr: "XDG_CONFIG_HOME: must be an absolute path",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := config.Path(envFunc(test.env))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Path error = %v, want one containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Path: %v", err)
			}
			if got != test.want {
				t.Errorf("Path = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSaveWritesTightPermissions(t *testing.T) {
	// Not parallel: it changes the process umask, which is process-wide. A
	// permissive umask is the exact condition that would otherwise leave the
	// credential world-readable, so it is the condition worth testing.
	restore := setUmask(t, 0)

	home := t.TempDir()
	path := filepath.Join(home, ".config", "simple", "config.json")

	if err := config.Save(path, config.Config{APIURL: "https://api.simple.test", Token: theSecret}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	restore()

	file, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if got := file.Mode().Perm(); got != config.FileMode {
		t.Errorf("file mode = %04o, want %04o", got, config.FileMode)
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat directory: %v", err)
	}
	if got := directory.Mode().Perm(); got != config.DirMode {
		t.Errorf("directory mode = %04o, want %04o", got, config.DirMode)
	}

	// The parent ~/.config is created by MkdirAll too and must not be loose
	// either, since the credential's directory sits inside it.
	parent, err := os.Stat(filepath.Join(home, ".config"))
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if got := parent.Mode().Perm() & 0o077; got != 0 {
		t.Errorf("parent mode = %04o, want no group or other bits", parent.Mode().Perm())
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Token != theSecret || loaded.APIURL != "https://api.simple.test" {
		t.Errorf("round trip lost data: %+v", loaded.APIURL)
	}
}

func TestSaveOverwritesAndStaysAtomic(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "simple", "config.json")
	if err := config.Save(path, config.Config{APIURL: "https://one.test", Token: theSecret}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := config.Save(path, config.Config{APIURL: "https://two.test", Token: "second"}); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.APIURL != "https://two.test" || loaded.Token != "second" {
		t.Errorf("overwrite did not take effect")
	}
	// No temporary file may be left behind holding a copy of the credential.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Errorf("directory holds %d entries, want only config.json", len(entries))
	}
}

func TestSaveRejectsBadValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     config.Config
		wantErr string
	}{
		{
			name:    "token with a newline",
			cfg:     config.Config{Token: "abc\ndef"},
			wantErr: "token: must contain only printable ASCII",
		},
		{
			name:    "oversized token",
			cfg:     config.Config{Token: strings.Repeat("a", 4097)},
			wantErr: "token: must be at most 4096 bytes",
		},
		{
			name:    "plain http to a public host",
			cfg:     config.Config{APIURL: "http://api.simple.test"},
			wantErr: "api_url: must use https",
		},
		{
			name:    "url with a query",
			cfg:     config.Config{APIURL: "https://api.simple.test?a=1"},
			wantErr: "api_url: must be an absolute http or https URL",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.json")
			err := config.Save(path, test.cfg)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Save error = %v, want one containing %q", err, test.wantErr)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("a rejected Save still created %s", path)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		write   func(t *testing.T, path string)
		want    config.Config
		wantErr string
	}{
		{
			name:  "missing file is the zero config",
			write: func(*testing.T, string) {},
			want:  config.Config{},
		},
		{
			name: "unknown fields are ignored",
			write: func(t *testing.T, path string) {
				writeFile(t, path, `{"api_url":"https://a.test","token":"t","future_field":1}`, 0o600)
			},
			want: config.Config{APIURL: "https://a.test", Token: "t"},
		},
		{
			name: "values are trimmed",
			write: func(t *testing.T, path string) {
				writeFile(t, path, "{\"api_url\":\" https://a.test \",\"token\":\"\\tt \"}", 0o600)
			},
			want: config.Config{APIURL: "https://a.test", Token: "t"},
		},
		{
			name: "group readable is refused",
			write: func(t *testing.T, path string) {
				writeFile(t, path, `{"token":"t"}`, 0o640)
			},
			wantErr: "readable by other users",
		},
		{
			name: "world readable is refused",
			write: func(t *testing.T, path string) {
				writeFile(t, path, `{"token":"t"}`, 0o644)
			},
			wantErr: "readable by other users",
		},
		{
			name: "malformed json",
			write: func(t *testing.T, path string) {
				writeFile(t, path, `{"token":`+theSecret, 0o600)
			},
			wantErr: "not a valid JSON object",
		},
		{
			name: "a directory is not a config file",
			write: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
			wantErr: "is not a regular file",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.json")
			test.write(t, path)

			got, err := config.Load(path)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Load error = %v, want one containing %q", err, test.wantErr)
				}
				if strings.Contains(err.Error(), theSecret) {
					t.Error("the error message quoted the credential")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got != test.want {
				t.Errorf("Load = %+v, want %+v", got.APIURL, test.want.APIURL)
			}
		})
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Config{Token: theSecret}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	for range 2 {
		if err := config.Delete(path); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Delete left the file behind: %v", err)
	}
}

func TestResolvePrecedence(t *testing.T) {
	t.Parallel()

	stored := config.Config{APIURL: "https://file.test", Token: "file-token"}
	environment := map[string]string{
		config.EnvAPIURL: "https://env.test",
		config.EnvToken:  "env-token",
	}

	tests := []struct {
		name       string
		flags      config.Flags
		env        map[string]string
		stored     config.Config
		wantURL    string
		wantToken  string
		wantURLSrc config.Origin
		wantTokSrc config.Origin
	}{
		{
			name:       "nothing set falls back to the default url and no token",
			wantURL:    config.DefaultAPIURL,
			wantURLSrc: config.OriginDefault,
			wantTokSrc: config.OriginUnset,
		},
		{
			name:       "file only",
			stored:     stored,
			wantURL:    "https://file.test",
			wantToken:  "file-token",
			wantURLSrc: config.OriginFile,
			wantTokSrc: config.OriginFile,
		},
		{
			name:       "environment beats file",
			stored:     stored,
			env:        environment,
			wantURL:    "https://env.test",
			wantToken:  "env-token",
			wantURLSrc: config.OriginEnv,
			wantTokSrc: config.OriginEnv,
		},
		{
			name:       "flag beats environment and file",
			stored:     stored,
			env:        environment,
			flags:      config.Flags{APIURL: "https://flag.test", Token: "flag-token"},
			wantURL:    "https://flag.test",
			wantToken:  "flag-token",
			wantURLSrc: config.OriginFlag,
			wantTokSrc: config.OriginFlag,
		},
		{
			name:       "each field is resolved on its own",
			stored:     stored,
			env:        map[string]string{config.EnvToken: "env-token"},
			flags:      config.Flags{APIURL: "https://flag.test"},
			wantURL:    "https://flag.test",
			wantToken:  "env-token",
			wantURLSrc: config.OriginFlag,
			wantTokSrc: config.OriginEnv,
		},
		{
			name:       "blank and whitespace inputs do not override",
			stored:     stored,
			env:        map[string]string{config.EnvAPIURL: "   ", config.EnvToken: ""},
			flags:      config.Flags{APIURL: "", Token: "  "},
			wantURL:    "https://file.test",
			wantToken:  "file-token",
			wantURLSrc: config.OriginFile,
			wantTokSrc: config.OriginFile,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := config.Resolve(test.flags, envFunc(test.env), test.stored)
			if got.APIURL != test.wantURL {
				t.Errorf("APIURL = %q, want %q", got.APIURL, test.wantURL)
			}
			if got.Token != test.wantToken {
				t.Error("the resolved credential is not the expected one")
			}
			if got.APIURLFrom != test.wantURLSrc {
				t.Errorf("APIURLFrom = %q, want %q", got.APIURLFrom, test.wantURLSrc)
			}
			if got.TokenFrom != test.wantTokSrc {
				t.Errorf("TokenFrom = %q, want %q", got.TokenFrom, test.wantTokSrc)
			}
		})
	}
}

func TestResolvedValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		flags   config.Flags
		want    string
		wantErr string
	}{
		{name: "loopback http is allowed", flags: config.Flags{APIURL: "http://127.0.0.1:8080/"}, want: "http://127.0.0.1:8080"},
		{name: "localhost http is allowed", flags: config.Flags{APIURL: "http://localhost:8080"}, want: "http://localhost:8080"},
		{name: "https anywhere is allowed", flags: config.Flags{APIURL: "https://api.simple.test/"}, want: "https://api.simple.test"},
		{
			name:    "userinfo is refused",
			flags:   config.Flags{APIURL: "https://user:pass@api.simple.test"},
			wantErr: "api_url: must be an absolute http or https URL with no userinfo, query or fragment",
		},
		{
			name:    "a bad token is reported as field: message",
			flags:   config.Flags{APIURL: "https://api.simple.test", Token: "bad token"},
			wantErr: "token: must contain only printable ASCII",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := config.Resolve(test.flags, envFunc(nil), config.Config{}).Validate()
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("Validate error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got != test.want {
				t.Errorf("Validate = %q, want %q", got, test.want)
			}
		})
	}
}

// TestCredentialNeverRenders is the promise the whole package exists to keep.
func TestCredentialNeverRenders(t *testing.T) {
	t.Parallel()

	cfg := config.Config{APIURL: "https://api.simple.test", Token: theSecret}
	resolved := config.Resolve(config.Flags{Token: theSecret}, envFunc(nil), cfg)

	rendered := []string{
		cfg.String(),
		resolved.String(),
		fmtSprint(cfg),
		fmtSprint(resolved),
		slogLine(t, cfg),
		slogLine(t, resolved),
	}
	for _, text := range rendered {
		if strings.Contains(text, theSecret) {
			t.Errorf("a rendering leaked the credential: %s", text)
		}
		if !strings.Contains(text, config.Redacted) {
			t.Errorf("a rendering did not mark the credential as redacted: %s", text)
		}
	}

	// The file itself must hold the credential — that is its job — but nothing
	// else about the package may.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var onDisk map[string]string
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("the saved file is not JSON: %v", err)
	}
	if onDisk["token"] != theSecret {
		t.Error("the saved file did not keep the credential")
	}
}

// fmtSprint goes through the %v, %+v and %s verbs, which is how an accidental
// leak would happen in practice.
func fmtSprint(value any) string { return fmt.Sprintf("%v|%+v|%s", value, value, value) }

func slogLine(t *testing.T, value any) string {
	t.Helper()

	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	logger.Info("configured", "config", value)
	return buffer.String()
}
