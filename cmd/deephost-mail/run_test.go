package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// token is long enough to be accepted, and is a literal so that the assertions
// about never echoing it have something to look for.
const token = "test-token-0123456789abcdefghijklmnop"

// -----------------------------------------------------------------------------
// Refusals and answers
// -----------------------------------------------------------------------------

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		env      map[string]string
		wantCode int
		// wantOut and wantErr are substrings of stdout and stderr.
		wantOut string
		wantErr string
	}{
		{
			name:     "version",
			args:     []string{"--version"},
			wantCode: codeOK,
			wantOut:  "deephost-mail ",
		},
		{
			name:     "help prints the usage block",
			args:     []string{"--help"},
			wantCode: codeOK,
			wantOut:  "usage: deephost-mail",
		},
		{
			name:     "an unknown flag is a usage mistake",
			args:     []string{"--nope"},
			wantCode: codeUsage,
			wantErr:  "usage: deephost-mail",
		},
		{
			name:     "a positional argument is a usage mistake",
			args:     []string{"start"},
			wantCode: codeUsage,
			wantErr:  `unexpected argument "start"`,
		},
		{
			// The refusal that matters most: the facade compares the hostname
			// this server reports against MAIL_HOSTNAME on every operation, and
			// reports a mismatch per operation rather than at startup.
			name:     "an empty hostname is refused",
			args:     []string{"--admin-token", token, "--validate"},
			wantCode: codeUsage,
			wantErr:  "the hostname is required and must equal the control plane's MAIL_HOSTNAME",
		},
		{
			name:     "a short admin token is refused",
			args:     []string{"--hostname", "mail.local.test", "--admin-token", "hunter2", "--validate"},
			wantCode: codeUsage,
			wantErr:  "the admin token must be at least 32 characters",
		},
		{
			name:     "a missing admin token is refused",
			args:     []string{"--hostname", "mail.local.test", "--validate"},
			wantCode: codeUsage,
			wantErr:  "got 0",
		},
		{
			name:     "both token flags at once",
			args:     []string{"--hostname", "mail.local.test", "--admin-token", token, "--admin-token-file", "/nonexistent"},
			wantCode: codeUsage,
			wantErr:  "choose one",
		},
		{
			name:     "an unreadable token file names the file",
			args:     []string{"--hostname", "mail.local.test", "--admin-token-file", "/nonexistent/token"},
			wantCode: codeUsage,
			wantErr:  "--admin-token-file:",
		},
		{
			name:     "API TLS without a certificate",
			args:     []string{"--hostname", "mail.local.test", "--admin-token", token, "--api-tls", "--validate"},
			wantCode: codeUsage,
			wantErr:  "--api-tls: needs --tls-cert and --tls-key",
		},
		{
			name:     "half a TLS pair",
			args:     []string{"--hostname", "mail.local.test", "--admin-token", token, "--tls-cert", "cert.pem", "--validate"},
			wantCode: codeUsage,
			wantErr:  "must be set together",
		},
		{
			name:     "a bad log level",
			args:     []string{"--hostname", "mail.local.test", "--admin-token", token, "--log-level", "loud"},
			wantCode: codeUsage,
			wantErr:  "--log-level: must be debug, info, warn or error",
		},
		{
			name:     "a bad log format",
			args:     []string{"--hostname", "mail.local.test", "--admin-token", token, "--log-format", "xml"},
			wantCode: codeUsage,
			wantErr:  "--log-format: must be text or json",
		},
		{
			name:     "a non-positive session limit",
			args:     []string{"--max-sessions", "0"},
			wantCode: codeUsage,
			wantErr:  "--max-sessions: must be positive",
		},
		{
			name:     "a non-positive shutdown grace",
			args:     []string{"--shutdown-grace", "0s"},
			wantCode: codeUsage,
			wantErr:  "--shutdown-grace: must be positive",
		},
		{
			name:     "an unparseable environment duration",
			args:     nil,
			env:      map[string]string{"MAILD_SESSION_TIMEOUT": "soon"},
			wantCode: codeUsage,
			wantErr:  `MAILD_SESSION_TIMEOUT: "soon" is not a duration`,
		},
		{
			name:     "an unparseable environment number",
			args:     nil,
			env:      map[string]string{"MAILD_MAX_SESSIONS": "lots"},
			wantCode: codeUsage,
			wantErr:  `MAILD_MAX_SESSIONS: "lots" is not a number`,
		},
		{
			name:     "an unparseable environment boolean",
			args:     nil,
			env:      map[string]string{"MAILD_API_TLS": "perhaps"},
			wantCode: codeUsage,
			wantErr:  `MAILD_API_TLS: "perhaps" is not a boolean`,
		},
		{
			name:     "validate accepts a complete configuration",
			args:     []string{"--hostname", "mail.local.test", "--admin-token", token, "--validate"},
			wantCode: codeOK,
			wantOut:  "the configuration is valid for mail.local.test",
		},
		{
			// Everything can come from the environment, because that is how a
			// chart configures a container.
			name: "the environment configures the whole process",
			env: map[string]string{
				"MAILD_HOSTNAME":    "mail.local.test",
				"MAILD_ADMIN_TOKEN": token,
			},
			args:     []string{"--validate"},
			wantCode: codeOK,
			wantOut:  "the configuration is valid for mail.local.test",
		},
		{
			// A flag beats the environment, so an operator can override one
			// value without rewriting the deployment.
			name: "a flag overrides the environment",
			env: map[string]string{
				"MAILD_HOSTNAME":    "wrong.local.test",
				"MAILD_ADMIN_TOKEN": token,
			},
			args:     []string{"--hostname", "mail.local.test", "--validate"},
			wantCode: codeOK,
			wantOut:  "the configuration is valid for mail.local.test",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"--data-dir", t.TempDir()}, test.args...)
			out, errOut, code := invoke(t, context.Background(), args, test.env)
			if code != test.wantCode {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, test.wantCode, out, errOut)
			}
			if test.wantOut != "" && !strings.Contains(out, test.wantOut) {
				t.Fatalf("stdout = %q, want it to contain %q", out, test.wantOut)
			}
			if test.wantErr != "" && !strings.Contains(errOut, test.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", errOut, test.wantErr)
			}
			if strings.Contains(out+errOut, token) {
				t.Fatalf("the output carried the admin token\nstdout: %s\nstderr: %s", out, errOut)
			}
		})
	}
}

// A token in a file is the deployment shape that keeps it out of the process
// table, and a file written by any ordinary means ends with a newline.
func TestAdminTokenFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	out, errOut, code := invoke(t, context.Background(), []string{
		"--hostname", "mail.local.test",
		"--admin-token-file", path,
		"--data-dir", t.TempDir(),
		"--validate",
	}, nil)
	if code != codeOK {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, codeOK, errOut)
	}
	if !strings.Contains(out, "the configuration is valid") {
		t.Fatalf("stdout = %q", out)
	}
}

// A token file whose content is too short is still refused, which proves the
// file is read before the length check rather than after it.
func TestAdminTokenFileIsCheckedForLength(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	_, errOut, code := invoke(t, context.Background(), []string{
		"--hostname", "mail.local.test",
		"--admin-token-file", path,
		"--data-dir", t.TempDir(),
		"--validate",
	}, nil)
	if code != codeUsage {
		t.Fatalf("exit code = %d, want %d", code, codeUsage)
	}
	if !strings.Contains(errOut, "got 7") {
		t.Fatalf("stderr = %q, want the length of the token it read", errOut)
	}
}

// A refused start must not have created a data directory, so a misconfigured
// container does not leave state behind on every restart.
func TestARefusedStartLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	_, _, code := invoke(t, context.Background(), []string{"--data-dir", dir, "--admin-token", token}, nil)
	if code != codeUsage {
		t.Fatalf("exit code = %d, want %d", code, codeUsage)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the refused start created %s", dir)
	}
}

// -----------------------------------------------------------------------------
// The process actually runs
// -----------------------------------------------------------------------------

// The whole binary starts, serves the management API, and stops cleanly on the
// signal a container runtime sends. Every listener but the management API is
// disabled, because this build has no SMTP or POP3 handler to install.
func TestRunServesAndStopsCleanly(t *testing.T) {
	t.Parallel()

	addr := freeAddr(t)
	smtpAddr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		out, errOut string
		code        int
	}
	done := make(chan result, 1)
	go func() {
		out, errOut, code := invoke(t, ctx, []string{
			"--hostname", "mail.local.test",
			"--admin-token", token,
			"--data-dir", t.TempDir(),
			"--api-addr", addr,
			"--log-format", "json",
			"--smtp-addr", smtpAddr,
			// Clearing an address is how a listener is turned off.
			"--submission-addr", "",
			"--submissions-addr", "",
			"--pop3-addr", "",
			"--pop3s-addr", "",
		}, nil)
		done <- result{out, errOut, code}
	}()

	// Wait for the port, then ask it to stop.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the management API never came up")
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			ignore(conn.Close())
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The SMTP port is a real SMTP server, not a bound socket with nothing
	// behind it: it greets.
	if banner := readBanner(t, smtpAddr); !strings.HasPrefix(banner, "220 mail.local.test ESMTP") {
		t.Fatalf("the SMTP banner was %q", banner)
	}
	cancel()

	select {
	case got := <-done:
		if got.code != codeOK {
			t.Fatalf("exit code = %d, want %d\nstderr: %s", got.code, codeOK, got.errOut)
		}
		for _, want := range []string{
			`"listener":"jmap"`,
			`"listener":"smtp"`,
			`"listener":"submission","reason":"no listen address is configured"`,
			// No certificate was configured, so the two implicit-TLS ports
			// could not have started even if they had been asked for.
			"STARTTLS cannot be offered",
			"maild stopped",
		} {
			if !strings.Contains(got.errOut, want) {
				t.Fatalf("the log is missing %q\n%s", want, got.errOut)
			}
		}
		if strings.Contains(got.out+got.errOut, token) {
			t.Fatal("the log carried the admin token")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the process did not stop after the signal")
	}
}

// A port that is already taken is a start failure, not a process that idles
// looking healthy.
func TestRunFailsWhenThePortIsTaken(t *testing.T) {
	t.Parallel()

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("take a port: %v", err)
	}
	defer func() { ignore(taken.Close()) }()

	_, errOut, code := invoke(t, context.Background(), []string{
		"--hostname", "mail.local.test",
		"--admin-token", token,
		"--data-dir", t.TempDir(),
		"--api-addr", taken.Addr().String(),
		"--smtp-addr", "", "--submission-addr", "", "--submissions-addr", "",
		"--pop3-addr", "", "--pop3s-addr", "",
	}, nil)
	if code != codeError {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, codeError, errOut)
	}
	if !strings.Contains(errOut, "bind jmap") {
		t.Fatalf("stderr = %q, want it to name the listener", errOut)
	}
}

// Nothing to serve is a refusal rather than an idle process, because a mail
// server with every listener off is a silent outage.
func TestRunRefusesWithNoListener(t *testing.T) {
	t.Parallel()

	_, errOut, code := invoke(t, context.Background(), []string{
		"--hostname", "mail.local.test",
		"--admin-token", token,
		"--data-dir", t.TempDir(),
		"--api-addr", "",
		"--smtp-addr", "", "--submission-addr", "", "--submissions-addr", "",
		"--pop3-addr", "", "--pop3s-addr", "",
	}, nil)
	if code != codeError {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, codeError, errOut)
	}
	if !strings.Contains(errOut, "no listener is enabled") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func invoke(t *testing.T, ctx context.Context, args []string, env map[string]string) (stdout, stderr string, code int) {
	t.Helper()
	out := &strings.Builder{}
	errOut := &strings.Builder{}
	code = run(ctx, args, func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}, out, errOut)
	return out.String(), errOut.String(), code
}

// readBanner dials and reads the greeting line a line protocol opens with.
func readBanner(t *testing.T, addr string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { ignore(conn.Close()) }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set a deadline: %v", err)
	}
	buffer := make([]byte, 128)
	n, err := conn.Read(buffer)
	if err != nil {
		t.Fatalf("read from %s: %v", addr, err)
	}
	return strings.TrimSpace(string(buffer[:n]))
}

// freeAddr returns a loopback address that was free a moment ago. Binding to
// port 0 would be sounder, but the address the process ends up on is not
// visible from outside it, and a test needs to know where to knock.
func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the probe port: %v", err)
	}
	return addr
}
