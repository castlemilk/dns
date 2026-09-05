package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"

	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/internal/simplecli/auth"
	"github.com/castlemilk/dns/internal/simplecli/client"
	"github.com/castlemilk/dns/internal/simplecli/config"
)

// envConsoleURL points `simple auth login` at the console when it is not served
// from the control plane's own origin.
const envConsoleURL = "SIMPLE_CONSOLE_URL"

func cmdAuth(ctx context.Context, e *env, o *options, args []string) error {
	if len(args) == 0 {
		return &usageError{message: "auth: choose login, logout, status or token", usage: authUsage}
	}
	switch args[0] {
	case "login":
		return cmdAuthLogin(ctx, e, o, args[1:])
	case "logout":
		return cmdAuthLogout(e, o, args[1:])
	case "status":
		return cmdAuthStatus(ctx, e, o, args[1:])
	case "token":
		return cmdAuthToken(e, o, args[1:])
	case "help", "-h", "--help":
		return &helpError{usage: authUsage}
	default:
		return &usageError{message: fmt.Sprintf("unknown auth command %q", args[0]), usage: authUsage}
	}
}

// cmdAuthLogin drives the §3 handoff: a loopback callback, a browser sent to
// the console's approval screen, a state and PKCE check on what comes back, and
// only then a 0600 write.
//
// This is not OIDC. The control plane has one static operator credential and no
// identity provider; the flow is authorization-code-shaped so that the
// credential is bound to this one invocation and never travels in a URL.
func cmdAuthLogin(ctx context.Context, e *env, o *options, args []string) error {
	var noBrowser bool
	fs, err := o.parse("simple auth login", authUsage, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&noBrowser, "no-browser", false, "print the approval URL instead of opening a browser")
	})
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, authUsage); err != nil {
		return err
	}
	resolved, _, err := o.settings(e)
	if err != nil {
		return err
	}
	apiURL, err := config.ValidateURL("api_url", resolved.APIURL)
	if err != nil {
		return err
	}
	consoleURL, err := o.console(e, apiURL)
	if err != nil {
		return err
	}
	path, err := o.configPath(e)
	if err != nil {
		return err
	}

	// The login waits for a human, so it keeps its own five-minute budget
	// unless the operator asked for a different one.
	timeout := auth.DefaultTimeout
	if provided(fs, "timeout") {
		timeout = o.timeout
	}

	open := e.openBrowser
	if noBrowser {
		open = func(string) error {
			return errors.New("--no-browser was given")
		}
	}
	printer := newPrinter(e, o)
	if _, err := fmt.Fprintf(e.stderr,
		"Approve this login in the console at %s.\nThis hands the operator credential to this machine; it is not a user login.\n",
		consoleURL); err != nil {
		return err
	}

	// The credential itself is deliberately dropped here: it has already been
	// written 0600 by Store, and nothing below has any use for its value.
	if _, err := auth.Login(ctx, auth.LoginOptions{
		ConsoleURL: consoleURL,
		Timeout:    timeout,
		Random:     e.random,
		Open:       open,
		// The URL carries the callback, the challenge and the state, and no
		// secret, so printing it is safe and is what makes a headless or
		// remote session workable.
		Announce: func(authorizeURL string) {
			ignore(writeLine(e.stderr, "  "+authorizeURL))
		},
		Store: func(credential auth.Credential) error {
			return config.Save(path, config.Config{APIURL: apiURL, Token: credential.Token})
		},
	}); err != nil {
		return err
	}

	document := struct {
		LoggedIn   bool   `json:"logged_in"`
		APIURL     string `json:"api_url"`
		ConsoleURL string `json:"console_url"`
		ConfigPath string `json:"config_path"`
	}{LoggedIn: true, APIURL: apiURL, ConsoleURL: consoleURL, ConfigPath: path}

	if o.json {
		return printer.emit(document, nil)
	}
	if err := writeBanner(e); err != nil {
		return err
	}
	return fields(e.stdout,
		pair("logged in to", apiURL),
		pair("credential", "stored at "+path+" (0600)"),
	)
}

// console decides where the approval screen lives. The console and the control
// plane share an origin in the default deployment, so the API URL is the
// fallback; --console-url or SIMPLE_CONSOLE_URL covers a split deployment.
func (o *options) console(e *env, apiURL string) (string, error) {
	raw := strings.TrimSpace(o.consoleURL)
	if raw == "" {
		raw = strings.TrimSpace(e.getenv(envConsoleURL))
	}
	if raw == "" {
		return apiURL, nil
	}
	return config.ValidateURL("console_url", raw)
}

func cmdAuthLogout(e *env, o *options, args []string) error {
	fs, err := o.parse("simple auth logout", authUsage, args, nil)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, authUsage); err != nil {
		return err
	}
	path, err := o.configPath(e)
	if err != nil {
		return err
	}
	if err := config.Delete(path); err != nil {
		return err
	}
	document := struct {
		LoggedOut  bool   `json:"logged_out"`
		ConfigPath string `json:"config_path"`
		Note       string `json:"note,omitempty"`
	}{LoggedOut: true, ConfigPath: path}
	if strings.TrimSpace(e.getenv(config.EnvToken)) != "" {
		// Deleting the file changes nothing while the environment still holds
		// a credential; saying so is the difference between "logged out" and
		// actually being logged out.
		document.Note = config.EnvToken + " is still set in this environment, so commands remain authenticated"
	}
	return newPrinter(e, o).emit(document, func(w io.Writer) error {
		pairs := []([2]string){pair("removed", path)}
		if document.Note != "" {
			pairs = append(pairs, pair("note", document.Note))
		}
		return fields(w, pairs...)
	})
}

func cmdAuthStatus(ctx context.Context, e *env, o *options, args []string) error {
	var check bool
	fs, err := o.parse("simple auth status", authUsage, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&check, "check", false, "ask the control plane whether the credential is accepted")
	})
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, authUsage); err != nil {
		return err
	}
	resolved, path, err := o.settings(e)
	if err != nil {
		return err
	}
	document := struct {
		APIURL        string `json:"api_url"`
		APIURLFrom    string `json:"api_url_from"`
		Authenticated bool   `json:"authenticated"`
		TokenFrom     string `json:"token_from"`
		ConfigPath    string `json:"config_path"`
		Checked       bool   `json:"checked"`
		Accepted      bool   `json:"accepted"`
		Detail        string `json:"detail,omitempty"`
	}{
		APIURL:        resolved.APIURL,
		APIURLFrom:    string(resolved.APIURLFrom),
		Authenticated: resolved.Token != "",
		TokenFrom:     string(resolved.TokenFrom),
		ConfigPath:    path,
	}
	if check {
		document.Checked = true
		clients, err := o.connect(e)
		if err != nil {
			return err
		}
		callCtx, cancel := o.call(ctx)
		defer cancel()
		if _, err := clients.Platform.GetPlatformStatus(callCtx,
			connect.NewRequest(&platformv1.GetPlatformStatusRequest{})); err != nil {
			document.Detail = client.ExplainFor(clients.BaseURL, err)
		} else {
			document.Accepted = true
		}
	}
	return newPrinter(e, o).emit(document, func(w io.Writer) error {
		pairs := []([2]string){
			pair("control plane", document.APIURL+" (from "+document.APIURLFrom+")"),
			pair("credential", credentialSummary(resolved)),
			pair("config", path),
		}
		if check {
			result := "accepted"
			if !document.Accepted {
				result = document.Detail
			}
			pairs = append(pairs, pair("checked", result))
		}
		return fields(w, pairs...)
	})
}

func credentialSummary(resolved config.Resolved) string {
	if resolved.Token == "" {
		return "none; run `simple auth login`, or pass --token"
	}
	return config.Redacted + " (from " + string(resolved.TokenFrom) + ")"
}

// cmdAuthToken prints the credential only when it is asked for explicitly, and
// says on stderr what has just been put on the screen.
func cmdAuthToken(e *env, o *options, args []string) error {
	var show bool
	fs, err := o.parse("simple auth token", authUsage, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&show, "show", false, "print the credential itself")
	})
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, authUsage); err != nil {
		return err
	}
	resolved, _, err := o.settings(e)
	if err != nil {
		return err
	}
	if resolved.Token == "" {
		return errors.New("token: none is configured; run `simple auth login`, or pass --token")
	}
	if !show {
		return newPrinter(e, o).emit(struct {
			Token     string `json:"token"`
			TokenFrom string `json:"token_from"`
		}{Token: config.Redacted, TokenFrom: string(resolved.TokenFrom)}, func(w io.Writer) error {
			return fields(w,
				pair("credential", config.Redacted+" (from "+string(resolved.TokenFrom)+")"),
				pair("to print it", "simple auth token --show"),
			)
		})
	}
	if _, err := fmt.Fprintln(e.stderr,
		"warning: the operator credential is now on your screen and in this terminal's scrollback."); err != nil {
		return err
	}
	if o.json {
		// The document is the credential the operator asked for; no mark, no
		// banner and nothing else is written to stdout.
		return writeJSONTo(e.stdout, struct {
			Token     string `json:"token"`
			TokenFrom string `json:"token_from"`
		}{Token: resolved.Token, TokenFrom: string(resolved.TokenFrom)})
	}
	return writeLine(e.stdout, resolved.Token)
}

func writeLine(w io.Writer, text string) error {
	_, err := fmt.Fprintln(w, text)
	return err
}
