package mail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/internal/mail/stalwart"
	"github.com/castlemilk/dns/internal/zone"
	"google.golang.org/protobuf/proto"
)

// The mail integration test runs against a real Stalwart server. It carries no
// build tag on purpose — it compiles under `go test ./...`, `go vet` and
// golangci-lint, and skips itself when the engine is not configured. Start one
// with scripts/dev/stalwart-up.sh, then:
//
//	set -a; . "$SCRATCH/stalwart-mail/mail.env"; set +a
//	make test-integration-stalwart
const (
	envStalwartURL      = "SIMPLE_TEST_STALWART_URL"
	envStalwartToken    = "SIMPLE_TEST_STALWART_TOKEN"
	envStalwartSMTP     = "SIMPLE_TEST_STALWART_SMTP"
	envStalwartHostname = "MAIL_HOSTNAME"
)

// liveHarness wires the facade against the real server, with a real zone store
// and a real platform store on a temporary path.
func liveHarness(t *testing.T) (*harness, *stalwart.Client) {
	t.Helper()
	url := strings.TrimSpace(os.Getenv(envStalwartURL))
	token := strings.TrimSpace(os.Getenv(envStalwartToken))
	if url == "" || token == "" {
		t.Skipf("set %s and %s to run the mail integration test (scripts/dev/stalwart-up.sh writes both)",
			envStalwartURL, envStalwartToken)
	}
	hostname := strings.TrimSpace(os.Getenv(envStalwartHostname))
	if hostname == "" {
		hostname = "mail.local.test"
	}

	h := newHarness(t)
	client, err := stalwart.New(url, token)
	if err != nil {
		t.Fatalf("build the client: %v", err)
	}
	cfg := h.cfg
	cfg.APIURL = url
	cfg.APIToken = token
	cfg.Hostname = hostname
	h.cfg = cfg
	h.service = newWithEngine(cfg, h.deps, client)
	return h, client
}

func randomZoneName(t *testing.T) string {
	t.Helper()
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatalf("random: %v", err)
	}
	return "it-" + hex.EncodeToString(buffer) + ".test"
}

func TestIntegrationMailBindMailboxForwarderUnbind(t *testing.T) {
	h, client := liveHarness(t)
	ctx := t.Context()

	if err := h.service.Probe(ctx); err != nil {
		t.Fatalf("probe the mail engine: %v", err)
	}
	status := h.service.Status()
	if !status.Reachable {
		t.Fatalf("the engine is unreachable: %s", status.Reason)
	}
	t.Logf("edition=%q endpoint=%s", status.Version, status.EndpointHost)

	zoneName := randomZoneName(t)
	zoneValue := h.zoneNamed(t, zoneName)

	// The domain, the mailboxes and the lists are removed even when an
	// assertion fails, so a failed run leaves the server as it found it.
	t.Cleanup(func() { removeDomain(t, client, zoneName) })

	// --- bind ------------------------------------------------------------
	dryRun, err := h.service.BindMailDomain(ctx, connect.NewRequest(&mailv1.BindMailDomainRequest{
		ZoneId: zoneValue.ID, DryRun: true,
	}))
	if err != nil {
		t.Fatalf("BindMailDomain(dry_run): %v", err)
	}
	if len(dryRun.Msg.GetDnsPlan()) == 0 {
		t.Error("the dry run produced no plan")
	}
	if _, storeErr := h.store.GetMailDomain(ctx, zoneValue.ID); storeErr == nil {
		t.Error("the dry run persisted a row")
	}

	// The client-autoconfiguration set is off here so this test still describes
	// exactly the policy records; TestIntegrationMailClientAutoconfigRecords
	// covers the rest against the same server.
	bound := bind(t, h, zoneValue, &mailv1.BindMailDomainRequest{
		PublishClientAutoconfig: proto.Bool(false),
	})
	if bound.GetDomain().GetState() != mailv1.MailDomainState_MAIL_DOMAIN_STATE_BOUND {
		t.Fatalf("state = %v, reason %q", bound.GetDomain().GetState(), bound.GetDomain().GetReason())
	}
	if bound.GetDomain().GetDkimPending() {
		t.Error("the bind did not wait for both DKIM keys")
	}
	if !bound.GetDomain().GetRecordsInSync() {
		t.Error("records_in_sync is false right after a bind")
	}

	published := summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	t.Logf("published records:\n  %s", strings.Join(published, "\n  "))
	if len(published) != 5 {
		t.Fatalf("published %d records, want MX + SPF + DMARC + two DKIM", len(published))
	}
	wantMX := fmt.Sprintf("MX @ %d %s.", h.cfg.MXPriority, h.cfg.Hostname)
	if !slices.Contains(published, wantMX) {
		t.Errorf("the MX record does not name MAIL_HOSTNAME: %v", published)
	}
	if !slices.Contains(published, `TXT @ "v=spf1 mx -all"`) {
		t.Errorf("SPF = %v", published)
	}
	dkim := 0
	for _, line := range published {
		if strings.Contains(line, "._domainkey") {
			dkim++
			if !strings.Contains(line, "v=DKIM1;") {
				t.Errorf("a DKIM record is not a key: %s", line)
			}
		}
	}
	if dkim != 2 {
		t.Errorf("published %d DKIM records, want one per algorithm", dkim)
	}

	// --- mailbox ---------------------------------------------------------
	created := createMailbox(t, h, zoneValue.ID, "mara")
	credential := created.GetPassword()
	if credential == "" {
		t.Fatal("no credential was returned")
	}
	assertCredentialIsNowhere(t, h, credential)

	// The server's own password policy (zxcvbn strength "three" by default)
	// must accept the generated credential: proving it here is what makes the
	// generator's shape a real guarantee rather than a local opinion.
	if err := client.UpdateAccount(ctx, created.GetMailbox().GetId(), stalwart.AccountPatch{Secret: &credential}); err != nil {
		t.Fatalf("the mail server rejected the generated credential: %v", err)
	}

	// Display name is optional in both mailbox forms, and Stalwart refuses
	// `"description": ""` with "Invalid value for property.", so the blank path
	// is the one that has to be exercised against the real server — from both
	// sides: never set, and cleared after being set.
	blank, err := h.service.CreateMailbox(ctx, connect.NewRequest(&mailv1.CreateMailboxRequest{
		ZoneId: zoneValue.ID, LocalPart: "iris",
	}))
	if err != nil {
		t.Fatalf("CreateMailbox without a display name: %v", err)
	}
	if name := blank.Msg.GetMailbox().GetDisplayName(); name != "" {
		t.Errorf("display name = %q, want it empty", name)
	}
	cleared := ""
	if _, err := h.service.UpdateMailbox(ctx, connect.NewRequest(&mailv1.UpdateMailboxRequest{
		ZoneId: zoneValue.ID, MailboxId: created.GetMailbox().GetId(), DisplayName: &cleared,
	})); err != nil {
		t.Fatalf("UpdateMailbox clearing the display name: %v", err)
	}

	// --- forwarders ------------------------------------------------------
	alias := createForwarder(t, h, zoneValue.ID, "hello", []string{"mara@" + zoneName})
	if alias.GetKind() != mailv1.ForwarderKind_FORWARDER_KIND_ALIAS {
		t.Errorf("a single local target produced %v", alias.GetKind())
	}
	list := createForwarder(t, h, zoneValue.ID, "team", []string{"someone@example.com"})
	if list.GetKind() != mailv1.ForwarderKind_FORWARDER_KIND_LIST || !list.GetExternal() {
		t.Errorf("an external target produced %#v", list)
	}
	if forwarders := listForwarders(t, h, zoneValue.ID); len(forwarders) != 2 {
		t.Errorf("listed %d forwarders, want 2", len(forwarders))
	}

	// --- delivery --------------------------------------------------------
	smtpAddr := strings.TrimSpace(os.Getenv(envStalwartSMTP))
	if smtpAddr == "" {
		smtpAddr = "127.0.0.1:20025"
	}
	before := usedBytes(t, h, zoneValue.ID, "mara@"+zoneName)
	if err := sendMessage(smtpAddr, "sender@example.org", "hello@"+zoneName); err != nil {
		t.Fatalf("deliver a message through %s: %v", smtpAddr, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	var after uint64
	for {
		after = usedBytes(t, h, zoneValue.ID, "mara@"+zoneName)
		if after > before || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if after <= before {
		t.Errorf("usage did not grow after a delivery: %d → %d", before, after)
	}
	t.Logf("mailbox usage %d → %d bytes", before, after)

	// A message to the external forwarder leaves the customer's address only in
	// orcpt, which is what the queue attribution has to survive. The lab has no
	// outbound DNS, so it may bounce before the queue is read; the assertion is
	// therefore on the attribution when a row exists, never on it existing.
	if err := sendMessage(smtpAddr, "sender@example.org", "team@"+zoneName); err != nil {
		t.Fatalf("deliver to the external forwarder: %v", err)
	}
	queue, err := h.service.GetMailQueue(ctx, connect.NewRequest(&mailv1.GetMailQueueRequest{ZoneId: zoneValue.ID}))
	if err != nil {
		t.Fatalf("GetMailQueue: %v", err)
	}
	if !queue.Msg.GetSummary().GetAvailable() {
		t.Error("the queue summary is unavailable on an idle server")
	}
	for _, message := range queue.Msg.GetMessages() {
		t.Logf("queued: from=%q to=%v status=%s queue=%s",
			message.GetFrom(), message.GetTo(), message.GetStatus(), message.GetQueue())
		attributed := false
		for _, recipient := range message.GetTo() {
			if strings.HasSuffix(recipient, "@"+zoneName) {
				attributed = true
			}
		}
		if !attributed {
			t.Errorf("a queued message was attributed to this zone without naming it: %#v", message)
		}
	}

	// --- honesty guards --------------------------------------------------
	// Per-domain delivery history is Enterprise-only. Locking the refusal in
	// keeps anyone from coding against a number this edition cannot produce.
	_, metricErr := client.Call(ctx, []stalwart.MethodCall{{Name: "x:Metric/query", Args: map[string]any{}, ID: "0"}})
	if !stalwart.IsForbidden(metricErr) {
		t.Errorf("x:Metric/query = %v, want a forbidden method error", metricErr)
	}
	// The mailing-list query has no domainId filter, which is why ListLists
	// pages unfiltered and matches the domain itself.
	_, filterErr := client.Call(ctx, []stalwart.MethodCall{{
		Name: "x:MailingList/query",
		Args: map[string]any{"filter": map[string]any{"domainId": "c"}},
		ID:   "0",
	}})
	kind, _ := stalwart.MethodErrorType(filterErr)
	if kind != stalwart.ErrorTypeUnsupportedFilter {
		t.Errorf("a domainId filter answered %v, want unsupportedFilter", filterErr)
	}

	// --- unbind ----------------------------------------------------------
	if _, err := h.service.UnbindMailDomain(ctx, connect.NewRequest(&mailv1.UnbindMailDomainRequest{
		ZoneId: zoneValue.ID,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("unbind without confirmation: code = %v (%v)", connect.CodeOf(err), err)
	}
	unbound, err := h.service.UnbindMailDomain(ctx, connect.NewRequest(&mailv1.UnbindMailDomainRequest{
		ZoneId: zoneValue.ID, DeleteMailboxes: true, ConfirmZoneName: zoneName,
	}))
	if err != nil {
		t.Fatalf("UnbindMailDomain: %v", err)
	}
	// mara and the display-name-less iris.
	if unbound.Msg.GetMailboxesDeleted() != 2 {
		t.Errorf("deleted %d mailboxes, want 2", unbound.Msg.GetMailboxesDeleted())
	}
	if unbound.Msg.GetForwardersDeleted() != 2 {
		t.Errorf("deleted %d forwarders", unbound.Msg.GetForwardersDeleted())
	}
	if _, found, err := client.FindDomain(ctx, zoneName); err != nil || found {
		t.Errorf("the domain survived the unbind (found=%v, err=%v)", found, err)
	}
	if remaining := summarize(h.records(t, zoneValue.ID), zone.SourceMail); len(remaining) != 0 {
		t.Errorf("records survived the unbind: %v", remaining)
	}
}

func TestIntegrationMailRebuildRecoversTheBinding(t *testing.T) {
	h, client := liveHarness(t)
	ctx := t.Context()

	zoneName := randomZoneName(t)
	zoneValue := h.zoneNamed(t, zoneName)
	t.Cleanup(func() { removeDomain(t, client, zoneName) })

	bind(t, h, zoneValue, nil)
	if err := h.store.DeleteMailDomain(ctx, zoneValue.ID); err != nil {
		t.Fatalf("DeleteMailDomain: %v", err)
	}

	report, err := h.service.Rebuild(ctx, false)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if report.Count != 1 {
		t.Fatalf("recovered %d bindings, want 1 (warnings: %v)", report.Count, report.Warnings)
	}
	doc, err := h.store.GetMailDomain(ctx, zoneValue.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	if doc.EngineDomainID == "" || doc.ZoneName != zoneName {
		t.Errorf("recovered row = %#v", doc)
	}
}

// sendMessage delivers one small message over plain SMTP.
//
// smtp.SendMail is not used: it greets with "localhost", which Stalwart refuses
// ("Invalid EHLO domain"), so the session is driven directly with a real
// hostname. The :25 listener is plaintext with STARTTLS offered; the lab server
// has no certificate, so the message is sent without it.
func sendMessage(addr, from, to string) error {
	client, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { ignoreSMTPClose(client.Close()) }()
	if err := client.Hello("client.local.test"); err != nil {
		return fmt.Errorf("ehlo: %w", err)
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	body := "From: " + from + "\r\nTo: " + to + "\r\nSubject: integration probe\r\n\r\nhello\r\n"
	if _, err := writer.Write([]byte(body)); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close the message: %w", err)
	}
	return client.Quit()
}

// ignoreSMTPClose drops the error from closing an SMTP session that Quit
// already closed cleanly; the delivery's success is decided before it.
func ignoreSMTPClose(error) {}

func usedBytes(t *testing.T, h *harness, zoneID, address string) uint64 {
	t.Helper()
	response, err := h.service.ListMailboxes(t.Context(), connect.NewRequest(&mailv1.ListMailboxesRequest{ZoneId: zoneID}))
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	for _, mailbox := range response.Msg.GetMailboxes() {
		if mailbox.GetAddress() == address {
			return mailbox.GetUsedBytes()
		}
	}
	t.Fatalf("mailbox %s was not listed", address)
	return 0
}

// removeDomain takes the test's domain off the server, whatever state the run
// left it in, so a failed run does not leak objects into the next one. It runs
// on its own context: t.Cleanup fires after the test's context is already done.
//
// Every failure is logged rather than failed on — this is teardown, and the
// assertions have already run.
func removeDomain(t *testing.T, client *stalwart.Client, zoneName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	domain, found, err := client.FindDomain(ctx, zoneName)
	if err != nil || !found {
		return
	}
	report := func(what string, err error) {
		if err != nil {
			t.Logf("clean up %s of %s: %v", what, zoneName, err)
		}
	}
	accounts, err := client.ListAccounts(ctx, domain.ID)
	report("the mailboxes", err)
	for _, account := range accounts {
		report("a mailbox", client.DestroyAccount(ctx, account.ID))
	}
	lists, err := client.ListLists(ctx, domain.ID)
	report("the forwarders", err)
	for _, list := range lists {
		report("a forwarder", client.DestroyList(ctx, list.ID))
	}
	signatures, err := client.ListDkim(ctx, domain.ID)
	report("the DKIM keys", err)
	for _, signature := range signatures {
		report("a DKIM key", client.DestroyDkim(ctx, signature.ID))
	}
	report("the domain", client.DestroyDomain(ctx, domain.ID))
}

// TestIntegrationMailClientAutoconfigRecords proves against the real server
// that the records this facade republishes are the ones the server actually
// renders, that the ones it refuses to publish stay out, and that the switch
// adds and removes them without touching anything else.
func TestIntegrationMailClientAutoconfigRecords(t *testing.T) {
	h, client := liveHarness(t)
	ctx := t.Context()

	zoneName := randomZoneName(t)
	zoneValue := h.zoneNamed(t, zoneName)
	t.Cleanup(func() { removeDomain(t, client, zoneName) })

	bound := bind(t, h, zoneValue, nil)
	if bound.GetDomain().GetState() != mailv1.MailDomainState_MAIL_DOMAIN_STATE_BOUND {
		t.Fatalf("state = %v, reason %q", bound.GetDomain().GetState(), bound.GetDomain().GetReason())
	}
	if !bound.GetDomain().GetPublishClientAutoconfig() {
		t.Fatal("publish_client_autoconfig is false after a default bind")
	}

	published := summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	t.Logf("published records:\n  %s", strings.Join(published, "\n  "))

	// Every autoconfiguration record must name MAIL_HOSTNAME. A record pointing
	// anywhere else would send a customer's mail clients to a host this
	// deployment does not vouch for.
	target := h.cfg.Hostname + "."
	byOwner := map[string]zone.Record{}
	for _, record := range h.records(t, zoneValue.ID) {
		if record.Source != zone.SourceMail {
			continue
		}
		byOwner[record.Name] = record
		switch record.Type {
		case zone.TypeSRV:
			if !strings.HasSuffix(record.Value, " "+target) {
				t.Errorf("%s SRV targets %q, not %s", record.Name, record.Value, target)
			}
		case zone.TypeCNAME:
			if !strings.EqualFold(record.Value, target) {
				t.Errorf("%s CNAME targets %q, not %s", record.Name, record.Value, target)
			}
		}
	}

	// The server's zone file is the source, so every allow-listed owner it
	// renders must be published, and the two aliases must be present: they are
	// what Thunderbird and Outlook look for.
	domain, err := client.GetDomain(ctx, bound.GetDomain().GetEngineDomainId())
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	offered := parseAutoconfigRecords(domain.DNSZoneFile, zoneName, h.cfg.Hostname)
	if len(offered) == 0 {
		t.Fatalf("the server offered no autoconfiguration records:\n%s", domain.DNSZoneFile)
	}
	for _, record := range offered {
		got, ok := byOwner[record.Name]
		if !ok {
			t.Errorf("the server offers %s %s but it was not published", record.Type, record.Name)
			continue
		}
		if string(got.Type) != record.Type {
			t.Errorf("%s published as %s, the server offers %s", record.Name, got.Type, record.Type)
		}
	}
	for _, owner := range autoconfigCNAMEOwners {
		if _, ok := byOwner[owner]; !ok {
			t.Errorf("%s was not published; the server rendered:\n%s", owner, domain.DNSZoneFile)
		}
	}

	// And what the deployment cannot serve stays out, whatever the server
	// offers: MTA-STS needs an HTTPS policy endpoint under the customer's own
	// name, TLS-RPT reports on policies this facade does not publish, and
	// ua-auto-config has MTA-STS's certificate problem with none of its client
	// support. All four are in the server's own output.
	for _, owner := range []string{"mta-sts", "_mta-sts", "_smtp._tls", "ua-auto-config", "_ua-auto-config"} {
		if record, ok := byOwner[owner]; ok {
			t.Errorf("published %s %s, which this deployment cannot serve", record.Type, owner)
		}
	}

	// Turning the switch off removes exactly those records, and nothing else.
	off, err := h.service.UpdateMailDomain(ctx, connect.NewRequest(&mailv1.UpdateMailDomainRequest{
		ZoneId:                  zoneValue.ID,
		PublishClientAutoconfig: proto.Bool(false),
	}))
	if err != nil {
		t.Fatalf("UpdateMailDomain(off): %v", err)
	}
	if off.Msg.GetDomain().GetPublishClientAutoconfig() {
		t.Error("publish_client_autoconfig is still true after turning it off")
	}
	remaining := summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	if len(remaining) != 5 {
		t.Errorf("after turning it off %d records remain, want MX + SPF + DMARC + two DKIM:\n  %s",
			len(remaining), strings.Join(remaining, "\n  "))
	}

	// And back on, with the reconciler rather than an explicit call doing the
	// work — the drift path the periodic pass takes.
	doc, err := h.store.GetMailDomain(ctx, zoneValue.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	doc.PublishClientAutoconfig = true
	if err := h.store.PutMailDomain(ctx, doc); err != nil {
		t.Fatalf("PutMailDomain: %v", err)
	}
	if err := h.service.ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if got := summarize(h.records(t, zoneValue.ID), zone.SourceMail); len(got) != len(published) {
		t.Errorf("the reconciler restored %d records, want the original %d:\n  %s",
			len(got), len(published), strings.Join(got, "\n  "))
	}
}

// Delivery-event integration. It needs more than the API: the mail server has
// to have been pointed at a receiver on this host, which
// scripts/dev/stalwart-webhook.sh does (and which needs a server restart to
// take effect — see docs/mail-runbook.md, "Delivery statistics").
const (
	envWebhookSecret = "SIMPLE_TEST_STALWART_WEBHOOK_SECRET"
	envWebhookPort   = "SIMPLE_TEST_STALWART_WEBHOOK_PORT"
	envWebhookPath   = "SIMPLE_TEST_STALWART_WEBHOOK_PATH"
)

// TestIntegrationMailDeliveryEvents is the empirical half of G5: a real message
// through a real server produces real delivery events, and this facade turns
// them into the domain's rolling counts. Nothing here is a fixture — if the
// server stopped emitting these events, this test would fail rather than the
// console quietly showing a wrong number.
func TestIntegrationMailDeliveryEvents(t *testing.T) {
	h, client := liveHarness(t)
	ctx := t.Context()

	secret := strings.TrimSpace(os.Getenv(envWebhookSecret))
	port := strings.TrimSpace(os.Getenv(envWebhookPort))
	smtpAddr := strings.TrimSpace(os.Getenv(envStalwartSMTP))
	if secret == "" || port == "" || smtpAddr == "" {
		t.Skipf("set %s, %s and %s to run the delivery-event test (scripts/dev/stalwart-webhook.sh writes them)",
			envWebhookSecret, envWebhookPort, envStalwartSMTP)
	}
	path := strings.TrimSpace(os.Getenv(envWebhookPath))
	if path == "" {
		path = "/mail/v1/delivery-events"
	}

	// Rebuild the facade with the secret set, so the receiver exists.
	cfg := h.cfg
	cfg.WebhookSecret = secret
	h.cfg = cfg
	h.service = newWithEngine(cfg, h.deps, client)
	// The live clock, not the harness's frozen one: the server stamps its
	// events with real time and they have to land in the same hour.
	h.clock.Store(time.Now().UnixNano())

	handler := h.service.WebhookHandler()
	if handler == nil {
		t.Fatal("the receiver was not mounted with a secret set")
	}

	mux := http.NewServeMux()
	mux.Handle("POST "+path, handler)
	listener, err := net.Listen("tcp", "0.0.0.0:"+port)
	if err != nil {
		t.Fatalf("listen on the webhook port the mail server was pointed at: %v", err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve the receiver: %v", err)
		}
	}()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Logf("shut the receiver down: %v", err)
		}
	})

	zoneName := randomZoneName(t)
	zoneValue := h.zoneNamed(t, zoneName)
	t.Cleanup(func() { removeDomain(t, client, zoneName) })
	bind(t, h, zoneValue, nil)
	createMailbox(t, h, zoneValue.ID, "mara")

	// Nothing has arrived from the mail server yet, so there is no figure to
	// report — a secret set on this side proves nothing about the webhook on
	// the other, and this is the state a hook that was never created, or was
	// created without the restart that loads it, stays in for ever.
	before, err := h.store.GetMailDomain(ctx, zoneValue.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	if stats := h.service.deliveryStats(before); stats != nil {
		t.Fatalf("delivery stats were reported before the mail server posted anything: %v", stats)
	}
	status, err := h.service.GetMailStatus(ctx, connect.NewRequest(&mailv1.GetMailStatusRequest{}))
	if err != nil {
		t.Fatalf("GetMailStatus: %v", err)
	}
	if status.Msg.GetDeliveryStatsAvailable() || status.Msg.GetDeliveryStatsNote() != DeliveryStatsPendingNote {
		t.Errorf("status before any event = available %t, note %q",
			status.Msg.GetDeliveryStatsAvailable(), status.Msg.GetDeliveryStatsNote())
	}

	// One real message, delivered locally to the mailbox above. The server
	// emits delivery.dsn-success for it and posts that event here.
	if err := sendMessage(smtpAddr, "sender@example.org", "mara@"+zoneName); err != nil {
		t.Fatalf("deliver a message: %v", err)
	}

	// The server throttles its deliveries, so the event arrives a beat later.
	var stats *mailv1.DeliveryStats
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		h.clock.Store(time.Now().UnixNano())
		h.service.flushDeliveries(ctx)
		doc, err := h.store.GetMailDomain(ctx, zoneValue.ID)
		if err != nil {
			t.Fatalf("GetMailDomain: %v", err)
		}
		if stats = h.service.deliveryStats(doc); stats != nil {
			break
		}
		time.Sleep(time.Second)
	}
	if stats == nil {
		t.Fatalf("no delivery event reached the receiver within a minute; "+
			"is the mail server pointed at port %s of this host, and was it restarted after it was configured?", port)
	}
	t.Logf("the mail server reached the receiver; %s reports %d delivered, %d bounced over %d hours",
		zoneName, stats.GetDelivered(), stats.GetBounced(), stats.GetWindowHours())

	// The message above is mail this domain received, not mail it sent, so it
	// is not part of the delivered/bounced pair — that pair is a sending
	// record, and the server refuses a message to an address that does not
	// exist before it is ever queued, so no inbound figure could include the
	// commonest inbound failure.
	if stats.GetDelivered() != 0 {
		t.Errorf("a received message was counted as sent: delivered = %d", stats.GetDelivered())
	}

	// The state reaches the wire through the ordinary read path, which is what
	// the console renders.
	response, err := h.service.GetMailDomain(ctx, connect.NewRequest(&mailv1.GetMailDomainRequest{ZoneId: zoneValue.ID}))
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	wire := response.Msg.GetDomain().GetDelivery()
	if !wire.GetAvailable() {
		t.Error("the wire reports no delivery stats although the receiver has been reached")
	}
	if wire.GetWindowHours() == 0 || wire.GetWindowHours() > 24 {
		t.Errorf("window_hours = %d, want between 1 and 24", wire.GetWindowHours())
	}
	status, err = h.service.GetMailStatus(ctx, connect.NewRequest(&mailv1.GetMailStatusRequest{}))
	if err != nil {
		t.Fatalf("GetMailStatus: %v", err)
	}
	if !status.Msg.GetDeliveryStatsAvailable() || status.Msg.GetDeliveryStatsNote() != DeliveryStatsWebhookNote {
		t.Errorf("status after a real event = available %t, note %q",
			status.Msg.GetDeliveryStatsAvailable(), status.Msg.GetDeliveryStatsNote())
	}

	// An unauthenticated delivery to the same route is refused, against the
	// same running receiver the mail server is talking to.
	body := strings.NewReader(`{"events":[]}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+port+path, body)
	if err != nil {
		t.Fatalf("build the unauthenticated request: %v", err)
	}
	unauthenticated, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post without a credential: %v", err)
	}
	defer func() { ignoreClose(unauthenticated.Body.Close()) }()
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unauthenticated delivery answered %d, want 401", unauthenticated.StatusCode)
	}

	// The secret never reaches the log.
	if strings.Contains(h.logs.String(), secret) {
		t.Error("the log leaked MAIL_WEBHOOK_SECRET")
	}
}

// ignoreClose drops a close error from a response body that has already been
// read; nothing about the assertion above depends on it.
func ignoreClose(error) {}
