package simplemcp

import (
	"strings"
	"testing"
)

func TestEveryToolIsWellFormed(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, definition := range registry {
		t.Run(definition.Name, func(t *testing.T) {
			if !strings.HasPrefix(definition.Name, "simple_") {
				t.Errorf("name %q does not mirror a `simple` command", definition.Name)
			}
			if definition.Name != strings.ToLower(definition.Name) {
				t.Errorf("name %q is not lower case", definition.Name)
			}
			if seen[definition.Name] {
				t.Errorf("name %q is registered twice", definition.Name)
			}
			seen[definition.Name] = true
			if definition.Handler == nil {
				t.Error("has no handler")
			}
			if definition.Summary == "" {
				t.Error("has no description")
			}
			schema := definition.inputSchema()
			properties := object(t, schema["properties"])
			for _, required := range definition.Required {
				if _, described := properties[required]; !described {
					t.Errorf("requires %q but does not describe it", required)
				}
			}
			if definition.Destructive && !definition.Mutates {
				t.Error("is destructive but not marked as mutating, so it would run without a credential")
			}
			if definition.Local && definition.Mutates {
				t.Error("is answered locally but claims to mutate")
			}
		})
	}
}

// The contract a caller relies on: a tool that can destroy something says so
// in the description it is chosen from, and takes the confirmation in its
// schema. Neither can be forgotten, because both are added from the same flag.
func TestDestructiveToolsAskForConfirmationInSchemaAndDescription(t *testing.T) {
	t.Parallel()
	destructive := 0
	for _, definition := range registry {
		if !definition.needsConfirmation() {
			if _, found := object(t, definition.inputSchema()["properties"])["confirm"]; found {
				t.Errorf("%s takes a confirm argument but never asks for one", definition.Name)
			}
			continue
		}
		destructive++
		t.Run(definition.Name, func(t *testing.T) {
			description := definition.Description()
			if !strings.Contains(description, "confirm: true") {
				t.Errorf("description does not say confirm: true is required: %q", description)
			}
			properties := object(t, definition.inputSchema()["properties"])
			confirm, described := properties["confirm"].(map[string]any)
			if !described {
				t.Fatal("schema has no confirm property")
			}
			if confirm["type"] != "boolean" {
				t.Errorf("confirm is %v, want a boolean", confirm["type"])
			}
		})
	}
	// The three the specification names by hand, plus the ones that follow the
	// same rule, must all be here; a registry that lost them would still pass
	// the loop above.
	for _, name := range []string{
		"simple_domain_delete", "simple_record_delete", "simple_site_detach",
		"simple_site_env_unset", "simple_mail_unbind", "simple_mail_mailbox_delete",
		"simple_mail_forwarder_delete", "simple_record_import",
	} {
		if !toolNamed(t, name).needsConfirmation() {
			t.Errorf("%s destroys data but does not ask for confirmation", name)
		}
	}
	if destructive == 0 {
		t.Fatal("no tool asks for confirmation, so the contract is not being tested")
	}
}

func TestReadOnlyHintMatchesWhetherTheToolWrites(t *testing.T) {
	t.Parallel()
	for _, described := range describeTools() {
		definition := toolNamed(t, text(t, described, "name"))
		annotations := object(t, described["annotations"])
		if annotations["readOnlyHint"] != !definition.Mutates {
			t.Errorf("%s: readOnlyHint = %v, Mutates = %v", definition.Name, annotations["readOnlyHint"], definition.Mutates)
		}
		if annotations["destructiveHint"] != definition.Destructive {
			t.Errorf("%s: destructiveHint = %v, Destructive = %v", definition.Name, annotations["destructiveHint"], definition.Destructive)
		}
	}
}

func TestZoneIsIdentifiedByNameOrID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		arguments map[string]any
		wantKind  string
		wantZone  string
	}{
		{name: "by name", arguments: map[string]any{"domain": "acme.dev"}, wantZone: "z-1"},
		{name: "by name in a different case", arguments: map[string]any{"domain": "ACME.dev"}, wantZone: "z-1"},
		{name: "by fully qualified name", arguments: map[string]any{"domain": "acme.dev."}, wantZone: "z-1"},
		{name: "by id", arguments: map[string]any{"zone_id": "z-1"}, wantZone: "z-1"},
		{name: "unknown name", arguments: map[string]any{"domain": "nope.dev"}, wantKind: KindNotFound},
		{name: "unknown id", arguments: map[string]any{"zone_id": "z-9"}, wantKind: KindNotFound},
		{name: "neither", arguments: map[string]any{}, wantKind: KindInvalidArgument},
		{name: "wrong type", arguments: map[string]any{"domain": 7}, wantKind: KindInvalidArgument},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakes := newPlatform()
			client := start(t, fakes.server(t, "operator-token"))
			arguments := map[string]any{"confirm": true}
			for key, value := range test.arguments {
				arguments[key] = value
			}
			answer := client.call("simple_domain_delete", arguments)
			if test.wantKind != "" {
				failure := refusal(t, answer)
				if failure["kind"] != test.wantKind {
					t.Fatalf("kind = %v, want %s (%v)", failure["kind"], test.wantKind, failure["message"])
				}
				if len(fakes.dns.deletedZones) != 0 {
					t.Errorf("a refused call still reached the control plane: %v", fakes.dns.deletedZones)
				}
				return
			}
			if answer.IsError {
				t.Fatalf("the call was refused: %v", answer.StructuredContent)
			}
			if len(fakes.dns.deletedZones) != 1 || fakes.dns.deletedZones[0] != test.wantZone {
				t.Fatalf("deleted %v, want [%s]", fakes.dns.deletedZones, test.wantZone)
			}
		})
	}
}

func TestArgumentsAreRejectedByFieldName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		tool      string
		arguments map[string]any
		want      string
	}{
		{name: "missing required", tool: "simple_record_add", arguments: map[string]any{"domain": "acme.dev", "type": "A", "value": "203.0.113.1"}, want: "name: required"},
		{name: "unknown record type", tool: "simple_record_add", arguments: map[string]any{"domain": "acme.dev", "name": "@", "type": "PTR", "value": "x"}, want: "type: must be one of"},
		{name: "ttl is not a number", tool: "simple_record_add", arguments: map[string]any{"domain": "acme.dev", "name": "@", "type": "A", "value": "203.0.113.1", "ttl": "soon"}, want: "ttl: must be a whole number"},
		{name: "unknown framework", tool: "simple_site_attach", arguments: map[string]any{"domain": "acme.dev", "framework": "rails"}, want: "framework: must be one of"},
		{name: "unknown www mode", tool: "simple_site_www", arguments: map[string]any{"domain": "acme.dev", "mode": "sideways"}, want: "mode: must be serve or redirect"},
		{name: "since is not a timestamp", tool: "simple_activity_list", arguments: map[string]any{"since": "yesterday"}, want: "since: must be an RFC 3339 timestamp"},
		{name: "targets are not strings", tool: "simple_mail_forwarder_add", arguments: map[string]any{"domain": "acme.dev", "local_part": "team", "targets": "mara@acme.dev"}, want: "targets: must be an array of strings"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakes := newPlatform()
			client := start(t, fakes.server(t, "operator-token"))
			failure := refusal(t, client.call(test.tool, test.arguments))
			if failure["kind"] != KindInvalidArgument {
				t.Errorf("kind = %v, want %s", failure["kind"], KindInvalidArgument)
			}
			message := text(t, failure, "message")
			if !strings.Contains(message, test.want) {
				t.Errorf("message = %q, want it to contain %q", message, test.want)
			}
		})
	}
}

// A zone import that replaces a zone's records destroys them, and one that
// only reports what it would do destroys nothing. The confirmation follows the
// call rather than the tool.
func TestImportAsksForConfirmationOnlyWhenItReplaces(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		arguments   map[string]any
		wantRefused bool
	}{
		{name: "create", arguments: map[string]any{"mode": "create"}},
		{name: "default mode", arguments: map[string]any{}},
		{name: "replace as a dry run", arguments: map[string]any{"mode": "replace", "dry_run": true}},
		{name: "replace", arguments: map[string]any{"mode": "replace"}, wantRefused: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakes := newPlatform()
			server := fakes.server(t, "operator-token")
			arguments := map[string]any{"name": "acme.dev", "zone_file": "@ 300 IN A 203.0.113.1\n"}
			for key, value := range test.arguments {
				arguments[key] = value
			}
			definition := toolNamed(t, "simple_record_import")
			raw := args{}
			for key, value := range arguments {
				raw[key] = mustJSON(t, value)
			}
			err := server.permit(definition, raw)
			if test.wantRefused {
				var failure *toolError
				if !asToolError(err, &failure) || failure.Kind != KindConfirmationRequired {
					t.Fatalf("permit returned %v, want a confirmation_required refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("permit refused a call that destroys nothing: %v", err)
			}
		})
	}
}
