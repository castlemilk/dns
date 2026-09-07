package deephostmcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

// The one secret this server hands back is a mailbox password. It comes back
// once, with the instruction attached to the answer and not only to the tool
// description, because the answer is what a model reads afterwards.
func TestMailboxPasswordIsReturnedOnceWithItsInstruction(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	client := start(t, fakes.server(t, "operator-token"))

	answer := client.call("deephost_mail_mailbox_add", map[string]any{"domain": "acme.dev", "local_part": "mara"})
	if answer.IsError {
		t.Fatalf("the call was refused: %v", answer.StructuredContent)
	}
	if answer.StructuredContent["password"] != fakes.mail.password {
		t.Fatalf("the generated password is not in the answer: %v", answer.StructuredContent)
	}
	note := text(t, answer.StructuredContent, "password_note")
	for _, phrase := range []string{"once", "keep no copy", "deephost_mail_mailbox_reset"} {
		if !strings.Contains(note, phrase) {
			t.Errorf("the note does not mention %q: %q", phrase, note)
		}
	}

	// The tool description must carry the same rule, because that is what a
	// model reads before it decides to call it.
	definition := toolNamed(t, "deephost_mail_mailbox_add")
	description := definition.Description()
	for _, phrase := range []string{"once", "do not write it"} {
		if !strings.Contains(description, phrase) {
			t.Errorf("the description does not state the secret-handling rule (%q): %q", phrase, description)
		}
	}

	// Resetting a password says the same thing.
	reset := client.call("deephost_mail_mailbox_reset", map[string]any{"domain": "acme.dev", "address": "mara@acme.dev"})
	if reset.IsError {
		t.Fatalf("the reset was refused: %v", reset.StructuredContent)
	}
	if reset.StructuredContent["password"] != fakes.mail.password {
		t.Errorf("the new password is not in the answer: %v", reset.StructuredContent)
	}
	if note := text(t, reset.StructuredContent, "password_note"); !strings.Contains(note, "once") {
		t.Errorf("the reset answer has no secret-handling note: %q", note)
	}
	if len(fakes.mail.resetIDs) != 1 || fakes.mail.resetIDs[0] != "mb-1" {
		t.Errorf("reset the wrong mailbox: %v", fakes.mail.resetIDs)
	}
}

// Everything else that could be a secret goes one way only.
func TestSecretsNeverComeBack(t *testing.T) {
	t.Parallel()
	const token = "operator-token-that-must-not-appear"
	const value = "postgres://admin:hunter2@db.internal:5432/app"

	fakes := newPlatform()
	server := fakes.server(t, token)
	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deephost_site_env_set","arguments":{"domain":"acme.dev","name":"DATABASE_URL","value":"` + value + `"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"deephost_site_env_get","arguments":{"domain":"acme.dev"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"deephost_version","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`,
	}, "\n") + "\n"

	var out strings.Builder
	if err := server.Serve(context.Background(), strings.NewReader(requests), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if strings.Contains(out.String(), value) {
		t.Error("an environment variable value was echoed back to the caller")
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Error("part of an environment variable value was echoed back to the caller")
	}
	if strings.Contains(out.String(), token) {
		t.Error("the operator credential appeared in an answer")
	}
	// The value did reach the control plane: the tool is not silently doing
	// nothing.
	if len(fakes.hosting.envSet) != 1 || fakes.hosting.envSet[0].GetValue() != value {
		t.Fatalf("the value did not reach the hosting engine: %v", fakes.hosting.envSet)
	}
}

// An engine that is not configured is a refusal carrying the control plane's
// own sentence, not a crash and not a rewrite.
func TestAnEngineThatIsNotConfiguredIsReportedVerbatim(t *testing.T) {
	t.Parallel()
	const sentence = "hosting: the hosting engine is not configured; set HOSTING_API_URL and HOSTING_API_TOKEN"
	fakes := newPlatform()
	fakes.hosting.listErr = connect.NewError(connect.CodeFailedPrecondition, errors.New(sentence))
	client := start(t, fakes.server(t, "operator-token"))

	failure := refusal(t, client.call("deephost_site_list", nil))
	if failure["kind"] != KindControlPlane {
		t.Errorf("kind = %v, want %s", failure["kind"], KindControlPlane)
	}
	if failure["code"] != "failed_precondition" {
		t.Errorf("code = %v, want failed_precondition", failure["code"])
	}
	message := text(t, failure, "message")
	if !strings.Contains(message, sentence) {
		t.Errorf("the control plane's own sentence was lost: %q", message)
	}
}

// A credential the control plane rejects is reported as something an operator
// can fix, not as an unexplained failure.
func TestARejectedCredentialPointsAtLoggingInAgain(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	fakes.dns.listErr = connect.NewError(connect.CodeUnauthenticated, errors.New("unauthorized"))
	client := start(t, fakes.server(t, "stale-token"))

	failure := refusal(t, client.call("deephost_domain_list", nil))
	if failure["kind"] != KindCredentialRequired {
		t.Errorf("kind = %v, want %s", failure["kind"], KindCredentialRequired)
	}
	if message := text(t, failure, "message"); !strings.Contains(message, "deephost auth login") {
		t.Errorf("message = %q, want it to point at logging in again", message)
	}
}

// Unbinding mail with its mailboxes needs the domain name to have been typed:
// the control plane asks for it as confirmation, and filling it in from an id
// would quietly answer a question meant for a person.
func TestUnbindingMailboxesNeedsTheDomainNameTyped(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	client := start(t, fakes.server(t, "operator-token"))

	failure := refusal(t, client.call("deephost_mail_unbind", map[string]any{
		"zone_id": "z-1", "delete_mailboxes": true, "confirm": true,
	}))
	if failure["kind"] != KindInvalidArgument {
		t.Fatalf("kind = %v, want %s (%v)", failure["kind"], KindInvalidArgument, failure["message"])
	}
	if len(fakes.mail.unbound) != 0 {
		t.Fatal("the unbind reached the control plane without a confirmation")
	}

	answer := client.call("deephost_mail_unbind", map[string]any{
		"domain": "acme.dev", "delete_mailboxes": true, "confirm": true,
	})
	if answer.IsError {
		t.Fatalf("the named call was refused: %v", answer.StructuredContent)
	}
	if len(fakes.mail.unbound) != 1 || fakes.mail.unbound[0].GetConfirmZoneName() != "acme.dev" {
		t.Fatalf("the confirmation did not reach the control plane: %v", fakes.mail.unbound)
	}
}

// Deleting a mailbox carries the address the caller wrote, for the same
// reason.
func TestDeletingAMailboxCarriesTheAddressAsConfirmation(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	client := start(t, fakes.server(t, "operator-token"))

	failure := refusal(t, client.call("deephost_mail_mailbox_delete", map[string]any{
		"domain": "acme.dev", "mailbox_id": "mb-1", "confirm": true,
	}))
	if failure["kind"] != KindInvalidArgument {
		t.Fatalf("kind = %v, want %s (%v)", failure["kind"], KindInvalidArgument, failure["message"])
	}

	answer := client.call("deephost_mail_mailbox_delete", map[string]any{
		"domain": "acme.dev", "address": "mara@acme.dev", "confirm": true,
	})
	if answer.IsError {
		t.Fatalf("the call was refused: %v", answer.StructuredContent)
	}
	if len(fakes.mail.deleted) != 1 || fakes.mail.deleted[0].GetConfirmAddress() != "mara@acme.dev" {
		t.Fatalf("the confirmation did not reach the control plane: %v", fakes.mail.deleted)
	}
}

// A false that matters must survive the trip. `supported: false` on an
// environment listing means the engine could not be asked, which is a
// different thing from a site with no variables.
func TestFalseFieldsThatMatterAreNotOmitted(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	client := start(t, fakes.server(t, "operator-token"))

	answer := client.call("deephost_status", nil)
	if answer.IsError {
		t.Fatalf("the call was refused: %v", answer.StructuredContent)
	}
	engines, ok := answer.StructuredContent["engines"].([]any)
	if !ok || len(engines) != 2 {
		t.Fatalf("want two engines, got %v", answer.StructuredContent["engines"])
	}
	hosting := object(t, engines[1])
	configured, found := hosting["configured"]
	if !found {
		t.Fatal("an engine that is not configured must say so; the field was omitted")
	}
	if configured != false {
		t.Errorf("configured = %v, want false", configured)
	}
	if missing := list(t, hosting["missing_env"]); len(missing) != 1 || missing[0] != "HOSTING_API_URL" {
		t.Errorf("the environment variable names an operator must set were lost: %v", hosting["missing_env"])
	}
}

// Numbers wider than a float64 arrive as strings, so a byte count cannot be
// silently rounded on the way through this server.
func TestLargeCountsSurviveAsStrings(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	fakes.mail.mailboxes[0].UsedBytes = 9007199254740993
	client := start(t, fakes.server(t, "operator-token"))

	answer := client.call("deephost_mail_mailbox_list", map[string]any{"domain": "acme.dev"})
	if answer.IsError {
		t.Fatalf("the call was refused: %v", answer.StructuredContent)
	}
	encoded, err := json.Marshal(answer.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"9007199254740993"`) {
		t.Errorf("a 64-bit count did not survive the round trip: %s", encoded)
	}
}
