// Package fakemail is an in-memory Stalwart. It implements the same engine
// contract as internal/mail/stalwart and reproduces the behaviour the facade
// has to survive: DKIM keys that appear a little after the domain does, the
// parenthesised multi-string RSA TXT of a real dnsZoneFile, the
// primaryKeyViolation and objectIsLinked set errors, and a queue whose
// forwarder fan-out carries the customer address only in orcpt.
//
// It deliberately depends on internal/mail/stalwart for the object types and
// the error shapes (never on internal/mail, which constructs it): a fake that
// invented its own error vocabulary would let the facade's mapping drift away
// from the server's.
package fakemail

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/castlemilk/dns/internal/mail/stalwart"
)

// DefaultHostname is what SystemSettings reports until SetHostname changes it.
const DefaultHostname = "mail.local.test"

// Engine is the in-memory server. The zero value is not usable; call New.
type Engine struct {
	// Clock is the fake's time source. Tests replace it to drive DKIM delay
	// and the selector dates.
	Clock func() time.Time

	mu        sync.Mutex
	hostname  string
	edition   string
	perms     []string
	dkimDelay time.Duration

	domains  []*domain
	accounts []*account
	lists    []*list
	queue    []stalwart.QueuedMessage

	nextID    int
	queueFull bool

	transportErr error
	unauthorized bool
	nextSetErr   *stalwart.SetError
}

type domain struct {
	id          string
	name        string
	description string
	enabled     bool
	reportURI   string
	createdAt   time.Time
	dkim        []stalwart.DkimSignature
	rsaAt       time.Time // when the RSA signature becomes visible
	rsa         stalwart.DkimSignature
}

type account struct {
	id          string
	localPart   string
	domainID    string
	description string
	quotaBytes  uint64
	usedBytes   uint64
	aliases     []stalwart.Alias
	createdAt   time.Time
}

type list struct {
	id          string
	localPart   string
	domainID    string
	description string
	recipients  []string
}

// New builds a fake with the full permission set, the community edition and a
// fixed clock, so a test that does not care about time gets stable selectors.
func New() *Engine {
	base := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	return &Engine{
		Clock:    func() time.Time { return base },
		hostname: DefaultHostname,
		edition:  "community",
		perms:    fullPermissions(),
	}
}

// fullPermissions is the set a correctly provisioned API key holds. It is
// spelled out rather than imported from internal/mail so this package stays a
// leaf of the facade.
func fullPermissions() []string {
	return []string{
		"sysDomainGet", "sysDomainCreate", "sysDomainUpdate", "sysDomainDestroy", "sysDomainQuery",
		"sysAccountGet", "sysAccountCreate", "sysAccountUpdate", "sysAccountDestroy", "sysAccountQuery",
		"sysAccountPasswordUpdate",
		"sysMailingListGet", "sysMailingListCreate", "sysMailingListUpdate", "sysMailingListDestroy", "sysMailingListQuery",
		"sysDkimSignatureGet", "sysDkimSignatureQuery", "sysDkimSignatureDestroy",
		"sysQueuedMessageGet", "sysQueuedMessageQuery",
		"sysSystemSettingsGet",
	}
}

// ---------------------------------------------------------------- hooks

// SetHostname changes the hostname SystemSettings reports. The facade must
// refuse to bind, and must stop writing MX, when it stops matching
// MAIL_HOSTNAME.
func (e *Engine) SetHostname(hostname string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hostname = hostname
}

// SetEdition changes the edition GET /api/account reports.
func (e *Engine) SetEdition(edition string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.edition = edition
}

// SetPermissions replaces the permission list, so a facade test can prove that
// a missing permission is reported rather than assumed.
func (e *Engine) SetPermissions(permissions []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.perms = append([]string(nil), permissions...)
}

// SetDkimDelay makes the RSA signature appear only after d has passed on the
// fake's clock. Ed25519 is always immediate, which is the real ordering.
func (e *Engine) SetDkimDelay(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dkimDelay = d
}

// Deliver adds bytes to a mailbox's usage, as a real delivery does.
func (e *Engine) Deliver(address string, bytes uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, entry := range e.accounts {
		if strings.EqualFold(e.addressOf(entry), address) {
			entry.usedBytes += bytes
			return
		}
		for _, alias := range entry.aliases {
			if strings.EqualFold(alias.Name+"@"+e.domainName(alias.DomainID), address) {
				entry.usedBytes += bytes
				return
			}
		}
	}
}

// EnqueueRemote adds a message queued for an external recipient. orcpt carries
// the customer address a forwarder fanned out from; pass "" for a direct send.
func (e *Engine) EnqueueRemote(from, to, orcpt, status string) {
	e.enqueue("remote", from, to, orcpt, status)
}

// EnqueueLocal adds a message queued for a local recipient.
func (e *Engine) EnqueueLocal(from, to, orcpt, status string) {
	e.enqueue("local", from, to, orcpt, status)
}

func (e *Engine) enqueue(queueName, from, to, orcpt, status string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.Clock().UTC()
	e.nextID++
	e.queue = append(e.queue, stalwart.QueuedMessage{
		ID:         fmt.Sprintf("q%d", e.nextID),
		ReturnPath: from,
		CreatedAt:  now,
		NextRetry:  now.Add(5 * time.Minute),
		Recipients: []stalwart.QueuedRecipient{{
			Address:    to,
			ORCPT:      orcpt,
			Status:     status,
			QueueName:  queueName,
			RetryCount: 1,
			RetryDue:   now.Add(5 * time.Minute),
		}},
	})
}

// SetQueueFull makes the next query return a full page, so the facade's
// "attribution incomplete → available=false" path is exercised.
func (e *Engine) SetQueueFull() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queueFull = true
}

// SetTransportError fails the next call with err. Pass nil to clear.
func (e *Engine) SetTransportError(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.transportErr = err
}

// SetUnauthorized makes every call answer ErrUnauthorized, as a rejected API
// key does.
func (e *Engine) SetUnauthorized() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.unauthorized = true
}

// FailNextSet makes the next mutating call fail with err. It covers the set
// errors the typed engine API cannot otherwise express — an unknown quota key,
// for instance, which the client never sends.
func (e *Engine) FailNextSet(err *stalwart.SetError) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextSetErr = err
}

// ---------------------------------------------------------------- engine

func (e *Engine) guard() error {
	if e.unauthorized {
		return stalwart.ErrUnauthorized
	}
	if err := e.transportErr; err != nil {
		e.transportErr = nil
		return fmt.Errorf("%w: %s", stalwart.ErrTransport, err.Error())
	}
	return nil
}

func (e *Engine) guardSet() error {
	if err := e.guard(); err != nil {
		return err
	}
	if err := e.nextSetErr; err != nil {
		e.nextSetErr = nil
		return err
	}
	return nil
}

func (e *Engine) Account(ctx context.Context) (stalwart.AccountInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guard()); err != nil {
		return stalwart.AccountInfo{}, err
	}
	return stalwart.AccountInfo{Edition: e.edition, Permissions: append([]string(nil), e.perms...)}, nil
}

func (e *Engine) SystemSettings(ctx context.Context) (stalwart.SystemSettings, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guard()); err != nil {
		return stalwart.SystemSettings{}, err
	}
	return stalwart.SystemSettings{
		DefaultHostname: e.hostname,
		MailExchangers:  []stalwart.MailExchanger{{Hostname: e.hostname, Priority: 10}},
	}, nil
}

func (e *Engine) FindDomain(ctx context.Context, name string) (stalwart.Domain, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guard()); err != nil {
		return stalwart.Domain{}, false, err
	}
	entry := e.findDomain(name)
	if entry == nil {
		return stalwart.Domain{}, false, nil
	}
	return e.view(entry), true, nil
}

func (e *Engine) ListDomains(ctx context.Context) ([]stalwart.Domain, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guard()); err != nil {
		return nil, err
	}
	domains := make([]stalwart.Domain, 0, len(e.domains))
	for _, entry := range e.domains {
		view := e.view(entry)
		view.DNSZoneFile = ""
		domains = append(domains, view)
	}
	return domains, nil
}

func (e *Engine) CreateDomain(ctx context.Context, in stalwart.DomainInput) (stalwart.Domain, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guardSet()); err != nil {
		return stalwart.Domain{}, err
	}
	if e.findDomain(in.Name) != nil {
		return stalwart.Domain{}, &stalwart.SetError{
			Method:      "x:Domain/set",
			Operation:   "create",
			Key:         "d1",
			Type:        stalwart.ErrorTypePrimaryKeyViolation,
			Description: "A domain with this name already exists",
			Properties:  []string{"name"},
		}
	}
	now := e.Clock().UTC()
	e.nextID++
	entry := &domain{
		id:          fmt.Sprintf("d%d", e.nextID),
		name:        strings.ToLower(in.Name),
		description: in.Description,
		enabled:     true,
		reportURI:   in.ReportAddressURI,
		createdAt:   now,
	}
	stamp := now.Format("20060102")
	entry.dkim = []stalwart.DkimSignature{{
		ID:        entry.id + "-ed25519",
		Selector:  "v1-ed25519-" + stamp,
		Algorithm: stalwart.DkimAlgorithmEd25519,
		Stage:     stalwart.DkimStageActive,
		PublicKey: "5t9iUKLc7NJ5SmfF8N9DBNAn74vuywt3j5mCnUDQmCM=",
		CreatedAt: now,
	}}
	entry.rsa = stalwart.DkimSignature{
		ID:        entry.id + "-rsa",
		Selector:  "v1-rsa-" + stamp,
		Algorithm: stalwart.DkimAlgorithmRSA,
		Stage:     stalwart.DkimStageActive,
		PublicKey: fakeRSAPublicKey,
		CreatedAt: now,
	}
	entry.rsaAt = now.Add(e.dkimDelay)
	e.domains = append(e.domains, entry)
	return e.view(entry), nil
}

func (e *Engine) GetDomain(ctx context.Context, id string) (stalwart.Domain, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guard()); err != nil {
		return stalwart.Domain{}, err
	}
	entry := e.domainByID(id)
	if entry == nil {
		return stalwart.Domain{}, fmt.Errorf("%w: domain", stalwart.ErrNotFound)
	}
	return e.view(entry), nil
}

func (e *Engine) UpdateDomain(ctx context.Context, id string, patch stalwart.DomainPatch) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guardSet()); err != nil {
		return err
	}
	entry := e.domainByID(id)
	if entry == nil {
		return &stalwart.SetError{Method: "x:Domain/set", Operation: "update", Key: id, Type: stalwart.ErrorTypeNotFound}
	}
	if patch.Description != nil {
		entry.description = *patch.Description
	}
	if patch.ReportAddressURI != nil {
		entry.reportURI = *patch.ReportAddressURI
	}
	if patch.Enabled != nil {
		entry.enabled = *patch.Enabled
	}
	return nil
}

func (e *Engine) DestroyDomain(ctx context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guardSet()); err != nil {
		return err
	}
	entry := e.domainByID(id)
	if entry == nil {
		return nil
	}
	var linked []stalwart.LinkedObject
	for _, value := range e.accounts {
		if value.domainID == id {
			linked = append(linked, stalwart.LinkedObject{Object: "Account", ID: value.id})
		}
	}
	for _, value := range e.lists {
		if value.domainID == id {
			linked = append(linked, stalwart.LinkedObject{Object: "MailingList", ID: value.id})
		}
	}
	for _, value := range e.visibleDkim(entry) {
		linked = append(linked, stalwart.LinkedObject{Object: "DkimSignature", ID: value.ID})
	}
	if len(linked) > 0 {
		return &stalwart.SetError{
			Method:        "x:Domain/set",
			Operation:     "destroy",
			Key:           id,
			Type:          stalwart.ErrorTypeObjectIsLinked,
			LinkedObjects: linked,
		}
	}
	e.domains = removeFunc(e.domains, func(value *domain) bool { return value.id == id })
	return nil
}

func (e *Engine) ListDkim(ctx context.Context, domainID string) ([]stalwart.DkimSignature, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guard()); err != nil {
		return nil, err
	}
	entry := e.domainByID(domainID)
	if entry == nil {
		return nil, nil
	}
	return e.visibleDkim(entry), nil
}

func (e *Engine) DestroyDkim(ctx context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guardSet()); err != nil {
		return err
	}
	for _, entry := range e.domains {
		entry.dkim = removeFunc(entry.dkim, func(value stalwart.DkimSignature) bool { return value.ID == id })
		if entry.rsa.ID == id {
			entry.rsa = stalwart.DkimSignature{}
		}
	}
	return nil
}

func (e *Engine) ListAccounts(ctx context.Context, domainID string) ([]stalwart.Account, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guard()); err != nil {
		return nil, err
	}
	accounts := make([]stalwart.Account, 0, len(e.accounts))
	for _, entry := range e.accounts {
		if entry.domainID != domainID {
			continue
		}
		accounts = append(accounts, e.accountView(entry))
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].EmailAddress < accounts[j].EmailAddress })
	return accounts, nil
}

func (e *Engine) CreateAccount(ctx context.Context, in stalwart.AccountInput) (stalwart.Account, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guardSet()); err != nil {
		return stalwart.Account{}, err
	}
	if e.domainByID(in.DomainID) == nil {
		return stalwart.Account{}, &stalwart.SetError{Method: "x:Account/set", Operation: "create", Key: "u1", Type: stalwart.ErrorTypeNotFound}
	}
	if e.addressTaken(in.DomainID, in.LocalPart) {
		return stalwart.Account{}, &stalwart.SetError{
			Method:      "x:Account/set",
			Operation:   "create",
			Key:         "u1",
			Type:        stalwart.ErrorTypePrimaryKeyViolation,
			Description: "An account with this email address already exists",
			Properties:  []string{"email"},
		}
	}
	e.nextID++
	entry := &account{
		id:          fmt.Sprintf("a%d", e.nextID),
		localPart:   strings.ToLower(in.LocalPart),
		domainID:    in.DomainID,
		description: in.Description,
		quotaBytes:  in.QuotaBytes,
		createdAt:   e.Clock().UTC(),
	}
	e.accounts = append(e.accounts, entry)
	return e.accountView(entry), nil
}

func (e *Engine) UpdateAccount(ctx context.Context, id string, patch stalwart.AccountPatch) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guardSet()); err != nil {
		return err
	}
	entry := e.accountByID(id)
	if entry == nil {
		return &stalwart.SetError{Method: "x:Account/set", Operation: "update", Key: id, Type: stalwart.ErrorTypeNotFound}
	}
	if patch.Description != nil {
		entry.description = *patch.Description
	}
	if patch.QuotaBytes != nil {
		entry.quotaBytes = *patch.QuotaBytes
	}
	if patch.Aliases != nil {
		for _, alias := range *patch.Aliases {
			if e.aliasTakenElsewhere(entry, alias) {
				return &stalwart.SetError{
					Method:     "x:Account/set",
					Operation:  "update",
					Key:        id,
					Type:       stalwart.ErrorTypePrimaryKeyViolation,
					Properties: []string{"email"},
				}
			}
		}
		entry.aliases = append([]stalwart.Alias(nil), *patch.Aliases...)
	}
	return nil
}

func (e *Engine) DestroyAccount(ctx context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guardSet()); err != nil {
		return err
	}
	e.accounts = removeFunc(e.accounts, func(value *account) bool { return value.id == id })
	return nil
}

func (e *Engine) ListLists(ctx context.Context, domainID string) ([]stalwart.MailingList, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guard()); err != nil {
		return nil, err
	}
	// The real query cannot filter by domain, so it returns everything and the
	// client matches; an empty domainID must therefore list every domain's
	// lists here too, or the facade's rebuild path would look filtered.
	lists := make([]stalwart.MailingList, 0, len(e.lists))
	for _, entry := range e.lists {
		if domainID != "" && entry.domainID != domainID {
			continue
		}
		lists = append(lists, e.listView(entry))
	}
	sort.Slice(lists, func(i, j int) bool { return lists[i].EmailAddress < lists[j].EmailAddress })
	return lists, nil
}

func (e *Engine) CreateList(ctx context.Context, in stalwart.ListInput) (stalwart.MailingList, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guardSet()); err != nil {
		return stalwart.MailingList{}, err
	}
	if e.domainByID(in.DomainID) == nil {
		return stalwart.MailingList{}, &stalwart.SetError{Method: "x:MailingList/set", Operation: "create", Key: "l1", Type: stalwart.ErrorTypeNotFound}
	}
	if e.addressTaken(in.DomainID, in.LocalPart) {
		return stalwart.MailingList{}, &stalwart.SetError{
			Method:     "x:MailingList/set",
			Operation:  "create",
			Key:        "l1",
			Type:       stalwart.ErrorTypePrimaryKeyViolation,
			Properties: []string{"name"},
		}
	}
	e.nextID++
	entry := &list{
		id:          fmt.Sprintf("l%d", e.nextID),
		localPart:   strings.ToLower(in.LocalPart),
		domainID:    in.DomainID,
		description: in.Description,
		recipients:  append([]string(nil), in.Recipients...),
	}
	sort.Strings(entry.recipients)
	e.lists = append(e.lists, entry)
	return e.listView(entry), nil
}

func (e *Engine) DestroyList(ctx context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guardSet()); err != nil {
		return err
	}
	e.lists = removeFunc(e.lists, func(value *list) bool { return value.id == id })
	return nil
}

func (e *Engine) QueryQueue(ctx context.Context, zoneName string, limit int) ([]stalwart.QueuedMessage, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := contextOr(ctx, e.guard()); err != nil {
		return nil, false, err
	}
	if limit <= 0 || limit > stalwart.MaxObjectsPerCall {
		limit = stalwart.MaxObjectsPerCall
	}
	page := e.queue
	complete := true
	if e.queueFull {
		// A full page means attribution is incomplete; the facade must report
		// "not available" rather than a count that is silently a lower bound.
		complete = false
		page = make([]stalwart.QueuedMessage, 0, stalwart.MaxObjectsPerCall)
		page = append(page, e.queue...)
		for len(page) < stalwart.MaxObjectsPerCall {
			page = append(page, stalwart.QueuedMessage{
				ID:         fmt.Sprintf("filler-%d", len(page)),
				ReturnPath: "noise@elsewhere.invalid",
				Recipients: []stalwart.QueuedRecipient{{
					Address:   "someone@elsewhere.invalid",
					Status:    stalwart.QueueStatusScheduled,
					QueueName: "remote",
				}},
			})
		}
	}
	messages := make([]stalwart.QueuedMessage, 0, len(page))
	for _, message := range page {
		if !stalwart.BelongsToZone(message, zoneName) {
			continue
		}
		messages = append(messages, message)
		if len(messages) >= limit {
			break
		}
	}
	return messages, complete, nil
}

// ---------------------------------------------------------------- helpers

func (e *Engine) findDomain(name string) *domain {
	for _, entry := range e.domains {
		if strings.EqualFold(entry.name, name) {
			return entry
		}
	}
	return nil
}

func (e *Engine) domainByID(id string) *domain {
	for _, entry := range e.domains {
		if entry.id == id {
			return entry
		}
	}
	return nil
}

func (e *Engine) domainName(id string) string {
	if entry := e.domainByID(id); entry != nil {
		return entry.name
	}
	return ""
}

func (e *Engine) accountByID(id string) *account {
	for _, entry := range e.accounts {
		if entry.id == id {
			return entry
		}
	}
	return nil
}

func (e *Engine) addressOf(entry *account) string {
	return entry.localPart + "@" + e.domainName(entry.domainID)
}

// addressTaken mirrors the server's uniqueness rule: an address collides with a
// mailbox, with any mailbox's alias, and with a mailing list.
func (e *Engine) addressTaken(domainID, localPart string) bool {
	localPart = strings.ToLower(localPart)
	for _, entry := range e.accounts {
		if entry.domainID == domainID && entry.localPart == localPart {
			return true
		}
		for _, alias := range entry.aliases {
			if alias.DomainID == domainID && strings.EqualFold(alias.Name, localPart) {
				return true
			}
		}
	}
	for _, entry := range e.lists {
		if entry.domainID == domainID && entry.localPart == localPart {
			return true
		}
	}
	return false
}

func (e *Engine) aliasTakenElsewhere(owner *account, alias stalwart.Alias) bool {
	for _, entry := range e.accounts {
		if entry.domainID == alias.DomainID && strings.EqualFold(entry.localPart, alias.Name) {
			return true
		}
		if entry == owner {
			continue
		}
		for _, existing := range entry.aliases {
			if existing.DomainID == alias.DomainID && strings.EqualFold(existing.Name, alias.Name) {
				return true
			}
		}
	}
	for _, entry := range e.lists {
		if entry.domainID == alias.DomainID && strings.EqualFold(entry.localPart, alias.Name) {
			return true
		}
	}
	return false
}

func (e *Engine) accountView(entry *account) stalwart.Account {
	return stalwart.Account{
		ID:           entry.id,
		EmailAddress: e.addressOf(entry),
		LocalPart:    entry.localPart,
		DomainID:     entry.domainID,
		Description:  entry.description,
		QuotaBytes:   entry.quotaBytes,
		UsedBytes:    entry.usedBytes,
		Aliases:      append([]stalwart.Alias(nil), entry.aliases...),
		CreatedAt:    entry.createdAt,
	}
}

func (e *Engine) listView(entry *list) stalwart.MailingList {
	return stalwart.MailingList{
		ID:           entry.id,
		Name:         entry.localPart,
		DomainID:     entry.domainID,
		Description:  entry.description,
		EmailAddress: entry.localPart + "@" + e.domainName(entry.domainID),
		Recipients:   append([]string(nil), entry.recipients...),
	}
}

// visibleDkim is the signature set a caller sees now: Ed25519 immediately, RSA
// once the configured delay has passed on the fake's clock.
func (e *Engine) visibleDkim(entry *domain) []stalwart.DkimSignature {
	signatures := append([]stalwart.DkimSignature(nil), entry.dkim...)
	if entry.rsa.ID != "" && !e.Clock().UTC().Before(entry.rsaAt) {
		signatures = append(signatures, entry.rsa)
	}
	sort.Slice(signatures, func(i, j int) bool { return signatures[i].Selector < signatures[j].Selector })
	return signatures
}

func (e *Engine) view(entry *domain) stalwart.Domain {
	return stalwart.Domain{
		ID:          entry.id,
		Name:        entry.name,
		Description: entry.description,
		Enabled:     entry.enabled,
		DNSZoneFile: e.zoneFile(entry),
		CreatedAt:   entry.createdAt,
	}
}

// zoneFile renders the same layout a real server does: absolute owner names,
// no $ORIGIN and no TTLs, the RSA key wrapped in parentheses as two quoted
// strings, and the full set of MX/SPF/DMARC/SRV/MTA-STS/autoconfig lines — the
// ones the facade derives itself, the ones it republishes behind
// publish_client_autoconfig, and the ones it must never publish at all. It is a
// line-for-line copy of the output captured from v0.16.20 in
// internal/mail/stalwart/testdata/zonefile_probe.txt.
func (e *Engine) zoneFile(entry *domain) string {
	var out strings.Builder
	for _, signature := range e.visibleDkim(entry) {
		owner := signature.Selector + "._domainkey." + entry.name + "."
		switch signature.Algorithm {
		case stalwart.DkimAlgorithmRSA:
			value := "v=DKIM1; k=rsa; h=sha256; p=" + signature.PublicKey
			head, tail := value[:255], value[255:]
			fmt.Fprintf(&out, "%s IN TXT (\n    %q\n    %q\n)\n", owner, head, tail)
		default:
			fmt.Fprintf(&out, "%s IN TXT %q\n", owner, "v=DKIM1; k=ed25519; h=sha256; p="+signature.PublicKey)
		}
	}
	report := entry.reportURI
	report = strings.TrimPrefix(report, "mailto:")
	if !strings.Contains(report, "@") {
		report = report + "@" + entry.name
	}
	fmt.Fprintf(&out, "%s. IN TXT %q\n", entry.name, "v=spf1 mx -all")
	fmt.Fprintf(&out, "%s. IN MX 10 %s.\n", entry.name, e.hostname)
	fmt.Fprintf(&out, "_dmarc.%s. IN TXT %q\n", entry.name, "v=DMARC1; p=reject; rua=mailto:"+report)
	for _, service := range []struct {
		name string
		port int
	}{{"_caldavs", 443}, {"_carddavs", 443}, {"_imaps", 993}, {"_jmap", 443}, {"_pop3s", 995}, {"_submissions", 465}} {
		fmt.Fprintf(&out, "%s._tcp.%s. IN SRV 0 1 %d %s.\n", service.name, entry.name, service.port, e.hostname)
	}
	fmt.Fprintf(&out, "mta-sts.%s. IN CNAME %s.\n", entry.name, e.hostname)
	fmt.Fprintf(&out, "_mta-sts.%s. IN TXT %q\n", entry.name, "v=STSv1; id=4966920683339627112")
	fmt.Fprintf(&out, "_smtp._tls.%s. IN TXT %q\n", entry.name, "v=TLSRPTv1; rua=mailto:"+report)
	// ua-auto-config is the server's own draft autoconfiguration scheme. It
	// looks exactly like the two lines below and must not be published, so the
	// fake emits it: without it no test would prove the allow-list excludes it.
	fmt.Fprintf(&out, "ua-auto-config.%s. IN CNAME %s.\n", entry.name, e.hostname)
	fmt.Fprintf(&out, "_ua-auto-config.%s. IN TXT %q\n", entry.name,
		"v=UAAC1; a=sha256; d=ChkkELbHwgcLV5jhN3d6hfEbPq1fXmI1kESn0hHLMgc=")
	fmt.Fprintf(&out, "autoconfig.%s. IN CNAME %s.\n", entry.name, e.hostname)
	fmt.Fprintf(&out, "autodiscover.%s. IN CNAME %s.\n", entry.name, e.hostname)
	// The server emits an SPF line for the domain that owns the mail hostname;
	// it is another owner and the parser must skip it.
	fmt.Fprintf(&out, "%s. IN TXT %q\n", e.hostname, "v=spf1 a -all")
	return out.String()
}

func contextOr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func removeFunc[T any](values []T, match func(T) bool) []T {
	kept := values[:0]
	for _, value := range values {
		if match(value) {
			continue
		}
		kept = append(kept, value)
	}
	return kept
}

// fakeRSAPublicKey is a real 2048-bit RSA public key in base64, so the rendered
// TXT value is long enough to exercise the parenthesised split form.
const fakeRSAPublicKey = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAwS6eGK8srmBUElDQbZiWywz6WPZL1cDihRY9rzl7t7+VLT8WyNbTykd45GxHJ5iojuk2cUlkwQ2zSJLoX7BKp/O3HtoHW/KmSz6Xt8FRlMPn0gN9GAjuHL6oHGEn/YwN6v7glNBtGmlmjEwUxvrZs/NmtWL7rXIWQG5CiC8Qz4JodQRApilp+sPyIJd60baX2lnbonM4CdGZ5TLerMixoy45Z2NSLLuVe3X5WtmVziz7p/fGCDmC/HvUAKtFY+O/i66x7dYas5OE/uhl2XOJTD6rYetEiXzD9RCiRpGqcDclVvNcg8FF08YL3zW2hJ0bMGZMfucSeH03NMJOOxH41wIDAQAB"
