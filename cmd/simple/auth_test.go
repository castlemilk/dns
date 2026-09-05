package main

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/simplecli/auth"
	"github.com/castlemilk/dns/internal/simplecli/config"
)

// fakeConsole stands in for the browser and the console's /cli/authorize page.
// It is handed the authorize URL exactly as a browser would be, and answers on
// the loopback callback exactly as the approval screen must.
//
// tamper lets a test corrupt the answer to prove the CLI refuses it.
func fakeConsole(t *testing.T, token string, tamper func(url.Values)) func(string) error {
	t.Helper()
	return func(authorizeURL string) error {
		parsed, err := url.Parse(authorizeURL)
		if err != nil {
			t.Errorf("the authorize URL does not parse: %v", err)
			return err
		}
		if parsed.Path != auth.AuthorizePath {
			t.Errorf("authorize path = %q, want %q", parsed.Path, auth.AuthorizePath)
		}
		query := parsed.Query()
		for _, name := range []string{"callback", "challenge", "state"} {
			if query.Get(name) == "" {
				t.Errorf("the authorize URL has no %s: %s", name, authorizeURL)
			}
		}
		// The verifier never leaves the CLI, and no credential is ever in a URL.
		if query.Get("verifier") != "" {
			t.Error("the authorize URL carries the PKCE verifier")
		}
		if strings.Contains(authorizeURL, token) {
			t.Error("the authorize URL carries the credential")
		}
		body := url.Values{
			"state":     {query.Get("state")},
			"challenge": {query.Get("challenge")},
			"token":     {token},
		}
		if tamper != nil {
			tamper(body)
		}
		response, err := http.Post( //nolint:noctx // a loopback callback in a test
			query.Get("callback"), "application/x-www-form-urlencoded", strings.NewReader(body.Encode()))
		if err != nil {
			t.Errorf("post to the callback: %v", err)
			return err
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Errorf("read the callback response: %v", err)
		}
		return response.Body.Close()
	}
}

// TestAuthLogin drives §3 end to end: PKCE, the loopback callback, the state
// check and the 0600 write. There is no identity provider in it, which is why
// what is stored is the operator credential itself.
func TestAuthLogin(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.openBrowser = fakeConsole(t, testToken, nil)

	h.mustRun("auth", "login")

	stored, err := config.Load(h.configPath())
	if err != nil {
		t.Fatalf("load the stored config: %v", err)
	}
	if stored.Token != testToken {
		t.Fatalf("stored token = %q, want the one the console handed over", stored.Token)
	}
	if stored.APIURL != h.server.URL {
		t.Fatalf("stored api_url = %q, want %q", stored.APIURL, h.server.URL)
	}
	info, err := os.Stat(h.configPath())
	if err != nil {
		t.Fatalf("stat the config: %v", err)
	}
	if got := info.Mode().Perm(); got != fs.FileMode(0o600) {
		t.Fatalf("config mode = %v, want 0600", got)
	}

	// The banner is the branded success, and neither stream carries the
	// credential.
	contains(t, h.out(), "logged in to", "the success report")
	absent(t, h.out(), testToken, "stdout")
	absent(t, h.err(), testToken, "stderr")

	// And the credential now works without any flag or environment variable.
	h.plane.requireToken = testToken
	h.mustRun("domain", "list")
}

// TestAuthLoginRefusals covers every way the handoff can fail. In each, nothing
// is written: a login that could not be verified must leave no credential
// behind.
func TestAuthLoginRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		tamper func(url.Values)
		want   string
	}{
		{
			name:   "a different state",
			tamper: func(body url.Values) { body.Set("state", "somebody-elses-state") },
			want:   "state:",
		},
		{
			name:   "no state at all",
			tamper: func(body url.Values) { body.Del("state") },
			want:   "state:",
		},
		{
			name:   "a challenge that does not match",
			tamper: func(body url.Values) { body.Set("challenge", "not-the-challenge") },
			want:   "verifier:",
		},
		{
			name:   "no proof at all",
			tamper: func(body url.Values) { body.Del("challenge") },
			want:   "verifier:",
		},
		{
			name:   "the human declined",
			tamper: func(body url.Values) { body.Set("error", "access_denied"); body.Del("token") },
			want:   "declined",
		},
		{
			name:   "an unusable credential",
			tamper: func(body url.Values) { body.Set("token", "with a space and a \n") },
			want:   "token:",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.env.openBrowser = fakeConsole(t, testToken, test.tamper)

			if code := h.run("auth", "login"); code != codeError {
				t.Fatalf("exit code = %d, want %d", code, codeError)
			}
			contains(t, h.err(), test.want, "the refusal")
			absent(t, h.err(), testToken, "the refusal")

			if _, err := os.Stat(h.configPath()); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a credential was stored despite the refusal: %v", err)
			}
		})
	}
}

// TestAuthLoginTimeout is the path an operator hits when they walk away: the
// message has to tell them what to do instead.
func TestAuthLoginTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// --no-browser announces the URL and opens nothing, so nothing ever
	// answers the callback.
	if code := h.run("auth", "login", "--no-browser", "--timeout", "50ms"); code != codeError {
		t.Fatalf("exit code = %d, want %d", code, codeError)
	}
	contains(t, h.err(), "--token", "the timeout message")
	contains(t, h.err(), auth.AuthorizePath, "the announced URL")
	if _, err := os.Stat(h.configPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a credential was stored after a timeout: %v", err)
	}
}

// TestAuthLoginUsesTheConsoleURL covers a console served from somewhere other
// than the control plane.
func TestAuthLoginUsesTheConsoleURL(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seen := ""
	h.env.openBrowser = func(authorizeURL string) error {
		seen = authorizeURL
		return errors.New("no browser here")
	}
	h.environ["SIMPLE_CONSOLE_URL"] = "https://console.simple.test"

	if code := h.run("auth", "login", "--timeout", "50ms"); code != codeError {
		t.Fatalf("exit code = %d, want a timeout", code)
	}
	if !strings.HasPrefix(seen, "https://console.simple.test/cli/authorize?") {
		t.Fatalf("the browser was sent to %q", seen)
	}
}

// TestAuthLogoutAndStatus covers the rest of the group, including the thing an
// operator most often gets wrong: logging out while the environment still holds
// a credential.
func TestAuthLogoutAndStatus(t *testing.T) {
	t.Parallel()

	t.Run("logout removes the file", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.env.openBrowser = fakeConsole(t, testToken, nil)
		h.mustRun("auth", "login")

		h.mustRun("auth", "logout")
		if _, err := os.Stat(h.configPath()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the config survived logout: %v", err)
		}
		// Twice is not an error: logging out of nothing is still logged out.
		h.mustRun("auth", "logout")
	})

	t.Run("logout says when the environment still authenticates", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("auth", "logout")
		contains(t, h.out(), config.EnvToken, "the logout note")
	})

	t.Run("status names where each value came from", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("--json", "auth", "status")
		document := h.decode()

		if document["authenticated"] != true {
			t.Errorf("authenticated = %v", document["authenticated"])
		}
		if document["token_from"] != string(config.OriginEnv) {
			t.Errorf("token_from = %v, want %q", document["token_from"], config.OriginEnv)
		}
		if document["api_url_from"] != string(config.OriginEnv) {
			t.Errorf("api_url_from = %v", document["api_url_from"])
		}
		absent(t, h.out(), testToken, "auth status --json")

		h.mustRun("auth", "status")
		contains(t, h.out(), config.Redacted, "auth status")
		absent(t, h.out(), testToken, "auth status")
	})

	t.Run("status --check asks the control plane", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.plane.requireToken = testToken

		h.mustRun("--json", "auth", "status", "--check")
		if h.decode()["accepted"] != true {
			t.Fatalf("a good credential was not accepted: %s", h.out())
		}

		h.environ[config.EnvToken] = "the-wrong-token"
		h.mustRun("--json", "auth", "status", "--check")
		document := h.decode()
		if document["accepted"] != false {
			t.Fatalf("a bad credential was accepted: %s", h.out())
		}
		contains(t, field[string](t, document, "detail"), "refused the credential", "the detail")
		absent(t, h.out(), "the-wrong-token", "auth status --check")
	})
}

// TestAuthToken is the one command allowed to print the credential, and only
// when asked in so many words.
func TestAuthToken(t *testing.T) {
	t.Parallel()

	t.Run("redacted by default", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("auth", "token")
		contains(t, h.out(), config.Redacted, "auth token")
		absent(t, h.out(), testToken, "auth token")
	})

	t.Run("--show prints it and warns", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("auth", "token", "--show")
		if strings.TrimSpace(h.out()) != testToken {
			t.Fatalf("stdout = %q, want just the credential", h.out())
		}
		contains(t, h.err(), "warning:", "the warning")
	})

	t.Run("nothing configured is an error, not an empty line", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		if code := h.run("auth", "token"); code != codeError {
			t.Fatalf("exit code = %d, want %d", code, codeError)
		}
		contains(t, h.err(), "simple auth login", "the advice")
	})
}
