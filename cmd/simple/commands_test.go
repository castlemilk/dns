package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStatusIsHonest is the load-bearing test for the whole product's promise:
// an engine that is not configured says so, and names the environment
// variables it is waiting for without ever showing a value.
func TestStatusIsHonest(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withToken()
	h.mustRun("status")

	contains(t, h.out(), "dns", "the engine table")
	contains(t, h.out(), "not configured; set HOSTING_API_URL, HOSTING_API_TOKEN", "the hosting row")
	contains(t, h.out(), "not configured; set STRIPE_SECRET_KEY", "the billing row")
	contains(t, h.out(), "ns1.simple.test.", "the nameservers")

	h.mustRun("--json", "status", "--probe")
	if engines := field[[]any](t, h.decode(), "engines"); len(engines) != 4 {
		t.Fatalf("want four engines, got %v", engines)
	}
}

func TestDomainCommands(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withToken()

	h.mustRun("--json", "domain", "add", "acme.test")
	zone := field[map[string]any](t, h.decode(), "zone")
	if zone["name"] != "acme.test" {
		t.Fatalf("created zone = %v", zone["name"])
	}
	if len(h.plane.zones) != 2 {
		t.Fatalf("the plane has %d zones", len(h.plane.zones))
	}

	// A zone is addressable by name or by id, in any case, with or without the
	// trailing dot an operator copies out of a zone file.
	for _, reference := range []string{"example.com", "EXAMPLE.COM", "example.com.", "zone-1"} {
		h.mustRun("--json", "domain", "show", reference)
		if id := h.decode()["id"]; id != "zone-1" {
			t.Errorf("domain show %q resolved to %v", reference, id)
		}
	}

	h.mustRun("domain", "show", "example.com")
	contains(t, h.out(), "rec-1", "the record table")
	contains(t, h.out(), "hosting", "the source of the engine-owned record")

	h.mustRun("domain", "delete", "acme.test", "--yes")
	if len(h.plane.zones) != 1 {
		t.Fatalf("the zone was not deleted: %d left", len(h.plane.zones))
	}
}

func TestRecordCommands(t *testing.T) {
	t.Parallel()

	t.Run("add, edit and delete", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()

		h.mustRun("record", "add", "example.com", "--name", "api", "--type", "a", "--value", "203.0.113.9")
		seeded := h.plane.zones[0]
		added := seeded.Records[len(seeded.Records)-1]
		if added.GetName() != "api" || added.GetValue() != "203.0.113.9" || added.GetTtl() != defaultTTL {
			t.Fatalf("added record = %v", added)
		}

		// An edit that mentions one field must not blank the others.
		h.mustRun("record", "edit", "example.com", added.GetId(), "--ttl", "60")
		if added.GetTtl() != 60 || added.GetValue() != "203.0.113.9" || added.GetName() != "api" {
			t.Fatalf("the edit lost a field: %v", added)
		}
		if code := h.run("record", "edit", "example.com", added.GetId()); code != codeUsage {
			t.Fatalf("an edit with no change should be a usage error, got %d", code)
		}

		before := len(seeded.Records)
		h.mustRun("record", "delete", "example.com", added.GetId(), "--yes")
		if len(h.plane.zones[0].Records) != before-1 {
			t.Fatal("the record was not deleted")
		}
		if code := h.run("record", "delete", "example.com", "rec-nope", "--yes"); code != codeError {
			t.Fatalf("deleting a missing record = %d", code)
		}
		contains(t, h.err(), "record_id:", "the failure")
	})

	t.Run("list filters", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()

		h.mustRun("--json", "record", "list", "example.com", "--type", "cname")
		if records := field[[]any](t, h.decode(), "records"); len(records) != 1 {
			t.Fatalf("want one CNAME, got %v", records)
		}
		h.mustRun("--json", "record", "list", "example.com", "--name", "www")
		if records := field[[]any](t, h.decode(), "records"); len(records) != 1 {
			t.Fatalf("want one record at www, got %v", records)
		}
		h.mustRun("--json", "record", "list", "example.com", "--type", "mx")
		if records := field[[]any](t, h.decode(), "records"); len(records) != 0 {
			t.Fatalf("want no MX records, got %v", records)
		}
	})

	t.Run("import and export", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		file := writeTemp(t, "$ORIGIN acme.test.\n@ 300 IN A 203.0.113.1\n")

		h.mustRun("--json", "record", "import", "acme.test", "--file", file, "--dry-run")
		document := h.decode()
		if document["dry_run"] != true {
			t.Fatalf("dry_run = %v", document["dry_run"])
		}
		if len(h.plane.zones) != 1 {
			t.Fatal("a dry run wrote a zone")
		}

		// A replacing import destroys records, so it asks first.
		h.withStdin("n\n")
		if code := h.run("record", "import", "acme.test", "--file", file, "--mode", "replace"); code != codeError {
			t.Fatalf("a replacing import did not ask: %d", code)
		}
		contains(t, h.err(), "Replace every record", "the confirmation")

		h.mustRun("record", "import", "acme.test", "--file", file, "--mode", "replace", "--yes")
		contains(t, h.out(), "unsupported record type ignored", "the warnings")

		// Importing from stdin is the same command with -.
		h.withStdin("$ORIGIN other.test.\n")
		h.mustRun("record", "import", "other.test", "--file", "-")

		output := filepath.Join(t.TempDir(), "zone.txt")
		h.mustRun("record", "export", "example.com", "--output", output)
		raw, err := os.ReadFile(output) //nolint:gosec // a path this test just made
		if err != nil {
			t.Fatalf("read the export: %v", err)
		}
		if !strings.HasPrefix(string(raw), "$ORIGIN example.com.") {
			t.Fatalf("exported file = %q", raw)
		}

		// --output and --json together give both the file and the document.
		jsonOutput := filepath.Join(t.TempDir(), "zone.json.txt")
		h.mustRun("--json", "record", "export", "example.com", "--output", jsonOutput)
		if path := field[string](t, h.decode(), "path"); path != jsonOutput {
			t.Fatalf("path = %q, want %q", path, jsonOutput)
		}
		if _, err := os.Stat(jsonOutput); err != nil {
			t.Fatalf("--json --output wrote no file: %v", err)
		}
	})
}

func TestSiteCommands(t *testing.T) {
	t.Parallel()

	t.Run("attach dry run writes nothing", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("site", "attach", "example.com", "--framework", "nextjs", "--dry-run")
		contains(t, h.out(), "dry run", "the report")
		contains(t, h.out(), "dns plan", "the plan")
		if len(h.plane.sites) != 0 {
			t.Fatal("a dry run attached a site")
		}
	})

	t.Run("attach, deploy, logs, rollback and detach", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()

		h.mustRun("site", "attach", "example.com", "--framework", "node", "--www-mode", "redirect")
		site := h.plane.sites["zone-1"]
		if site == nil || site.GetFramework().String() != "FRAMEWORK_NODE" {
			t.Fatalf("attached site = %v", site)
		}

		h.mustRun("--json", "site", "deploy", "example.com", "--repository", "https://github.com/acme/site")
		deploy := field[map[string]any](t, h.decode(), "deploy")
		if deploy["phase"] != "DEPLOY_PHASE_QUEUED" {
			t.Fatalf("phase = %v", deploy["phase"])
		}
		contains(t, h.plane.deploys[0].GetSource().GetRepository(), "acme/site", "the deploy's repository")

		h.mustRun("site", "deploys", "example.com")
		contains(t, h.out(), "queued", "the deploy table")

		h.mustRun("site", "deploys", "example.com", "--deploy-id", h.plane.deploys[0].GetId())
		contains(t, h.out(), h.plane.deploys[0].GetId(), "the single deploy")
		if code := h.run("site", "deploys", "--deploy-id", "deploy-1"); code != codeUsage {
			t.Fatalf("a deploy id without a domain = %d", code)
		}

		h.mustRun("site", "update", "example.com", "--repository", "https://github.com/acme/other")
		if got := h.plane.sites["zone-1"].GetRepository(); got != "https://github.com/acme/other" {
			t.Fatalf("repository = %q", got)
		}
		if code := h.run("site", "update", "example.com"); code != codeUsage {
			t.Fatalf("an update with nothing to change = %d", code)
		}

		h.mustRun("site", "rollback", "example.com", "--deploy-id", h.plane.deploys[0].GetId())
		contains(t, h.out(), "rollback", "the rollback report")

		h.withStdin("y\n")
		h.mustRun("site", "detach", "example.com")
		if len(h.plane.sites) != 0 {
			t.Fatal("the site survived a confirmed detach")
		}
	})

	t.Run("a git token comes from the environment, never a flag", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("site", "attach", "example.com", "--framework", "static")
		h.environ[envGitToken] = "ghp_secret_value"

		h.mustRun("site", "deploy", "example.com", "--repository", "https://github.com/acme/private")
		if !h.plane.deploys[0].GetSource().GetPrivateRepository() {
			t.Fatal("the git token did not reach the control plane")
		}
		absent(t, h.out(), "ghp_secret_value", "stdout")
		absent(t, h.err(), "ghp_secret_value", "stderr")

		// There is no flag that would put it in the process list.
		if code := h.run("site", "deploy", "example.com", "--git-token", "ghp_secret_value"); code != codeUsage {
			t.Fatalf("--git-token was accepted (exit %d)", code)
		}
	})

	t.Run("environment variables are write-only", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("site", "attach", "example.com", "--framework", "static")

		h.withStdin("s3cret-value\n")
		h.mustRun("site", "env", "set", "example.com", "API_KEY")
		absent(t, h.out(), "s3cret-value", "stdout")
		absent(t, h.err(), "s3cret-value", "stderr")
		contains(t, h.err(), "until the next deploy", "the control plane's note")

		h.mustRun("site", "env", "get", "example.com")
		contains(t, h.out(), "API_KEY", "the variable list")
		contains(t, h.out(), "write-only", "the explanation")
		absent(t, h.out(), "s3cret-value", "the variable list")

		h.mustRun("site", "env", "set", "example.com", "OTHER", "--value-file", writeTemp(t, "from-a-file"))
		absent(t, h.out(), "from-a-file", "stdout")

		h.mustRun("site", "env", "unset", "example.com", "API_KEY", "--yes")
		for _, variable := range h.plane.envVars["zone-1"] {
			if variable.GetName() == "API_KEY" {
				t.Fatal("the variable was not unset")
			}
		}
	})

	t.Run("www and gateway", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("site", "attach", "example.com", "--framework", "static")

		h.mustRun("site", "www", "example.com", "--mode", "redirect")
		if h.plane.sites["zone-1"].GetWwwMode().String() != "WWW_MODE_REDIRECT" {
			t.Fatalf("www mode = %v", h.plane.sites["zone-1"].GetWwwMode())
		}
		if code := h.run("site", "www", "example.com"); code != codeUsage {
			t.Fatalf("a missing --mode should be a usage error, got %d", code)
		}

		h.mustRun("site", "gateway")
		contains(t, h.out(), "203.0.113.20,203.0.113.21", "the pending address advice")

		h.mustRun("site", "gateway", "--confirm", "203.0.113.20,203.0.113.21", "--yes")
		contains(t, h.out(), "sites queued", "the confirmation report")

		h.mustRun("site", "reapply-dns", "example.com", "--dry-run")
		contains(t, h.out(), "dns plan", "the plan")
	})

	t.Run("a zone with no site says what to do about it", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		if code := h.run("site", "show", "example.com"); code != codeError {
			t.Fatalf("exit code = %d", code)
		}
		contains(t, h.err(), "not found", "the failure")
	})
}

func TestMailCommands(t *testing.T) {
	t.Parallel()

	t.Run("bind, mailboxes, forwarders and unbind", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()

		h.mustRun("mail", "bind", "example.com", "--dmarc-policy", "quarantine")
		domain := h.plane.mailbound["zone-1"]
		if domain.GetDmarcPolicy() != "quarantine" {
			t.Fatalf("dmarc policy = %q", domain.GetDmarcPolicy())
		}
		// Absent means the platform default, which is on: the CLI must not opt
		// a binding out by sending false for a flag nobody set.
		if !domain.GetPublishClientAutoconfig() {
			t.Fatal("client autoconfig was turned off by default")
		}

		h.mustRun("mail", "update", "example.com", "--client-autoconfig=false")
		if h.plane.mailbound["zone-1"].GetPublishClientAutoconfig() {
			t.Fatal("client autoconfig was not turned off")
		}
		if code := h.run("mail", "update", "example.com"); code != codeUsage {
			t.Fatalf("an update with nothing to change = %d", code)
		}

		h.mustRun("mail", "mailbox", "add", "example.com", "ben", "--display-name", "Ben")
		contains(t, h.out(), "correct-horse-battery-staple", "the password, shown once")
		contains(t, h.err(), "shown once", "the warning")

		h.mustRun("mail", "mailbox", "list", "example.com")
		contains(t, h.out(), "ben@example.com", "the mailbox list")

		// A mailbox is addressable by address, local part or id.
		for _, reference := range []string{"ben@example.com", "ben", h.plane.mailboxes["zone-1"][0].GetId()} {
			h.mustRun("mail", "mailbox", "edit", "example.com", reference, "--display-name", "Ben E")
		}

		h.mustRun("mail", "mailbox", "reset", "example.com", "ben", "--yes")
		contains(t, h.out(), "fresh-password-1", "the new password")
		contains(t, h.err(), "shown once", "the warning")

		h.mustRun("mail", "forwarder", "add", "example.com", "hello",
			"--target", "ben@example.com", "--target", "someone@example.net")
		forwarder := h.plane.forwarder["zone-1"][0]
		if len(forwarder.GetTargets()) != 2 {
			t.Fatalf("targets = %v", forwarder.GetTargets())
		}
		if code := h.run("mail", "forwarder", "add", "example.com", "empty"); code != codeUsage {
			t.Fatalf("a forwarder with no target = %d", code)
		}
		h.mustRun("mail", "forwarder", "delete", "example.com", forwarder.GetId(), "--yes")

		// Mailboxes exist, so an unbind that keeps them is refused by the
		// control plane and the refusal is passed through.
		if code := h.run("mail", "unbind", "example.com", "--yes"); code != codeError {
			t.Fatalf("unbind with mailboxes = %d", code)
		}
		contains(t, h.err(), "delete_mailboxes:", "the control plane's refusal")

		h.mustRun("mail", "unbind", "example.com", "--delete-mailboxes", "--yes")
		if len(h.plane.mailbound) != 0 {
			t.Fatal("the domain is still bound")
		}
	})

	t.Run("counts that were not read are not shown as zero", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("mail", "status")
		contains(t, h.out(), "unknown", "the mailbox count the engine could not be asked for")
		contains(t, h.out(), "not configured; set MAIL_API_URL", "the engine row")
	})

	t.Run("show and queue", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t).withToken()
		h.mustRun("mail", "bind", "example.com")

		h.mustRun("mail", "show", "example.com")
		contains(t, h.out(), "example.com", "the domain")
		contains(t, h.out(), "1 scheduled", "the queue summary")

		h.mustRun("mail", "queue", "example.com")
		contains(t, h.out(), "msg-1", "the queued message")

		h.mustRun("mail", "reapply-dns", "example.com", "--dry-run")
		contains(t, h.out(), "dry run", "the report")
	})
}

func TestBillingCommands(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withToken()

	h.mustRun("billing", "status")
	contains(t, h.out(), "$6 / domain / month", "the price")
	contains(t, h.out(), "not configured, so subscription changes", "the webhook honesty")
	contains(t, h.out(), "nothing here changes what the platform serves", "the policy note")

	h.mustRun("billing", "summary")
	contains(t, h.out(), "unbilled", "the summary row")

	h.mustRun("billing", "subscribe", "example.com")
	contains(t, h.out(), "https://checkout.test/session", "the checkout URL")
	contains(t, h.out(), "simple billing confirm cs_test_1", "the next step")

	h.mustRun("billing", "confirm", "cs_test_1")
	contains(t, h.out(), "active", "the subscription state")

	h.mustRun("billing", "portal", "--flow", "payment_method_update")
	contains(t, h.out(), "https://portal.test/session", "the portal URL")

	h.mustRun("billing", "invoices")
	contains(t, h.out(), "INV-001", "the invoice")
	contains(t, h.out(), "6.00 USD", "the amount")

	h.mustRun("billing", "retry-webhooks")
	contains(t, h.out(), "2", "the re-queued count")
}

func TestActivityCommands(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withToken()

	h.mustRun("activity")
	contains(t, h.out(), "zone.created", "the event")

	h.mustRun("activity", "--domain", "example.com", "--kind", "zone.")
	contains(t, h.out(), "zone.created", "the prefix-filtered event")

	h.mustRun("activity", "--kind", "deploy.succeeded")
	contains(t, h.out(), "no events matched", "the empty result")

	// The clock is fake, so a relative --since is exact: the seeded event is
	// two hours old.
	h.mustRun("activity", "--since", "3h")
	contains(t, h.out(), "zone.created", "an event inside the window")
	h.mustRun("activity", "--since", "1h")
	contains(t, h.out(), "no events matched", "an event outside the window")
	h.mustRun("activity", "--since", "2026-09-04T00:00:00Z")
	contains(t, h.out(), "zone.created", "an absolute --since")

	if code := h.run("activity", "--since", "yesterday"); code != codeUsage {
		t.Fatalf("an unparsable --since = %d", code)
	}

	h.mustRun("activity", "show", "0000019a2f-0001")
	contains(t, h.out(), "example.com was created", "the summary")
	contains(t, h.out(), "zone:", "the details")

	if code := h.run("activity", "show", "no-such-event"); code != codeError {
		t.Fatalf("a missing event = %d", code)
	}
}

func TestPlatformRebuild(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withToken()

	// A dry run changes nothing, so it does not ask.
	h.mustRun("platform", "rebuild", "--dry-run")
	contains(t, h.out(), "dry run", "the report")
	contains(t, h.out(), "has no matching zone", "the warnings")

	h.withStdin("y\n")
	h.mustRun("platform", "rebuild")
	contains(t, h.err(), "[y/N]", "the confirmation")
}
