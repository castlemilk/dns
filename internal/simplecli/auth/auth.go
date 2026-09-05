// Package auth carries the reusable parts of `simple auth login`.
//
// This is not OIDC and there is no identity provider. The control plane has
// exactly one static operator credential, and this flow is an
// authorization-code-shaped handoff of that credential from a browser session
// that is already unlocked to a CLI running on the same machine. PKCE and the
// state parameter are here to bind the browser's answer to this one CLI
// invocation, not to authenticate a user — say so anywhere this is described,
// rather than implying an IdP exists.
//
// The credential travels in the body of a POST to 127.0.0.1 and nowhere else:
// never in a URL, a query string, a log line or the terminal. Nothing is
// stored on any failure path; storing is the caller's Store function, which is
// reached only after state and challenge have both been verified.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/castlemilk/dns/internal/simplecli/config"
)

const (
	// DefaultTimeout is how long the CLI waits for the human to approve. After
	// it, the operator is told to use --token instead.
	DefaultTimeout = 5 * time.Minute

	// CallbackPath is the only path the loopback server answers on.
	CallbackPath = "/callback"

	// AuthorizePath is the console route that renders the approval screen.
	AuthorizePath = "/cli/authorize"

	// verifierBytes yields a 43-character S256 verifier, the shortest RFC 7636
	// allows and long enough that guessing it is hopeless.
	verifierBytes = 32
	stateBytes    = 16

	// maxCallbackBytes bounds the POST body. The answer is three short strings.
	maxCallbackBytes = 64 << 10

	// shutdownGrace bounds how long Close waits for the browser's response to
	// finish being written before the listener is torn down.
	shutdownGrace = 5 * time.Second
)

// The failures a caller distinguishes. Each is phrased for an operator, and
// each means nothing was stored.
var (
	ErrStateMismatch = errors.New(
		"state: the browser answered with a different state, so the login was abandoned")
	ErrChallengeMismatch = errors.New(
		"verifier: the browser answered with a verifier that does not match the challenge, so the login was abandoned")
	ErrTimedOut = errors.New(
		"the browser did not answer in time; approve the login sooner, or pass --token instead")
	ErrDenied = errors.New(
		"the login was declined in the browser")
)

// Credential is the operator token the browser handed back. Its String and
// LogValue render a placeholder, so printing one by accident is harmless.
type Credential struct {
	Token string
}

func (c Credential) String() string {
	if c.Token == "" {
		return "credential{}"
	}
	return "credential{" + config.Redacted + "}"
}

func (c Credential) LogValue() slog.Value {
	if c.Token == "" {
		return slog.StringValue("")
	}
	return slog.StringValue(config.Redacted)
}

var (
	_ fmt.Stringer   = Credential{}
	_ slog.LogValuer = Credential{}
)

// PKCE is one verifier and the S256 challenge derived from it. The verifier
// stays in the CLI until the browser sends its copy back; the challenge is the
// only half that appears in a URL.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE draws a fresh verifier. random may be nil, in which case
// crypto/rand.Reader is used.
func NewPKCE(random io.Reader) (PKCE, error) {
	verifier, err := randomString(random, verifierBytes)
	if err != nil {
		return PKCE{}, fmt.Errorf("pkce: %w", err)
	}
	return PKCE{Verifier: verifier, Challenge: ChallengeFor(verifier)}, nil
}

// ChallengeFor is the S256 transformation: base64url(sha256(verifier)), no
// padding, exactly as RFC 7636 defines it.
func ChallengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// VerifyChallenge reports whether verifier is the one the challenge was made
// from. The comparison is constant time, and an empty half never matches.
func VerifyChallenge(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(ChallengeFor(verifier)), []byte(challenge)) == 1
}

// NewState draws the random state that binds the browser's answer to this
// invocation. random may be nil.
func NewState(random io.Reader) (string, error) {
	state, err := randomString(random, stateBytes)
	if err != nil {
		return "", fmt.Errorf("state: %w", err)
	}
	return state, nil
}

// VerifyState compares two states in constant time; an empty half never
// matches.
func VerifyState(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func randomString(random io.Reader, size int) (string, error) {
	if random == nil {
		random = rand.Reader
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Expect is what the browser's answer has to match.
//
// The console is given only the challenge, never the verifier, so it can prove
// it is answering this invocation in one of two ways: by echoing the challenge
// it was sent, or by returning a verifier that hashes to it. Both are accepted;
// which one the console uses is its choice, and neither reveals anything the
// console did not already have.
type Expect struct {
	// State is the random value the console was sent and must return.
	State string

	// Verifier is the CLI's own PKCE verifier. It never leaves the process.
	// When Challenge is empty it is derived from this.
	Verifier string

	// Challenge is base64url(sha256(Verifier)). Set it directly only when the
	// verifier is held elsewhere.
	Challenge string
}

// normalise fills in the derived half and reports whether the pair is usable.
func (e Expect) normalise() (Expect, error) {
	if e.State == "" {
		return Expect{}, errors.New("expect: state is required")
	}
	if e.Challenge == "" {
		if e.Verifier == "" {
			return Expect{}, errors.New("expect: a verifier or a challenge is required")
		}
		e.Challenge = ChallengeFor(e.Verifier)
	}
	return e, nil
}

// Listener is the loopback callback server. It binds 127.0.0.1 on an ephemeral
// port, answers exactly one POST, and shuts down as soon as it has an answer or
// the caller gives up.
type Listener struct {
	expect   Expect
	listener net.Listener
	server   *http.Server
	answers  chan answer

	answered  sync.Once
	closeOnce sync.Once
	closeErr  error
	done      chan struct{}
}

type answer struct {
	credential Credential
	err        error
}

// Listen starts the callback server. The caller must Close it, which Wait also
// does.
func Listen(expect Expect) (*Listener, error) {
	expect, err := expect.normalise()
	if err != nil {
		return nil, err
	}
	// 127.0.0.1 explicitly, not "localhost": the callback must never be
	// reachable from another host, and must not depend on how localhost
	// happens to resolve.
	socket, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		return nil, fmt.Errorf("callback: %w", listenErr)
	}
	listener := &Listener{
		expect:   expect,
		listener: socket,
		answers:  make(chan answer, 1),
		done:     make(chan struct{}),
	}
	listener.server = &http.Server{
		Handler:           http.HandlerFunc(listener.serve),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		defer close(listener.done)
		if err := listener.server.Serve(socket); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listener.settle(answer{err: fmt.Errorf("callback: %w", err)})
		}
	}()
	return listener, nil
}

// RedirectURI is the callback the console must post to. It is safe to put in a
// URL: it holds no secret.
func (l *Listener) RedirectURI() string {
	return "http://" + l.listener.Addr().String() + CallbackPath
}

// Port is the ephemeral port the callback is listening on.
func (l *Listener) Port() int {
	if address, ok := l.listener.Addr().(*net.TCPAddr); ok {
		return address.Port
	}
	return 0
}

// Wait blocks until the browser answers, ctx is done, or the server fails. It
// closes the listener before returning, so the port is never left open.
//
// A ctx deadline becomes ErrTimedOut; the state and challenge failures become
// ErrStateMismatch and ErrChallengeMismatch. In every failing case the returned
// Credential is the zero value and the caller has been given nothing to store.
func (l *Listener) Wait(ctx context.Context) (Credential, error) {
	credential, err := l.wait(ctx)
	// The port must never be left open, and a shutdown that failed is worth
	// reporting even when the login itself succeeded: the caller would
	// otherwise leave a listening socket behind without knowing.
	return credential, errors.Join(err, l.Close())
}

func (l *Listener) wait(ctx context.Context) (Credential, error) {
	select {
	case got := <-l.answers:
		if got.err != nil {
			return Credential{}, got.err
		}
		return got.credential, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Credential{}, ErrTimedOut
		}
		return Credential{}, ctx.Err()
	}
}

// Close shuts the callback server down. It is idempotent and safe to call
// alongside Wait.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		l.closeErr = l.server.Shutdown(shutdown)
		if errors.Is(l.closeErr, context.DeadlineExceeded) {
			l.closeErr = l.server.Close()
		}
		<-l.done
	})
	return l.closeErr
}

// settle records the first answer and ignores every later one, so a second
// request cannot overwrite a decision already taken.
func (l *Listener) settle(got answer) {
	l.answered.Do(func() { l.answers <- got })
}

func (l *Listener) serve(writer http.ResponseWriter, request *http.Request) {
	// The console is served from another origin, so the browser preflights the
	// POST. Nothing in the response is secret, so any origin may read it; the
	// state and challenge are what actually bind the answer to this CLI.
	writer.Header().Set("Access-Control-Allow-Origin", "*")
	writer.Header().Set("Cache-Control", "no-store")

	if request.Method == http.MethodOptions {
		writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		// Chrome's Private Network Access check: a public HTTPS console
		// reaching a loopback address is refused without this.
		writer.Header().Set("Access-Control-Allow-Private-Network", "true")
		writer.Header().Set("Access-Control-Max-Age", "600")
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if request.URL.Path != CallbackPath {
		writeProblem(writer, http.StatusNotFound, "not_found", "no such callback")
		return
	}
	if request.Method != http.MethodPost {
		// The credential must arrive in a body, never in a URL, so nothing
		// else is served here.
		writer.Header().Set("Allow", "POST, OPTIONS")
		writeProblem(writer, http.StatusMethodNotAllowed, "method_not_allowed",
			"the credential must be posted in a request body")
		return
	}

	posted, err := readAnswer(writer, request)
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "invalid_request", "the callback body could not be read")
		l.settle(answer{err: err})
		return
	}
	if posted.Error != "" {
		writeProblem(writer, http.StatusOK, posted.Error, "the login was declined")
		l.settle(answer{err: ErrDenied})
		return
	}
	if !VerifyState(posted.State, l.expect.State) {
		writeProblem(writer, http.StatusBadRequest, "invalid_state", "the state did not match")
		l.settle(answer{err: ErrStateMismatch})
		return
	}
	if !l.verifyProof(posted) {
		writeProblem(writer, http.StatusBadRequest, "invalid_verifier", "the proof did not match the challenge")
		l.settle(answer{err: ErrChallengeMismatch})
		return
	}
	if err := config.ValidateToken(posted.Token); err != nil {
		writeProblem(writer, http.StatusBadRequest, "invalid_token", "the credential is not a usable bearer token")
		l.settle(answer{err: err})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	l.settle(answer{credential: Credential{Token: posted.Token}})
}

// verifyProof checks the half of the exchange that binds the answer to this
// process: either a verifier that hashes to the expected challenge, or the
// expected challenge echoed back. Both comparisons are constant time.
func (l *Listener) verifyProof(posted postedAnswer) bool {
	if posted.Verifier != "" {
		return VerifyChallenge(posted.Verifier, l.expect.Challenge)
	}
	if posted.Challenge != "" {
		return subtle.ConstantTimeCompare([]byte(posted.Challenge), []byte(l.expect.Challenge)) == 1
	}
	return false
}

// postedAnswer is what the console sends back. Only Token is secret, and it is
// never echoed into a response, a log or an error.
type postedAnswer struct {
	State     string `json:"state"`
	Verifier  string `json:"verifier"`
	Challenge string `json:"challenge"`
	Token     string `json:"token"`
	Error     string `json:"error"`
}

func readAnswer(writer http.ResponseWriter, request *http.Request) (postedAnswer, error) {
	body := http.MaxBytesReader(writer, request.Body, maxCallbackBytes)
	mediaType, _, _ := strings.Cut(request.Header.Get("Content-Type"), ";")

	if strings.TrimSpace(strings.ToLower(mediaType)) == "application/json" {
		raw, err := io.ReadAll(body)
		if err != nil {
			return postedAnswer{}, errors.New("callback: the body could not be read")
		}
		var posted postedAnswer
		if err := json.Unmarshal(raw, &posted); err != nil {
			// The decoder's message can quote the body, which holds the
			// credential, so it is deliberately dropped.
			return postedAnswer{}, errors.New("callback: the body is not a valid JSON object")
		}
		return posted, nil
	}

	raw, err := io.ReadAll(body)
	if err != nil {
		return postedAnswer{}, errors.New("callback: the body could not be read")
	}
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		return postedAnswer{}, errors.New("callback: the body is not a valid form")
	}
	return postedAnswer{
		State:     values.Get("state"),
		Verifier:  values.Get("verifier"),
		Challenge: values.Get("challenge"),
		Token:     values.Get("token"),
		Error:     values.Get("error"),
	}, nil
}

func writeProblem(writer http.ResponseWriter, status int, code, description string) {
	writeJSON(writer, status, map[string]string{"error": code, "error_description": description})
}

func writeJSON(writer http.ResponseWriter, status int, body map[string]string) {
	raw, err := json.Marshal(body)
	if err != nil {
		raw = []byte(`{"error":"internal"}`)
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	// A browser that hung up before reading the reply changes nothing: the
	// decision has already been recorded on the channel.
	_, writeErr := writer.Write(raw)
	ignore(writeErr)
}

// ignore drops an error deliberately, on a path that has nothing better to do
// with it. It exists because this repository's errcheck configuration rejects
// the `_ = f()` form, and a named call makes each such decision reviewable.
func ignore(error) {}

// AuthorizeURL builds the console approval URL. It carries the callback, the
// challenge and the state — never the verifier, and never a credential.
func AuthorizeURL(consoleURL, callback, challenge, state string) (string, error) {
	base, err := config.ValidateURL("console_url", consoleURL)
	if err != nil {
		return "", err
	}
	switch {
	case callback == "":
		return "", errors.New("callback: is required")
	case challenge == "":
		return "", errors.New("challenge: is required")
	case state == "":
		return "", errors.New("state: is required")
	}
	parsed, err := url.Parse(base + AuthorizePath)
	if err != nil {
		return "", fmt.Errorf("console_url: %w", err)
	}
	query := url.Values{}
	query.Set("callback", callback)
	query.Set("challenge", challenge)
	query.Set("state", state)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// LoginOptions describes one `simple auth login`.
type LoginOptions struct {
	// ConsoleURL is where the approval screen lives.
	ConsoleURL string

	// Timeout bounds the wait for the human. Zero means DefaultTimeout.
	Timeout time.Duration

	// Random draws the verifier and state. Zero means crypto/rand.
	Random io.Reader

	// Announce is called once with the authorize URL, before the browser is
	// opened, so the CLI can print it for an operator whose browser cannot be
	// opened for them. The URL holds no secret. Optional.
	Announce func(authorizeURL string)

	// Open opens the authorize URL. Nil means OpenBrowser. A failure here is
	// not fatal, because Announce has already given the operator the URL.
	Open func(authorizeURL string) error

	// Store persists the credential. It is called only after state and
	// challenge have both been verified, so no failure path reaches it.
	// Required.
	Store func(Credential) error
}

// Login runs the whole flow: draw a verifier and a state, open a loopback
// callback, send the human to the console, verify what comes back, and only
// then hand the credential to Store.
//
// On every failure path Store is not called, so nothing is written anywhere.
func Login(ctx context.Context, opts LoginOptions) (Credential, error) {
	if opts.Store == nil {
		return Credential{}, errors.New("store: is required")
	}
	pkce, err := NewPKCE(opts.Random)
	if err != nil {
		return Credential{}, err
	}
	state, err := NewState(opts.Random)
	if err != nil {
		return Credential{}, err
	}
	listener, err := Listen(Expect{State: state, Verifier: pkce.Verifier, Challenge: pkce.Challenge})
	if err != nil {
		return Credential{}, err
	}
	// Wait closes the listener; this covers the paths that never reach it.
	defer func() { ignore(listener.Close()) }()

	authorize, err := AuthorizeURL(opts.ConsoleURL, listener.RedirectURI(), pkce.Challenge, state)
	if err != nil {
		return Credential{}, err
	}
	if opts.Announce != nil {
		opts.Announce(authorize)
	}
	open := opts.Open
	if open == nil {
		open = OpenBrowser
	}
	// A browser that will not open is only fatal when nobody was told the URL:
	// without Announce the operator has no way to reach the approval screen,
	// and waiting out the timeout would tell them nothing useful.
	if err := open(authorize); err != nil && opts.Announce == nil {
		return Credential{}, fmt.Errorf("browser: could not be opened, and no URL was announced: %w", err)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Wait has already verified the state and the challenge; nothing that
	// failed either check can reach this line.
	credential, err := listener.Wait(waitCtx)
	if err != nil {
		return Credential{}, err
	}
	if err := opts.Store(credential); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

// OpenBrowser asks the desktop to open an http or https URL. It never runs a
// shell, and refuses any other scheme, so a URL can never become a command.
func OpenBrowser(target string) error {
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("browser: only http and https URLs can be opened")
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", parsed.String())
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", parsed.String())
	default:
		command = exec.Command("xdg-open", parsed.String())
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("browser: %w", err)
	}
	// The browser outlives the CLI; reaping it here would block until the
	// window closed, so the child is waited on in the background only to stop
	// it becoming a zombie.
	go func() { ignore(command.Wait()) }()
	return nil
}
