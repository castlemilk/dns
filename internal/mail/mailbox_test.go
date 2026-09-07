package mail

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/zone"
)

// mailDomainDoc is the row a bind persists before its first engine write. Tests
// use it to simulate a crash at exactly that point.
func mailDomainDoc(zoneValue zone.Zone, now time.Time) platform.MailDomainDoc {
	return platform.MailDomainDoc{
		V:             1,
		ZoneID:        zoneValue.ID,
		ZoneName:      zoneValue.Name,
		State:         StateBinding,
		DmarcPolicy:   "quarantine",
		ReportAddress: "postmaster@" + zoneValue.Name,
		CreatedAt:     now,
		UpdatedAt:     now,
		BindStartedAt: &now,
	}
}

func createMailbox(t *testing.T, h *harness, zoneID, local string) *mailv1.CreateMailboxResponse {
	t.Helper()
	response, err := h.service.CreateMailbox(t.Context(), connect.NewRequest(&mailv1.CreateMailboxRequest{
		ZoneId: zoneID, LocalPart: local, DisplayName: strings.ToUpper(local[:1]) + local[1:],
	}))
	if err != nil {
		t.Fatalf("CreateMailbox(%s): %v", local, err)
	}
	return response.Msg
}

func createForwarder(t *testing.T, h *harness, zoneID, local string, targets []string) *mailv1.Forwarder {
	t.Helper()
	response, err := h.service.CreateForwarder(t.Context(), connect.NewRequest(&mailv1.CreateForwarderRequest{
		ZoneId: zoneID, LocalPart: local, Targets: targets,
	}))
	if err != nil {
		t.Fatalf("CreateForwarder(%s): %v", local, err)
	}
	return response.Msg.GetForwarder()
}

func boundZone(t *testing.T) (*harness, zone.Zone) {
	t.Helper()
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)
	return h, zoneValue
}

func TestCreateMailboxReturnsTheCredentialOnceWithTheClientPorts(t *testing.T) {
	h, zoneValue := boundZone(t)
	created := createMailbox(t, h, zoneValue.ID, "mara")

	if created.GetMailbox().GetAddress() != "mara@acme.dev" {
		t.Errorf("address = %q", created.GetMailbox().GetAddress())
	}
	credential := created.GetPassword()
	if credential == "" {
		t.Fatal("no credential was returned")
	}
	// The retrieval hint is derived from the engine's own autoconfiguration
	// records, not from a constant: an engine advertising no IMAP must not be
	// described as IMAP. fakemail advertises _imaps._tcp on 993.
	if created.GetRetrievalProtocol() != "imaps" {
		t.Errorf("retrieval protocol = %q, want imaps", created.GetRetrievalProtocol())
	}
	if created.GetRetrievalHost() != "mail.local.test" || created.GetRetrievalPort() != 993 {
		t.Errorf("retrieval = %s:%d", created.GetRetrievalHost(), created.GetRetrievalPort())
	}
	if created.GetSmtpHost() != "mail.local.test" || created.GetSmtpPort() != 465 {
		t.Errorf("smtp = %s:%d", created.GetSmtpHost(), created.GetSmtpPort())
	}

	// It is shown once: no later read carries it.
	listed, err := h.service.ListMailboxes(t.Context(), connect.NewRequest(&mailv1.ListMailboxesRequest{ZoneId: zoneValue.ID}))
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	if rendered := fmt.Sprintf("%v", listed.Msg); strings.Contains(rendered, credential) {
		t.Error("ListMailboxes returned the credential")
	}

	assertCredentialIsNowhere(t, h, credential)
}

// assertCredentialIsNowhere is the structural guarantee: a generated credential
// has no recognisable shape, so no redactor could catch it after the fact. It
// must therefore never reach the log, an activity event or the store.
func assertCredentialIsNowhere(t *testing.T, h *harness, credential string) {
	t.Helper()
	if credential == "" {
		t.Fatal("no credential to check")
	}
	if strings.Contains(h.logs.String(), credential) {
		t.Error("the credential reached the log")
	}
	for _, event := range h.events.all() {
		if strings.Contains(event.Summary, credential) {
			t.Errorf("the credential reached an event summary: %s", event.Summary)
		}
		for key, value := range event.Details {
			if strings.Contains(value, credential) {
				t.Errorf("the credential reached event detail %q", key)
			}
		}
	}
	docs, err := h.store.ListMailDomains(t.Context())
	if err != nil {
		t.Fatalf("ListMailDomains: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%#v", docs), credential) {
		t.Error("the credential reached the platform store")
	}
}

func TestCreateMailboxRefusesADuplicateAddress(t *testing.T) {
	h, zoneValue := boundZone(t)
	createMailbox(t, h, zoneValue.ID, "mara")
	_, err := h.service.CreateMailbox(t.Context(), connect.NewRequest(&mailv1.CreateMailboxRequest{
		ZoneId: zoneValue.ID, LocalPart: "mara",
	}))
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "mara@acme.dev already exists") {
		t.Errorf("message = %v", err)
	}
}

func TestCreateMailboxRefusesAnAddressAForwarderAlreadyUses(t *testing.T) {
	h, zoneValue := boundZone(t)
	createForwarder(t, h, zoneValue.ID, "team", []string{"someone@example.com"})
	_, err := h.service.CreateMailbox(t.Context(), connect.NewRequest(&mailv1.CreateMailboxRequest{
		ZoneId: zoneValue.ID, LocalPart: "team",
	}))
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
}

func TestTheMailboxLimitIsEnforced(t *testing.T) {
	h := newHarness(t, func(cfg *config.Mail) { cfg.MailboxesPerDomain = 2 })
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)

	createMailbox(t, h, zoneValue.ID, "one")
	createMailbox(t, h, zoneValue.ID, "two")
	_, err := h.service.CreateMailbox(t.Context(), connect.NewRequest(&mailv1.CreateMailboxRequest{
		ZoneId: zoneValue.ID, LocalPart: "three",
	}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "2 mailboxes per domain") {
		t.Errorf("message = %v", err)
	}
}

func TestLocalPartValidation(t *testing.T) {
	h, zoneValue := boundZone(t)
	for _, local := range []string{"", "-mara", "mara-", "ma..ra", "Mara Smith", "mär", strings.Repeat("a", 65)} {
		_, err := h.service.CreateMailbox(t.Context(), connect.NewRequest(&mailv1.CreateMailboxRequest{
			ZoneId: zoneValue.ID, LocalPart: local,
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("local part %q was accepted (%v)", local, err)
		}
	}
	// The separators a mail client accepts are allowed between alphanumerics.
	for _, local := range []string{"mara", "mara.smith", "mara+news", "mara_1", "m1"} {
		if _, err := normalizeLocalPart(local); err != nil {
			t.Errorf("local part %q was refused: %v", local, err)
		}
	}
}

func TestQuotaBoundsAndDefault(t *testing.T) {
	h, zoneValue := boundZone(t)
	created := createMailbox(t, h, zoneValue.ID, "mara")
	if created.GetMailbox().GetQuotaBytes() != 5<<30 {
		t.Errorf("default quota = %d", created.GetMailbox().GetQuotaBytes())
	}
	for _, quota := range []uint64{1, 2 << 40} {
		_, err := h.service.CreateMailbox(t.Context(), connect.NewRequest(&mailv1.CreateMailboxRequest{
			ZoneId: zoneValue.ID, LocalPart: "other", QuotaBytes: quota,
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("quota %d was accepted (%v)", quota, err)
		}
	}
}

func TestUsageIsLive(t *testing.T) {
	h, zoneValue := boundZone(t)
	createMailbox(t, h, zoneValue.ID, "mara")
	h.engine.Deliver("mara@acme.dev", 1894)

	listed, err := h.service.ListMailboxes(t.Context(), connect.NewRequest(&mailv1.ListMailboxesRequest{ZoneId: zoneValue.ID}))
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	if got := listed.Msg.GetMailboxes()[0].GetUsedBytes(); got != 1894 {
		t.Errorf("used bytes = %d", got)
	}
	if listed.Msg.GetLimit() != 5 {
		t.Errorf("limit = %d", listed.Msg.GetLimit())
	}
}

func TestResetIssuesANewCredential(t *testing.T) {
	h, zoneValue := boundZone(t)
	created := createMailbox(t, h, zoneValue.ID, "mara")

	response, err := h.service.ResetMailboxPassword(t.Context(), connect.NewRequest(&mailv1.ResetMailboxPasswordRequest{
		ZoneId: zoneValue.ID, MailboxId: created.GetMailbox().GetId(),
	}))
	if err != nil {
		t.Fatalf("ResetMailboxPassword: %v", err)
	}
	fresh := response.Msg.GetPassword()
	if fresh == "" || fresh == created.GetPassword() {
		t.Error("the reset did not issue a different credential")
	}
	assertCredentialIsNowhere(t, h, fresh)
	if !h.events.has(activity.KindMailboxPasswordReset) {
		t.Errorf("no reset event: %v", h.events.kinds())
	}
}

func TestDeleteMailboxRequiresTheTypedAddress(t *testing.T) {
	h, zoneValue := boundZone(t)
	created := createMailbox(t, h, zoneValue.ID, "mara")

	_, err := h.service.DeleteMailbox(t.Context(), connect.NewRequest(&mailv1.DeleteMailboxRequest{
		ZoneId: zoneValue.ID, MailboxId: created.GetMailbox().GetId(), ConfirmAddress: "mara@wrong.test",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
	if _, err := h.service.DeleteMailbox(t.Context(), connect.NewRequest(&mailv1.DeleteMailboxRequest{
		ZoneId: zoneValue.ID, MailboxId: created.GetMailbox().GetId(), ConfirmAddress: "mara@acme.dev",
	})); err != nil {
		t.Fatalf("DeleteMailbox: %v", err)
	}
	accounts, err := h.engine.ListAccounts(t.Context(), engineDomainID(t, h, zoneValue.ID))
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("the mailbox survived: %#v", accounts)
	}
}

func TestUpdateMailboxChangesTheQuotaAndDisplayName(t *testing.T) {
	h, zoneValue := boundZone(t)
	created := createMailbox(t, h, zoneValue.ID, "mara")

	name := "Mara Smith"
	quota := uint64(1 << 30)
	response, err := h.service.UpdateMailbox(t.Context(), connect.NewRequest(&mailv1.UpdateMailboxRequest{
		ZoneId: zoneValue.ID, MailboxId: created.GetMailbox().GetId(), DisplayName: &name, QuotaBytes: &quota,
	}))
	if err != nil {
		t.Fatalf("UpdateMailbox: %v", err)
	}
	if response.Msg.GetMailbox().GetDisplayName() != name || response.Msg.GetMailbox().GetQuotaBytes() != quota {
		t.Errorf("mailbox = %#v", response.Msg.GetMailbox())
	}
}

func TestForwarderToOneLocalMailboxIsAnAlias(t *testing.T) {
	h, zoneValue := boundZone(t)
	createMailbox(t, h, zoneValue.ID, "mara")

	forwarder := createForwarder(t, h, zoneValue.ID, "hello", []string{"mara@acme.dev"})
	if forwarder.GetKind() != mailv1.ForwarderKind_FORWARDER_KIND_ALIAS {
		t.Fatalf("kind = %v", forwarder.GetKind())
	}
	if forwarder.GetExternal() {
		t.Error("a local alias was marked external")
	}
	if !strings.HasPrefix(forwarder.GetId(), aliasIDPrefix) {
		t.Errorf("id = %q", forwarder.GetId())
	}
	// It shows up in the merged list and delivering to it grows the mailbox.
	listed := listForwarders(t, h, zoneValue.ID)
	if len(listed) != 1 || listed[0].GetAddress() != "hello@acme.dev" {
		t.Fatalf("forwarders = %#v", listed)
	}
}

func TestForwarderToAnExternalAddressIsAList(t *testing.T) {
	h, zoneValue := boundZone(t)
	forwarder := createForwarder(t, h, zoneValue.ID, "team", []string{"someone@example.com"})
	if forwarder.GetKind() != mailv1.ForwarderKind_FORWARDER_KIND_LIST {
		t.Fatalf("kind = %v", forwarder.GetKind())
	}
	if !forwarder.GetExternal() {
		t.Error("an external target was not flagged")
	}
}

func TestSeveralLocalMailboxTargetsAreRefused(t *testing.T) {
	h, zoneValue := boundZone(t)
	createMailbox(t, h, zoneValue.ID, "mara")
	createMailbox(t, h, zoneValue.ID, "sam")

	_, err := h.service.CreateForwarder(t.Context(), connect.NewRequest(&mailv1.CreateForwarderRequest{
		ZoneId: zoneValue.ID, LocalPart: "team", Targets: []string{"mara@acme.dev", "sam@acme.dev"},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "group forwarder") {
		t.Errorf("message = %v", err)
	}
}

func TestForwarderTargetValidation(t *testing.T) {
	h, zoneValue := boundZone(t)
	cases := [][]string{
		{},
		{"not-an-address"},
		{"someone@localhost"},
		make([]string, 11),
	}
	for index := range cases[3] {
		cases[3][index] = fmt.Sprintf("person%d@example.com", index)
	}
	for _, targets := range cases {
		_, err := h.service.CreateForwarder(t.Context(), connect.NewRequest(&mailv1.CreateForwarderRequest{
			ZoneId: zoneValue.ID, LocalPart: "team", Targets: targets,
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("targets %v were accepted (%v)", targets, err)
		}
	}
}

func TestDeletingAForwarder(t *testing.T) {
	h, zoneValue := boundZone(t)
	createMailbox(t, h, zoneValue.ID, "mara")
	alias := createForwarder(t, h, zoneValue.ID, "hello", []string{"mara@acme.dev"})
	list := createForwarder(t, h, zoneValue.ID, "team", []string{"someone@example.com"})

	for _, forwarder := range []*mailv1.Forwarder{alias, list} {
		if _, err := h.service.DeleteForwarder(t.Context(), connect.NewRequest(&mailv1.DeleteForwarderRequest{
			ZoneId: zoneValue.ID, ForwarderId: forwarder.GetId(),
		})); err != nil {
			t.Fatalf("DeleteForwarder(%s): %v", forwarder.GetId(), err)
		}
	}
	if listed := listForwarders(t, h, zoneValue.ID); len(listed) != 0 {
		t.Errorf("forwarders survived: %#v", listed)
	}
	// An id that names nothing is a NotFound, and a malformed one an
	// InvalidArgument.
	if _, err := h.service.DeleteForwarder(t.Context(), connect.NewRequest(&mailv1.DeleteForwarderRequest{
		ZoneId: zoneValue.ID, ForwarderId: "list:nope",
	})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown list id: code = %v (%v)", connect.CodeOf(err), err)
	}
	if _, err := h.service.DeleteForwarder(t.Context(), connect.NewRequest(&mailv1.DeleteForwarderRequest{
		ZoneId: zoneValue.ID, ForwarderId: "nonsense",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("malformed id: code = %v (%v)", connect.CodeOf(err), err)
	}
}

func TestPostmasterIsReportedFromAnyMechanism(t *testing.T) {
	h, zoneValue := boundZone(t)
	if domainOf(t, h, zoneValue.ID).GetHasPostmaster() {
		t.Fatal("postmaster was reported before it existed")
	}
	createForwarder(t, h, zoneValue.ID, "postmaster", []string{"ops@example.com"})
	if !domainOf(t, h, zoneValue.ID).GetHasPostmaster() {
		t.Error("a postmaster forwarder was not counted")
	}
}

func TestForwarderCountCoversAliasesAndLists(t *testing.T) {
	h, zoneValue := boundZone(t)
	createMailbox(t, h, zoneValue.ID, "mara")
	createForwarder(t, h, zoneValue.ID, "hello", []string{"mara@acme.dev"})
	createForwarder(t, h, zoneValue.ID, "team", []string{"someone@example.com"})

	domain := domainOf(t, h, zoneValue.ID)
	if domain.GetForwarderCount() != 2 {
		t.Errorf("forwarder count = %d", domain.GetForwarderCount())
	}
	if domain.GetMailboxCount() != 1 {
		t.Errorf("mailbox count = %d", domain.GetMailboxCount())
	}
}

func listForwarders(t *testing.T, h *harness, zoneID string) []*mailv1.Forwarder {
	t.Helper()
	response, err := h.service.ListForwarders(t.Context(), connect.NewRequest(&mailv1.ListForwardersRequest{ZoneId: zoneID}))
	if err != nil {
		t.Fatalf("ListForwarders: %v", err)
	}
	return response.Msg.GetForwarders()
}

func domainOf(t *testing.T, h *harness, zoneID string) *mailv1.MailDomain {
	t.Helper()
	response, err := h.service.GetMailDomain(t.Context(), connect.NewRequest(&mailv1.GetMailDomainRequest{ZoneId: zoneID}))
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	return response.Msg.GetDomain()
}

func engineDomainID(t *testing.T, h *harness, zoneID string) string {
	t.Helper()
	doc, err := h.store.GetMailDomain(t.Context(), zoneID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	return doc.EngineDomainID
}

func TestQueueAttributesAForwarderFanOutAndExcludesStrangers(t *testing.T) {
	h, zoneValue := boundZone(t)
	h.engine.EnqueueRemote("sender@example.org", "someone@example.com", "team@acme.dev", "TemporaryFailure")
	h.engine.EnqueueRemote("noise@elsewhere.invalid", "nobody@elsewhere.invalid", "", "Scheduled")

	response, err := h.service.GetMailQueue(t.Context(), connect.NewRequest(&mailv1.GetMailQueueRequest{ZoneId: zoneValue.ID}))
	if err != nil {
		t.Fatalf("GetMailQueue: %v", err)
	}
	summary := response.Msg.GetSummary()
	if !summary.GetAvailable() {
		t.Fatal("the summary is unavailable for a short page")
	}
	if summary.GetFailing() != 1 || summary.GetScheduled() != 0 {
		t.Errorf("summary = %#v", summary)
	}
	messages := response.Msg.GetMessages()
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	// The customer's own address is the one the operator recognises.
	if !slices.Contains(messages[0].GetTo(), "team@acme.dev") {
		t.Errorf("to = %v", messages[0].GetTo())
	}
	if messages[0].GetQueue() != "remote" || messages[0].GetStatus() != "TemporaryFailure" {
		t.Errorf("message = %#v", messages[0])
	}
	// The sender is outside the zone, so only its domain is shown.
	if messages[0].GetFrom() != "example.org" {
		t.Errorf("from = %q", messages[0].GetFrom())
	}
}

func TestAFullQueuePageIsReportedAsUnavailable(t *testing.T) {
	h, zoneValue := boundZone(t)
	h.engine.EnqueueRemote("sender@example.org", "mara@acme.dev", "", "Scheduled")
	h.engine.SetQueueFull()

	response, err := h.service.GetMailQueue(t.Context(), connect.NewRequest(&mailv1.GetMailQueueRequest{ZoneId: zoneValue.ID}))
	if err != nil {
		t.Fatalf("GetMailQueue: %v", err)
	}
	if response.Msg.GetSummary().GetAvailable() {
		t.Error("an incomplete page was reported as available")
	}
	if len(response.Msg.GetMessages()) != 0 {
		t.Error("rows were shown for an incomplete attribution")
	}
}

func TestStatusReportsTheHonestDeliveryAnswer(t *testing.T) {
	h, zoneValue := boundZone(t)
	createMailbox(t, h, zoneValue.ID, "mara")
	h.engine.Deliver("mara@acme.dev", 2048)

	response, err := h.service.GetMailStatus(t.Context(), connect.NewRequest(&mailv1.GetMailStatusRequest{}))
	if err != nil {
		t.Fatalf("GetMailStatus: %v", err)
	}
	message := response.Msg
	if message.GetDeliveryStatsAvailable() {
		t.Error("delivery statistics were claimed to be available")
	}
	if message.GetDeliveryStatsNote() != DeliveryStatsNote {
		t.Errorf("note = %q", message.GetDeliveryStatsNote())
	}
	if message.GetDomains() != 1 || message.GetMailboxes() != 1 || message.GetUsedBytes() != 2048 {
		t.Errorf("totals = %d domains, %d mailboxes, %d bytes",
			message.GetDomains(), message.GetMailboxes(), message.GetUsedBytes())
	}
	if message.GetMailboxesPerDomain() != 5 {
		t.Errorf("mailboxes per domain = %d", message.GetMailboxesPerDomain())
	}
}

func TestProbeReportsMissingPermissions(t *testing.T) {
	h := newHarness(t)
	h.engine.SetPermissions([]string{"sysDomainGet"})
	if err := h.service.Probe(t.Context()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	response, err := h.service.GetMailStatus(t.Context(), connect.NewRequest(&mailv1.GetMailStatusRequest{}))
	if err != nil {
		t.Fatalf("GetMailStatus: %v", err)
	}
	missing := response.Msg.GetMissingPermissions()
	if len(missing) == 0 || slices.Contains(missing, "sysDomainGet") {
		t.Errorf("missing permissions = %v", missing)
	}
	if !slices.Contains(missing, "sysSystemSettingsGet") {
		t.Errorf("the SystemSettings permission was not reported: %v", missing)
	}
}

func TestProbeFailsOnAHostnameMismatch(t *testing.T) {
	h := newHarness(t)
	h.engine.SetHostname("mx9.elsewhere.test")
	if err := h.service.Probe(t.Context()); err == nil {
		t.Fatal("the probe passed with a mismatched hostname")
	}
	status := h.service.Status()
	if status.Reachable {
		t.Error("the engine is reported reachable")
	}
	if !strings.Contains(status.Reason, "mx9.elsewhere.test") {
		t.Errorf("reason = %q", status.Reason)
	}
}

func TestAnUnconfiguredServiceRefusesMutationsAndListsNothing(t *testing.T) {
	service := New(config.Mail{}, platform.Deps{})
	if _, err := service.BindMailDomain(t.Context(), connect.NewRequest(&mailv1.BindMailDomainRequest{ZoneId: "z"})); err == nil {
		t.Fatal("an unconfigured bind was accepted")
	} else if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "MAIL_API_URL") {
		t.Errorf("err = %v", err)
	}
	listed, err := service.ListMailDomains(t.Context(), connect.NewRequest(&mailv1.ListMailDomainsRequest{}))
	if err != nil {
		t.Fatalf("ListMailDomains: %v", err)
	}
	if len(listed.Msg.GetDomains()) != 0 {
		t.Error("an unconfigured service listed domains")
	}
	status := service.Status()
	if status.Configured || status.Reachable {
		t.Errorf("status = %#v", status)
	}
	if !slices.Contains(status.MissingEnv, "MAIL_API_URL") {
		t.Errorf("missing env = %v", status.MissingEnv)
	}
}

func TestEngineFailuresMapToTheirCodes(t *testing.T) {
	h, zoneValue := boundZone(t)
	h.engine.SetUnauthorized()
	_, err := h.service.ListMailboxes(t.Context(), connect.NewRequest(&mailv1.ListMailboxesRequest{ZoneId: zoneValue.ID}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "MAIL_API_TOKEN") {
		t.Errorf("message = %v", err)
	}
}

func TestTransportFailuresAreUnavailable(t *testing.T) {
	h, zoneValue := boundZone(t)
	h.engine.SetTransportError(fmt.Errorf("connection refused"))
	_, err := h.service.ListMailboxes(t.Context(), connect.NewRequest(&mailv1.ListMailboxesRequest{ZoneId: zoneValue.ID}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("message = %v", err)
	}
}

// TestDeriveClientHintsNamesOnlyWhatTheEngineOffers pins the fix for a mailbox
// response that told users to configure IMAP on 993 because that was a constant
// copied from Stalwart's defaults. Running a POP3-only engine, that hint pointed
// a mail client at a port nothing listened on.
func TestDeriveClientHintsNamesOnlyWhatTheEngineOffers(t *testing.T) {
	t.Parallel()

	srv := func(owner, target string, port int) autoconfigRecord {
		return autoconfigRecord{
			Name:  owner,
			Type:  "SRV",
			Value: fmt.Sprintf("0 1 %d %s.", port, strings.TrimSuffix(target, ".")),
		}
	}

	tests := []struct {
		name         string
		records      []autoconfigRecord
		wantProtocol string
		wantHost     string
		wantPort     uint32
		wantSMTPPort uint32
	}{
		{
			name:         "an engine offering IMAP is described as IMAP",
			records:      []autoconfigRecord{srv("_imaps._tcp", "mail.example.test", 993)},
			wantProtocol: "imaps", wantHost: "mail.example.test", wantPort: 993,
		},
		{
			// The bug: this engine speaks POP3 and no IMAP.
			name:         "a POP3-only engine is described as POP3, never IMAP",
			records:      []autoconfigRecord{srv("_pop3s._tcp", "mail.example.test", 995)},
			wantProtocol: "pop3s", wantHost: "mail.example.test", wantPort: 995,
		},
		{
			name: "IMAP wins when the engine offers both",
			records: []autoconfigRecord{
				srv("_pop3s._tcp", "mail.example.test", 995),
				srv("_imaps._tcp", "mail.example.test", 993),
			},
			wantProtocol: "imaps", wantHost: "mail.example.test", wantPort: 993,
		},
		{
			name:         "an engine advertising neither names no protocol at all",
			records:      []autoconfigRecord{srv("_submissions._tcp", "mail.example.test", 465)},
			wantProtocol: "", wantHost: "", wantPort: 0, wantSMTPPort: 465,
		},
		{
			name:         "no records at all is not an invented port",
			records:      nil,
			wantProtocol: "", wantHost: "", wantPort: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := deriveClientHints(test.records, "fallback.test")

			if got.RetrievalProtocol != test.wantProtocol {
				t.Errorf("protocol = %q, want %q", got.RetrievalProtocol, test.wantProtocol)
			}
			if got.RetrievalHost != test.wantHost {
				t.Errorf("host = %q, want %q", got.RetrievalHost, test.wantHost)
			}
			if got.RetrievalPort != test.wantPort {
				t.Errorf("port = %d, want %d", got.RetrievalPort, test.wantPort)
			}
			if got.SMTPPort != test.wantSMTPPort {
				t.Errorf("smtp port = %d, want %d", got.SMTPPort, test.wantSMTPPort)
			}
			if got.SMTPHost == "" {
				t.Error("smtp host is empty; it must fall back to the configured hostname")
			}
		})
	}
}
