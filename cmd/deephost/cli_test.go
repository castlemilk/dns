package main

import (
	"errors"
	"strings"

	"github.com/castlemilk/dns/internal/brand"
	"testing"

	"connectrpc.com/connect"
)

// TestExitCodes pins the promise the docs make: 0 ok, 1 error, 2 usage. A
// scripted caller distinguishes "you asked for the wrong thing" from "the thing
// you asked for went wrong" on this and nothing else.
func TestExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		args   []string
		want   int
		stdout string // a fragment stdout must contain
		stderr string // a fragment stderr must contain
	}{
		{name: "no arguments greet", args: nil, want: codeOK, stdout: "usage: deephost"},
		{name: "help", args: []string{"help"}, want: codeOK, stdout: "groups:"},
		{name: "help flag", args: []string{"--help"}, want: codeOK, stdout: "usage: deephost"},
		{name: "version", args: []string{"version"}, want: codeOK, stdout: "v1.2.3-test"},
		{
			name: "unknown command", args: []string{"teleport"},
			want: codeUsage, stderr: `unknown command "teleport"`,
		},
		{
			name: "group without command", args: []string{"domain"},
			want: codeUsage, stderr: "choose list, add, show or delete",
		},
		{
			name: "unknown subcommand", args: []string{"domain", "frobnicate"},
			want: codeUsage, stderr: `unknown domain command "frobnicate"`,
		},
		{
			name: "unknown flag", args: []string{"domain", "list", "--nope"},
			want: codeUsage, stderr: "flag provided but not defined",
		},
		{
			name: "missing positional", args: []string{"domain", "show"},
			want: codeUsage, stderr: "domain: is required",
		},
		{
			name: "extra positional", args: []string{"domain", "list", "spare"},
			want: codeUsage, stderr: `unexpected argument "spare"`,
		},
		{
			name: "bad record type", args: []string{"record", "add", "example.com", "--name", "a", "--type", "QQ", "--value", "x"},
			want: codeUsage, stderr: "--type: must be one of",
		},
		{
			name: "non-positive timeout", args: []string{"--timeout", "0s", "domain", "list"},
			want: codeUsage, stderr: "--timeout: must be positive",
		},
		{
			name: "help for a group is not a failure", args: []string{"site", "--help"},
			want: codeOK, stdout: "deephost site attach",
		},
		{
			name: "unknown domain is an error, not a usage mistake",
			args: []string{"domain", "show", "nowhere.test"},
			want: codeError, stderr: `"nowhere.test" is not a zone`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t).withToken()
			if got := h.run(test.args...); got != test.want {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s",
					got, test.want, h.out(), h.err())
			}
			if test.stdout != "" {
				contains(t, h.out(), test.stdout, "stdout")
			}
			if test.stderr != "" {
				contains(t, h.err(), test.stderr, "stderr")
			}
		})
	}
}

// TestGlobalFlagsAreAcceptedBeforeAndAfterTheCommand keeps `deephost --json
// domain list` and `deephost domain list --json` the same command.
func TestGlobalFlagsAreAcceptedBeforeAndAfterTheCommand(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withToken()

	h.mustRun("--json", "domain", "list")
	leading := h.out()
	h.mustRun("domain", "list", "--json")
	trailing := h.out()

	if leading != trailing {
		t.Fatalf("flag position changed the output:\nleading:  %s\ntrailing: %s", leading, trailing)
	}
	if !strings.Contains(leading, `"zones"`) {
		t.Fatalf("the zones document is missing: %s", leading)
	}
}

// TestJSONContract is the shape a script depends on: one document on stdout,
// nothing else on stdout, and a failure as {"error": …} on stderr.
func TestJSONContract(t *testing.T) {
	t.Parallel()

	t.Run("success is one document on stdout", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("--json", "domain", "list")
		document := h.decode()
		zones, ok := document["zones"].([]any)
		if !ok || len(zones) != 1 {
			t.Fatalf("zones missing from %v", document)
		}
		first, ok := zones[0].(map[string]any)
		if !ok {
			t.Fatalf("a zone is not an object: %v", zones[0])
		}
		// Proto field names, not Go or JSON-camel ones: the CLI's document is
		// the wire message.
		for _, name := range []string{"id", "name", "serial", "records", "nameservers"} {
			if _, present := first[name]; !present {
				t.Errorf("zone has no %q field: %v", name, first)
			}
		}
		absent(t, h.out(), brand.Wordmark+"\n", "a JSON run's stdout (the brand mark belongs to text mode)")
	})

	t.Run("failure is the error envelope on stderr", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.plane.failWith("/dns.v1.DNSService/ListZones",
			connect.NewError(connect.CodeUnavailable, errors.New("engine down")))

		if code := h.run("--json", "domain", "list"); code != codeError {
			t.Fatalf("exit code = %d, want %d", code, codeError)
		}
		if got := strings.TrimSpace(h.out()); got != "" {
			t.Fatalf("stdout must stay empty on failure, got %q", got)
		}
		contains(t, h.decodeError(), "not reachable", "the JSON error")
	})

	t.Run("every group answers --json", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("site", "attach", "example.com", "--framework", "static")
		h.mustRun("mail", "bind", "example.com")

		commands := [][]string{
			{"status"},
			{"version"},
			{"domain", "list"},
			{"domain", "show", "example.com"},
			{"record", "list", "example.com"},
			{"record", "export", "example.com"},
			{"site", "list"},
			{"site", "show", "example.com"},
			{"site", "deploys"},
			{"site", "env", "get", "example.com"},
			{"site", "gateway"},
			{"mail", "status"},
			{"mail", "list"},
			{"mail", "show", "example.com"},
			{"mail", "mailbox", "list", "example.com"},
			{"mail", "forwarder", "list", "example.com"},
			{"mail", "queue", "example.com"},
			{"billing", "status"},
			{"billing", "summary"},
			{"billing", "invoices"},
			{"activity"},
			{"auth", "status"},
		}
		for _, command := range commands {
			args := append([]string{"--json"}, command...)
			if code := h.run(args...); code != codeOK {
				t.Errorf("deephost %s: exit %d\n%s", strings.Join(args, " "), code, h.err())
				continue
			}
			if strings.TrimSpace(h.out()) == "" {
				t.Errorf("deephost %s wrote no document", strings.Join(args, " "))
				continue
			}
			h.decode()
		}
	})
}

// TestTextOutputIsBrandedAndPlain proves the one-line mark is printed for a
// human and that a redirected stream never receives an escape byte.
func TestTextOutputIsBrandedAndPlain(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withToken()
	h.mustRun("domain", "list")

	if !strings.HasPrefix(h.out(), brand.Wordmark+"\n") {
		t.Fatalf("text output does not start with the brand mark:\n%s", h.out())
	}
	if strings.ContainsRune(h.out(), 0x1b) {
		t.Fatal("text output written to a buffer contains an ANSI escape")
	}
	contains(t, h.out(), "example.com", "the zone table")
	contains(t, h.out(), "ZONE ID", "the zone table header")
}

// TestRawOutputIsNotBranded keeps the commands whose stdout is data — a zone
// file, a build log — free of anything a caller would have to strip.
func TestRawOutputIsNotBranded(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withToken()
	h.mustRun("site", "attach", "example.com", "--framework", "static")

	h.mustRun("record", "export", "example.com")
	if !strings.HasPrefix(h.out(), "$ORIGIN example.com.") {
		t.Fatalf("the zone file has something in front of it:\n%s", h.out())
	}

	h.mustRun("site", "deploy", "example.com")
	h.mustRun("site", "logs", "example.com")
	if h.out() != "npm install\nbuild succeeded\n" {
		t.Fatalf("log output is not the log:\n%s", h.out())
	}
	contains(t, h.err(), "snapshot log", "the log's provenance note on stderr")
}

// TestDestructiveCommandsConfirm covers the three ways a destructive command
// can end: refused, accepted, and refused-because-nobody-could-be-asked.
func TestDestructiveCommandsConfirm(t *testing.T) {
	t.Parallel()

	t.Run("a no leaves the zone alone", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken().withStdin("n\n")
		if code := h.run("domain", "delete", "example.com"); code != codeError {
			t.Fatalf("exit code = %d, want %d", code, codeError)
		}
		contains(t, h.err(), "cancelled", "the refusal")
		if len(h.plane.zones) != 1 {
			t.Fatal("the zone was deleted despite the refusal")
		}
	})

	t.Run("a yes deletes it", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken().withStdin("y\n")
		h.mustRun("domain", "delete", "example.com")
		if len(h.plane.zones) != 0 {
			t.Fatal("the zone survived a confirmed delete")
		}
	})

	t.Run("--yes needs no prompt", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("domain", "delete", "example.com", "--yes")
		if len(h.plane.zones) != 0 {
			t.Fatal("the zone survived --yes")
		}
	})

	t.Run("--json refuses to guess", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		if code := h.run("--json", "domain", "delete", "example.com"); code != codeUsage {
			t.Fatalf("exit code = %d, want %d", code, codeUsage)
		}
		contains(t, h.decodeError(), "--yes", "the refusal")
		if len(h.plane.zones) != 1 {
			t.Fatal("the zone was deleted without a confirmation")
		}
	})

	t.Run("every destructive command asks", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("site", "attach", "example.com", "--framework", "static")
		h.mustRun("mail", "bind", "example.com")
		h.mustRun("mail", "mailbox", "add", "example.com", "ben")
		h.mustRun("mail", "forwarder", "add", "example.com", "hello", "--target", "ben@example.com")
		h.mustRun("site", "env", "set", "example.com", "API_URL", "--value-file", writeTemp(t, "value"))

		destructive := [][]string{
			{"domain", "delete", "example.com"},
			{"record", "delete", "example.com", "rec-1"},
			{"site", "detach", "example.com"},
			{"site", "env", "unset", "example.com", "API_URL"},
			{"mail", "unbind", "example.com"},
			{"mail", "mailbox", "delete", "example.com", "ben@example.com"},
			{"mail", "mailbox", "reset", "example.com", "ben@example.com"},
			{"mail", "forwarder", "delete", "example.com", "fwd-4"},
			{"platform", "rebuild"},
		}
		for _, command := range destructive {
			h.withStdin("n\n")
			if code := h.run(command...); code != codeError {
				t.Errorf("deephost %s: exit %d, want a refusal", strings.Join(command, " "), code)
				continue
			}
			contains(t, h.err(), "[y/N]", "deephost "+strings.Join(command, " ")+" prompt")
		}
	})
}

// TestErrorMapping walks every Connect code the control plane can answer with
// and asserts the sentence an operator is given. Validation text is passed
// through verbatim, because it already names the field.
func TestErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		code  connect.Code
		text  string
		want  string
		exit  int
		check func(t *testing.T, h *harness)
	}{
		{
			name: "invalid argument keeps field: message",
			code: connect.CodeInvalidArgument, text: "name: must be a valid domain",
			want: "name: must be a valid domain", exit: codeError,
		},
		{
			name: "not found passes through",
			code: connect.CodeNotFound, text: "zone: not found",
			want: "zone: not found", exit: codeError,
		},
		{
			name: "already exists passes through",
			code: connect.CodeAlreadyExists, text: "name: already exists",
			want: "name: already exists", exit: codeError,
		},
		{
			name: "failed precondition passes through",
			code: connect.CodeFailedPrecondition, text: "delete_mailboxes: is required",
			want: "delete_mailboxes: is required", exit: codeError,
		},
		{
			name: "unauthenticated points at login",
			code: connect.CodeUnauthenticated, text: "no",
			want: "refused the credential", exit: codeError,
		},
		{
			name: "permission denied is explained",
			code: connect.CodePermissionDenied, text: "no",
			want: "not allowed", exit: codeError,
		},
		{
			name: "unavailable names the control plane",
			code: connect.CodeUnavailable, text: "down",
			want: "not reachable", exit: codeError,
		},
		{
			name: "unimplemented suggests an older build",
			code: connect.CodeUnimplemented, text: "no",
			want: "older build", exit: codeError,
		},
		{
			name: "internal points at the server logs",
			code: connect.CodeInternal, text: "boom",
			want: "check its logs", exit: codeError,
		},
		{
			name: "resource exhausted mentions rate limiting",
			code: connect.CodeResourceExhausted, text: "",
			want: "rate limiting", exit: codeError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t).withToken()
			h.plane.failWith("/dns.v1.DNSService/ListZones",
				connect.NewError(test.code, errors.New(test.text)))

			if code := h.run("domain", "list"); code != test.exit {
				t.Fatalf("exit code = %d, want %d (%s)", code, test.exit, h.err())
			}
			contains(t, h.err(), test.want, "the failure")
			// However it failed, the credential is not in the message.
			absent(t, h.err(), testToken, "the failure")
		})
	}
}

// TestCredentialTravelsOnTheHeader proves the token reaches the control plane
// as a bearer header, and that a wrong one is reported as something an operator
// can fix.
func TestCredentialTravelsOnTheHeader(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withToken()
	h.plane.requireToken = testToken

	h.mustRun("domain", "list")
	if got := h.plane.seenAuthorization; got != "Bearer "+testToken {
		t.Fatalf("Authorization header = %q", got)
	}
	absent(t, h.out(), testToken, "stdout")

	if code := h.run("--token", "not-the-token", "domain", "list"); code != codeError {
		t.Fatalf("a wrong credential should fail, got exit %d", code)
	}
	contains(t, h.err(), "run `deephost auth login`", "the rejection")
	absent(t, h.err(), "not-the-token", "the rejection")
}

// TestTransportFailureIsExplained covers the failures that never reach Connect
// at all: nothing is listening on the address the operator gave.
func TestTransportFailureIsExplained(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withToken()
	h.server.Close()

	if code := h.run("domain", "list"); code != codeError {
		t.Fatalf("exit code = %d, want %d", code, codeError)
	}
	contains(t, h.err(), "not reachable", "the failure")
}

// TestInvalidSettingsAreRejectedBeforeAnyCall keeps a plain-HTTP URL to a
// non-loopback host from putting the credential on the wire in clear text.
func TestInvalidSettingsAreRejectedBeforeAnyCall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "http to a public host",
			args: []string{"--api-url", "http://api.example.com", "domain", "list"},
			want: "api_url: must use https",
		},
		{
			name: "not a URL",
			args: []string{"--api-url", "not-a-url", "domain", "list"},
			want: "api_url: must be an absolute http or https URL",
		},
		{
			name: "credential with a newline in it",
			args: []string{"--token", "abc\ndef", "domain", "list"},
			want: "token: must contain only printable ASCII",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t).withToken()
			if code := h.run(test.args...); code != codeError {
				t.Fatalf("exit code = %d, want %d", code, codeError)
			}
			contains(t, h.err(), test.want, "the rejection")
			if len(h.plane.seenProcedures) != 0 {
				t.Fatalf("the control plane was called anyway: %v", h.plane.seenProcedures)
			}
		})
	}
}

// TestHelpIsADocumentInJSONMode keeps --json's promise even for --help: a
// caller that always parses stdout as JSON never has to special-case it.
func TestHelpIsADocumentInJSONMode(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withToken()
	for _, args := range [][]string{
		{"--json", "--help"},
		{"--json", "mail", "--help"},
		{"--json"},
	} {
		if code := h.run(args...); code != codeOK {
			t.Fatalf("deephost %s: exit %d", strings.Join(args, " "), code)
		}
		if usage := field[string](t, h.decode(), "usage"); !strings.Contains(usage, "usage: deephost") {
			t.Fatalf("deephost %s: usage = %q", strings.Join(args, " "), usage)
		}
	}
}
