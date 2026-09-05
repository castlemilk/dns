package auth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/simplecli/auth"
)

// ignore drops an error a test has nothing better to do with, because this
// repository's errcheck configuration rejects the `_ = f()` form.
func ignore(error) {}

// theSecret is the operator credential the fake console hands back. No test may
// find it anywhere except in the Credential the success path returns.
const theSecret = "tok_live_do_not_print_me"

// vault is the "somewhere" a credential could be stored. Every failing test
// asserts it is still empty, which is the promise the package makes.
type vault struct {
	mu     sync.Mutex
	stored []auth.Credential
}

func (v *vault) store(credential auth.Credential) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stored = append(v.stored, credential)
	return nil
}

func (v *vault) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.stored)
}

func (v *vault) mustBeEmpty(t *testing.T) {
	t.Helper()

	if got := v.count(); got != 0 {
		t.Errorf("%d credentials were stored on a failure path, want 0", got)
	}
}

// browser is a fake console: it reads the authorize URL the CLI opened and
// posts whatever answer the test asked for. It is the only thing that ever
// touches the loopback callback.
type browser struct {
	// answer builds the body from the query the CLI put in the authorize URL.
	answer func(query url.Values) map[string]string

	// contentType selects JSON or form encoding; empty means JSON.
	contentType string

	mu       sync.Mutex
	opened   []string
	response *http.Response
	postErr  error
}

func (b *browser) open(authorizeURL string) error {
	b.mu.Lock()
	b.opened = append(b.opened, authorizeURL)
	b.mu.Unlock()

	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		return err
	}
	query := parsed.Query()
	body := b.answer(query)

	request, err := b.request(query.Get("callback"), body)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	b.mu.Lock()
	b.response, b.postErr = response, err
	b.mu.Unlock()
	if err != nil {
		return err
	}
	defer func() { ignore(response.Body.Close()) }()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		return err
	}
	return nil
}

func (b *browser) request(callback string, body map[string]string) (*http.Request, error) {
	if b.contentType == "application/x-www-form-urlencoded" {
		form := url.Values{}
		for key, value := range body {
			form.Set(key, value)
		}
		request, err := http.NewRequest(http.MethodPost, callback, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", b.contentType)
		return request, nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost, callback, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return request, nil
}

func (b *browser) status() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.response == nil {
		return 0
	}
	return b.response.StatusCode
}

func (b *browser) authorizeURL(t *testing.T) *url.URL {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.opened) != 1 {
		t.Fatalf("the browser was opened %d times, want once", len(b.opened))
	}
	parsed, err := url.Parse(b.opened[0])
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	return parsed
}

func TestPKCE(t *testing.T) {
	t.Parallel()

	pkce, err := auth.NewPKCE(nil)
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	if len(pkce.Verifier) < 43 || len(pkce.Verifier) > 128 {
		t.Errorf("verifier is %d characters, want RFC 7636's 43 to 128", len(pkce.Verifier))
	}
	sum := sha256.Sum256([]byte(pkce.Verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); pkce.Challenge != want {
		t.Errorf("challenge = %q, want the S256 transformation %q", pkce.Challenge, want)
	}
	if strings.ContainsAny(pkce.Verifier, "+/=") {
		t.Errorf("verifier %q is not URL-safe base64", pkce.Verifier)
	}

	// Two draws must differ, or the state and verifier would not bind anything.
	other, err := auth.NewPKCE(nil)
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	if other.Verifier == pkce.Verifier {
		t.Error("two verifiers came out the same")
	}
}

func TestNewPKCEAndStateReportExhaustedRandomness(t *testing.T) {
	t.Parallel()

	if _, err := auth.NewPKCE(strings.NewReader("too short")); err == nil {
		t.Error("NewPKCE accepted a short random source")
	}
	if _, err := auth.NewState(strings.NewReader("")); err == nil {
		t.Error("NewState accepted an empty random source")
	}
}

func TestVerifyChallengeAndState(t *testing.T) {
	t.Parallel()

	pkce, err := auth.NewPKCE(nil)
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}

	tests := []struct {
		name      string
		verifier  string
		challenge string
		want      bool
	}{
		{name: "matching pair", verifier: pkce.Verifier, challenge: pkce.Challenge, want: true},
		{name: "wrong verifier", verifier: "not-it", challenge: pkce.Challenge, want: false},
		{name: "wrong challenge", verifier: pkce.Verifier, challenge: "not-it", want: false},
		{name: "empty verifier", verifier: "", challenge: pkce.Challenge, want: false},
		{name: "empty challenge", verifier: pkce.Verifier, challenge: "", want: false},
		{name: "both empty", verifier: "", challenge: "", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := auth.VerifyChallenge(test.verifier, test.challenge); got != test.want {
				t.Errorf("VerifyChallenge = %v, want %v", got, test.want)
			}
		})
	}

	states := []struct {
		name      string
		got, want string
		expect    bool
	}{
		{name: "equal", got: "abc", want: "abc", expect: true},
		{name: "different", got: "abc", want: "abd", expect: false},
		{name: "empty got", got: "", want: "abc", expect: false},
		{name: "empty want", got: "abc", want: "", expect: false},
	}
	for _, test := range states {
		t.Run("state/"+test.name, func(t *testing.T) {
			t.Parallel()

			if got := auth.VerifyState(test.got, test.want); got != test.expect {
				t.Errorf("VerifyState = %v, want %v", got, test.expect)
			}
		})
	}
}

func TestAuthorizeURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		console   string
		callback  string
		challenge string
		state     string
		want      string
		wantErr   string
	}{
		{
			name:      "https console",
			console:   "https://simple.test/",
			callback:  "http://127.0.0.1:54321/callback",
			challenge: "chal",
			state:     "st",
			want:      "https://simple.test/cli/authorize?callback=http%3A%2F%2F127.0.0.1%3A54321%2Fcallback&challenge=chal&state=st",
		},
		{
			name:      "loopback console for development",
			console:   "http://127.0.0.1:3000",
			callback:  "http://127.0.0.1:54321/callback",
			challenge: "chal",
			state:     "st",
			want:      "http://127.0.0.1:3000/cli/authorize?callback=http%3A%2F%2F127.0.0.1%3A54321%2Fcallback&challenge=chal&state=st",
		},
		{
			name:      "plain http to a public console is refused",
			console:   "http://simple.test",
			callback:  "http://127.0.0.1:1/callback",
			challenge: "chal",
			state:     "st",
			wantErr:   "console_url: must use https",
		},
		{name: "missing callback", console: "https://simple.test", challenge: "c", state: "s", wantErr: "callback: is required"},
		{name: "missing challenge", console: "https://simple.test", callback: "http://127.0.0.1:1/callback", state: "s", wantErr: "challenge: is required"},
		{name: "missing state", console: "https://simple.test", callback: "http://127.0.0.1:1/callback", challenge: "c", wantErr: "state: is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := auth.AuthorizeURL(test.console, test.callback, test.challenge, test.state)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("AuthorizeURL error = %v, want one containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("AuthorizeURL: %v", err)
			}
			if got != test.want {
				t.Errorf("AuthorizeURL = %q, want %q", got, test.want)
			}
		})
	}
}

func TestListenBindsLoopbackOnly(t *testing.T) {
	t.Parallel()

	listener, err := auth.Listen(auth.Expect{State: "st", Verifier: "ver"})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	redirect := listener.RedirectURI()
	if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
		t.Errorf("RedirectURI = %q, want a 127.0.0.1 address", redirect)
	}
	if !strings.HasSuffix(redirect, auth.CallbackPath) {
		t.Errorf("RedirectURI = %q, want it to end in %q", redirect, auth.CallbackPath)
	}
	if listener.Port() <= 0 {
		t.Errorf("Port = %d, want an ephemeral port", listener.Port())
	}
	// Close is idempotent: the CLI closes it and so does Wait.
	if err := listener.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestListenRejectsAnUnusableExpectation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		expect  auth.Expect
		wantErr string
	}{
		{name: "no state", expect: auth.Expect{Verifier: "v"}, wantErr: "expect: state is required"},
		{name: "no proof", expect: auth.Expect{State: "s"}, wantErr: "expect: a verifier or a challenge is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			listener, err := auth.Listen(test.expect)
			if err == nil {
				ignore(listener.Close())
				t.Fatal("Listen accepted an unusable expectation")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("Listen error = %v, want one containing %q", err, test.wantErr)
			}
		})
	}
}

// TestLogin walks the whole flow with a fake browser standing in for the
// console, one case per outcome the spec names.
func TestLogin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		answer      func(query url.Values) map[string]string
		contentType string
		timeout     time.Duration
		wantErr     error
		wantErrText string
		wantStatus  int
	}{
		{
			name: "success with the verifier echoed as a challenge",
			answer: func(query url.Values) map[string]string {
				return map[string]string{
					"state":     query.Get("state"),
					"challenge": query.Get("challenge"),
					"token":     theSecret,
				}
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "success with a form-encoded body",
			answer: func(query url.Values) map[string]string {
				return map[string]string{
					"state":     query.Get("state"),
					"challenge": query.Get("challenge"),
					"token":     theSecret,
				}
			},
			contentType: "application/x-www-form-urlencoded",
			wantStatus:  http.StatusOK,
		},
		{
			name: "state mismatch",
			answer: func(query url.Values) map[string]string {
				return map[string]string{
					"state":     "some-other-state",
					"challenge": query.Get("challenge"),
					"token":     theSecret,
				}
			},
			wantErr:    auth.ErrStateMismatch,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "missing state",
			answer: func(query url.Values) map[string]string {
				return map[string]string{"challenge": query.Get("challenge"), "token": theSecret}
			},
			wantErr:    auth.ErrStateMismatch,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "challenge mismatch",
			answer: func(query url.Values) map[string]string {
				return map[string]string{
					"state":     query.Get("state"),
					"challenge": "some-other-challenge",
					"token":     theSecret,
				}
			},
			wantErr:    auth.ErrChallengeMismatch,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "a verifier that does not hash to the challenge",
			answer: func(query url.Values) map[string]string {
				return map[string]string{
					"state":    query.Get("state"),
					"verifier": "not-the-verifier",
					"token":    theSecret,
				}
			},
			wantErr:    auth.ErrChallengeMismatch,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "no proof at all",
			answer: func(query url.Values) map[string]string {
				return map[string]string{"state": query.Get("state"), "token": theSecret}
			},
			wantErr:    auth.ErrChallengeMismatch,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "denied in the browser",
			answer: func(query url.Values) map[string]string {
				return map[string]string{"state": query.Get("state"), "error": "access_denied"}
			},
			wantErr:    auth.ErrDenied,
			wantStatus: http.StatusOK,
		},
		{
			name: "an unusable credential",
			answer: func(query url.Values) map[string]string {
				return map[string]string{
					"state":     query.Get("state"),
					"challenge": query.Get("challenge"),
					"token":     "has a space",
				}
			},
			wantErrText: "token: must contain only printable ASCII",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name: "an empty credential",
			answer: func(query url.Values) map[string]string {
				return map[string]string{
					"state":     query.Get("state"),
					"challenge": query.Get("challenge"),
					"token":     "",
				}
			},
			wantErrText: "token: is required",
			wantStatus:  http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			safe := &vault{}
			fake := &browser{answer: test.answer, contentType: test.contentType}

			var announced []string
			credential, err := auth.Login(t.Context(), auth.LoginOptions{
				ConsoleURL: "https://simple.test",
				Timeout:    10 * time.Second,
				Announce:   func(target string) { announced = append(announced, target) },
				Open:       fake.open,
				Store:      safe.store,
			})

			// Whatever happened, the authorize URL carried no credential.
			authorize := fake.authorizeURL(t)
			if strings.Contains(authorize.String(), theSecret) {
				t.Error("the authorize URL carried the credential")
			}
			if authorize.Path != auth.AuthorizePath {
				t.Errorf("authorize path = %q, want %q", authorize.Path, auth.AuthorizePath)
			}
			for _, name := range []string{"callback", "challenge", "state"} {
				if authorize.Query().Get(name) == "" {
					t.Errorf("the authorize URL is missing %q", name)
				}
			}
			if authorize.Query().Get("verifier") != "" {
				t.Error("the authorize URL leaked the PKCE verifier")
			}
			if len(announced) != 1 || announced[0] != authorize.String() {
				t.Errorf("Announce was called %d times, want once with the authorize URL", len(announced))
			}
			if got := fake.status(); got != test.wantStatus {
				t.Errorf("the browser received %d, want %d", got, test.wantStatus)
			}

			if test.wantErr == nil && test.wantErrText == "" {
				if err != nil {
					t.Fatalf("Login: %v", err)
				}
				if credential.Token != theSecret {
					t.Error("the credential that came back is not the one the console sent")
				}
				if safe.count() != 1 {
					t.Fatalf("%d credentials were stored, want exactly 1", safe.count())
				}
				return
			}

			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("Login error = %v, want %v", err, test.wantErr)
			}
			if test.wantErrText != "" && (err == nil || !strings.Contains(err.Error(), test.wantErrText)) {
				t.Fatalf("Login error = %v, want one containing %q", err, test.wantErrText)
			}
			if credential.Token != "" {
				t.Error("a failing login returned a credential")
			}
			if err != nil && strings.Contains(err.Error(), theSecret) {
				t.Error("the error quoted the credential")
			}
			safe.mustBeEmpty(t)
		})
	}
}

func TestLoginTimesOut(t *testing.T) {
	t.Parallel()

	safe := &vault{}
	// A browser that never answers is exactly the operator who walked away.
	silent := func(string) error { return nil }

	started := time.Now()
	credential, err := auth.Login(t.Context(), auth.LoginOptions{
		ConsoleURL: "https://simple.test",
		Timeout:    150 * time.Millisecond,
		Open:       silent,
		Store:      safe.store,
	})
	if !errors.Is(err, auth.ErrTimedOut) {
		t.Fatalf("Login error = %v, want ErrTimedOut", err)
	}
	if !strings.Contains(err.Error(), "--token") {
		t.Errorf("the timeout message does not tell the operator to use --token: %v", err)
	}
	if credential.Token != "" {
		t.Error("a timed-out login returned a credential")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("the timeout took %s, want it bounded by Timeout", elapsed)
	}
	safe.mustBeEmpty(t)
}

func TestLoginStopsWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	safe := &vault{}
	ctx, cancel := context.WithCancel(t.Context())

	credential, err := auth.Login(ctx, auth.LoginOptions{
		ConsoleURL: "https://simple.test",
		Timeout:    time.Minute,
		Open: func(string) error {
			cancel()
			return nil
		},
		Store: safe.store,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Login error = %v, want context.Canceled", err)
	}
	if credential.Token != "" {
		t.Error("a cancelled login returned a credential")
	}
	safe.mustBeEmpty(t)
}

func TestLoginRejectsBadOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    auth.LoginOptions
		wantErr string
	}{
		{
			name:    "no store",
			opts:    auth.LoginOptions{ConsoleURL: "https://simple.test"},
			wantErr: "store: is required",
		},
		{
			name: "an unusable console URL",
			opts: auth.LoginOptions{
				ConsoleURL: "ftp://simple.test",
				Store:      func(auth.Credential) error { return nil },
				Open:       func(string) error { return nil },
			},
			wantErr: "console_url: must be an absolute http or https URL",
		},
		{
			name: "an exhausted random source",
			opts: auth.LoginOptions{
				ConsoleURL: "https://simple.test",
				Random:     strings.NewReader(""),
				Store:      func(auth.Credential) error { return nil },
				Open:       func(string) error { return nil },
			},
			wantErr: "pkce:",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if _, err := auth.Login(t.Context(), test.opts); err == nil ||
				!strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Login error = %v, want one containing %q", err, test.wantErr)
			}
		})
	}
}

// TestLoginSurfacesAStoreFailure covers the one path where verification passed
// but persistence did not: the operator must be told, not left believing they
// are logged in.
func TestLoginSurfacesAStoreFailure(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("config: read-only file system")
	fake := &browser{answer: func(query url.Values) map[string]string {
		return map[string]string{
			"state":     query.Get("state"),
			"challenge": query.Get("challenge"),
			"token":     theSecret,
		}
	}}

	credential, err := auth.Login(t.Context(), auth.LoginOptions{
		ConsoleURL: "https://simple.test",
		Timeout:    10 * time.Second,
		Open:       fake.open,
		Store:      func(auth.Credential) error { return sentinel },
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Login error = %v, want the store's error", err)
	}
	if credential.Token != "" {
		t.Error("Login returned a credential it could not store")
	}
}

// TestLoginWhenTheBrowserWillNotOpen covers the two halves of the rule: a
// browser that cannot be opened is fatal only when nobody was told the URL.
func TestLoginWhenTheBrowserWillNotOpen(t *testing.T) {
	t.Parallel()

	broken := func(string) error { return errors.New("no display") }

	t.Run("without Announce the operator is told", func(t *testing.T) {
		t.Parallel()

		safe := &vault{}
		_, err := auth.Login(t.Context(), auth.LoginOptions{
			ConsoleURL: "https://simple.test",
			Timeout:    10 * time.Second,
			Open:       broken,
			Store:      safe.store,
		})
		if err == nil || !strings.Contains(err.Error(), "browser: could not be opened") {
			t.Fatalf("Login error = %v, want it to name the browser", err)
		}
		safe.mustBeEmpty(t)
	})

	t.Run("with Announce the operator can open it themselves", func(t *testing.T) {
		t.Parallel()

		safe := &vault{}
		announced := make(chan string, 1)
		_, err := auth.Login(t.Context(), auth.LoginOptions{
			ConsoleURL: "https://simple.test",
			Timeout:    150 * time.Millisecond,
			Announce:   func(target string) { announced <- target },
			Open:       broken,
			Store:      safe.store,
		})
		if !errors.Is(err, auth.ErrTimedOut) {
			t.Fatalf("Login error = %v, want it to have waited and timed out", err)
		}
		select {
		case target := <-announced:
			if !strings.Contains(target, auth.AuthorizePath) {
				t.Errorf("announced %q, want the authorize URL", target)
			}
		default:
			t.Error("nothing was announced")
		}
		safe.mustBeEmpty(t)
	})
}

// TestCallbackRefusesEverythingButPost is the "POST body only" rule: a GET can
// put a credential in a server log, so nothing else is served.
func TestCallbackRefusesEverythingButPost(t *testing.T) {
	t.Parallel()

	pkce, err := auth.NewPKCE(nil)
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	listener, err := auth.Listen(auth.Expect{State: "st", Verifier: pkce.Verifier})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ignore(listener.Close()) })

	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{name: "get is refused", method: http.MethodGet, path: auth.CallbackPath, want: http.StatusMethodNotAllowed},
		{name: "put is refused", method: http.MethodPut, path: auth.CallbackPath, want: http.StatusMethodNotAllowed},
		{name: "another path is not found", method: http.MethodPost, path: "/", want: http.StatusNotFound},
		{name: "preflight is allowed", method: http.MethodOptions, path: auth.CallbackPath, want: http.StatusNoContent},
	}
	base := strings.TrimSuffix(listener.RedirectURI(), auth.CallbackPath)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), test.method, base+test.path, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer func() { ignore(response.Body.Close()) }()

			if response.StatusCode != test.want {
				t.Errorf("status = %d, want %d", response.StatusCode, test.want)
			}
			if test.method == http.MethodOptions {
				if got := response.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
					t.Errorf("preflight allows %q, want POST", got)
				}
				if got := response.Header.Get("Access-Control-Allow-Private-Network"); got != "true" {
					t.Errorf("preflight private-network header = %q, want true", got)
				}
			}
		})
	}

	// None of that counted as an answer: the listener is still waiting.
	waitCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := listener.Wait(waitCtx); !errors.Is(err, auth.ErrTimedOut) {
		t.Errorf("a refused request settled the login: %v", err)
	}
}

func TestCallbackRejectsAnOversizedBody(t *testing.T) {
	t.Parallel()

	listener, err := auth.Listen(auth.Expect{State: "st", Verifier: "ver"})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ignore(listener.Close()) })

	body := strings.NewReader(`{"state":"st","token":"` + strings.Repeat("a", 128<<10) + `"}`)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, listener.RedirectURI(), body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err == nil {
		defer func() { ignore(response.Body.Close()) }()
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
		}
	}

	if _, err := listener.Wait(t.Context()); err == nil {
		t.Error("an oversized body was accepted")
	}
}

// TestCallbackAnswersOnlyOnce proves the server shuts down as soon as it has an
// answer, so a second post cannot replace the first.
func TestCallbackAnswersOnlyOnce(t *testing.T) {
	t.Parallel()

	pkce, err := auth.NewPKCE(nil)
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	listener, err := auth.Listen(auth.Expect{State: "st", Verifier: pkce.Verifier})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	callback := listener.RedirectURI()

	post := func(token string) (int, error) {
		body, err := json.Marshal(map[string]string{
			"state": "st", "challenge": pkce.Challenge, "token": token,
		})
		if err != nil {
			return 0, err
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, callback, bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return 0, err
		}
		defer func() { ignore(response.Body.Close()) }()
		return response.StatusCode, nil
	}

	status, err := post(theSecret)
	if err != nil {
		t.Fatalf("first post: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("first post status = %d, want 200", status)
	}

	credential, err := listener.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if credential.Token != theSecret {
		t.Error("the wrong credential came back")
	}

	// Wait closed the listener, so nothing is listening on that port any more.
	if _, err := post("a-second-token"); err == nil {
		t.Error("the callback server was still accepting posts after it answered")
	}
}

// TestCredentialNeverRenders keeps the token out of every rendering, so a
// stray %v in the CLI cannot print it.
func TestCredentialNeverRenders(t *testing.T) {
	t.Parallel()

	credential := auth.Credential{Token: theSecret}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("logged in", "credential", credential)

	for _, rendered := range []string{
		credential.String(),
		fmt.Sprintf("%v|%+v|%s", credential, credential, credential),
		logged.String(),
	} {
		if strings.Contains(rendered, theSecret) {
			t.Errorf("a rendering leaked the credential: %s", rendered)
		}
		if !strings.Contains(rendered, "[redacted]") {
			t.Errorf("a rendering did not mark the credential as redacted: %s", rendered)
		}
	}
	if got := (auth.Credential{}).String(); strings.Contains(got, "[redacted]") {
		t.Errorf("the zero credential renders as %q, want it to say nothing is held", got)
	}
}

func TestOpenBrowserRefusesNonHTTPSchemes(t *testing.T) {
	t.Parallel()

	for _, target := range []string{
		"file:///etc/passwd",
		"javascript:alert(1)",
		"ftp://simple.test",
		"; rm -rf /",
	} {
		if err := auth.OpenBrowser(target); err == nil {
			t.Errorf("OpenBrowser accepted %q", target)
		}
	}
}
