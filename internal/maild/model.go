package maild

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DocVersion is the schema version stamped on every stored document. A reader
// that finds a higher version refuses the file rather than dropping fields it
// cannot represent.
const DocVersion = 1

// -----------------------------------------------------------------------------
// Wire vocabulary
//
// Every constant below is a literal the frozen client in
// internal/mail/stalwart sends or matches. They are declared here rather than
// imported so this package stays independent of the control plane; store_test.go
// asserts each one against the stalwart package, so a drift fails the build
// instead of a customer's mail binding. See CONTRACT.md.
// -----------------------------------------------------------------------------

// JMAP capabilities. The client sends exactly these two, in this order, in
// every request's "using" list.
const (
	CapabilityCore     = "urn:ietf:params:jmap:core"
	CapabilityStalwart = "urn:stalwart:jmap"
)

// Transport limits. The client refuses to send more than MaxCallsPerRequest
// calls, pages every unfiltered query at MaxObjectsPerCall, and reads at most
// MaxResponseBytes of a response.
const (
	MaxCallsPerRequest = 16
	MaxObjectsPerCall  = 500
	MaxResponseBytes   = 10 << 20
)

// Method names. This is the complete set the client ever issues; anything else
// must answer the unknownMethod error.
const (
	MethodSystemSettingsGet = "x:SystemSettings/get"

	MethodDomainQuery = "x:Domain/query"
	MethodDomainGet   = "x:Domain/get"
	MethodDomainSet   = "x:Domain/set"

	MethodDkimQuery = "x:DkimSignature/query"
	MethodDkimGet   = "x:DkimSignature/get"
	MethodDkimSet   = "x:DkimSignature/set"

	MethodAccountQuery = "x:Account/query"
	MethodAccountGet   = "x:Account/get"
	MethodAccountSet   = "x:Account/set"

	MethodListQuery = "x:MailingList/query"
	MethodListGet   = "x:MailingList/get"
	MethodListSet   = "x:MailingList/set"

	MethodQueueQuery = "x:QueuedMessage/query"
	MethodQueueGet   = "x:QueuedMessage/get"
)

// Methods is the complete set, for a router that wants to reject everything
// else with unknownMethod.
var Methods = []string{
	MethodSystemSettingsGet,
	MethodDomainQuery, MethodDomainGet, MethodDomainSet,
	MethodDkimQuery, MethodDkimGet, MethodDkimSet,
	MethodAccountQuery, MethodAccountGet, MethodAccountSet,
	MethodListQuery, MethodListGet, MethodListSet,
	MethodQueueQuery, MethodQueueGet,
}

// SingletonID is the only id x:SystemSettings/get accepts. A bare /get with no
// ids returns an empty list.
const SingletonID = "singleton"

// JMAP error types. The first block is the set internal/mail/stalwart/errors.go
// recognises by name; invalidResultReference is JMAP's own and is what a broken
// "#ids" chain must answer with.
const (
	ErrTypeForbidden              = "forbidden"
	ErrTypeUnsupportedFilter      = "unsupportedFilter"
	ErrTypeInvalidPatch           = "invalidPatch"
	ErrTypeInvalidArguments       = "invalidArguments"
	ErrTypeInvalidProperties      = "invalidProperties"
	ErrTypeObjectIsLinked         = "objectIsLinked"
	ErrTypeNotFound               = "notFound"
	ErrTypeAccountNotFound        = "accountNotFound"
	ErrTypePrimaryKeyViolation    = "primaryKeyViolation"
	ErrTypeAlreadyExists          = "alreadyExists"
	ErrTypeUnknownMethod          = "unknownMethod"
	ErrTypeStateMismatch          = "stateMismatch"
	ErrTypeOverQuota              = "overQuota"
	ErrTypeTooLarge               = "tooLarge"
	ErrTypeRateLimit              = "rateLimit"
	ErrTypeInvalidResultReference = "invalidResultReference"
)

// DKIM algorithm names. These are the JMAP object's "@type", and the facade
// waits for one active signature of each before a binding is healthy. RSA is
// not optional: Gmail and Microsoft verify RSA only.
const (
	DkimAlgorithmEd25519 = "Dkim1Ed25519Sha256"
	DkimAlgorithmRSA     = "Dkim1RsaSha256"
)

// RequestedDkimAlgorithms is the set every domain is created with, and the set
// the facade's bind step waits for before it publishes.
var RequestedDkimAlgorithms = []string{DkimAlgorithmEd25519, DkimAlgorithmRSA}

// DKIM lifecycle stages. Only StageActive is published.
const (
	DkimStagePending  = "pending"
	DkimStageActive   = "active"
	DkimStageRetiring = "retiring"
	DkimStageRetired  = "retired"
)

// DkimSelectorTag is the "{algorithm}" substitution of the selector template:
// the short name, not the "@type". The probe capture expands
// "v{version}-{algorithm}-{date-%Y%m%d}" to "v1-rsa-20260903".
var DkimSelectorTag = map[string]string{
	DkimAlgorithmEd25519: "ed25519",
	DkimAlgorithmRSA:     "rsa",
}

// DkimKeyType is the "k=" tag of the published TXT record. zonefile.go rejects
// a record whose k= is anything else, and a rejected record is never published.
var DkimKeyType = map[string]string{
	DkimAlgorithmEd25519: "ed25519",
	DkimAlgorithmRSA:     "rsa",
}

// DefaultSelectorTemplate is what the facade sends on every domain create.
const DefaultSelectorTemplate = "v{version}-{algorithm}-{date-%Y%m%d}"

// Queue statuses, the "@type" of x:QueuedRecipient.status.
const (
	QueueStatusScheduled        = "Scheduled"
	QueueStatusCompleted        = "Completed"
	QueueStatusTemporaryFailure = "TemporaryFailure"
	QueueStatusPermanentFailure = "PermanentFailure"
)

// GrantedPermissions is what GET /api/account reports. It must be a superset of
// internal/mail.RequiredPermissions, because Probe lists every required name the
// credential does not hold and shows the difference to the operator — an absent
// name is reported, never assumed. maild has no permission model of its own, so
// it reports the whole set. store_test.go asserts the two agree.
var GrantedPermissions = []string{
	"authenticate",
	"sysDomainGet", "sysDomainCreate", "sysDomainUpdate", "sysDomainDestroy", "sysDomainQuery",
	"sysAccountGet", "sysAccountCreate", "sysAccountUpdate", "sysAccountDestroy", "sysAccountQuery",
	"sysAccountPasswordUpdate",
	"sysMailingListGet", "sysMailingListCreate", "sysMailingListUpdate", "sysMailingListDestroy", "sysMailingListQuery",
	"sysDkimSignatureGet", "sysDkimSignatureQuery", "sysDkimSignatureDestroy",
	"sysQueuedMessageGet", "sysQueuedMessageQuery",
	"sysSystemSettingsGet",
}

// Edition is what GET /api/account reports as its edition. Nothing compares it;
// it is shown to the operator on the Settings page, so it names this server
// rather than pretending to be a Stalwart edition.
const Edition = "maild"

// -----------------------------------------------------------------------------
// Secrets
// -----------------------------------------------------------------------------

// Secret is a byte slice that never prints itself. It marshals as base64 like
// any []byte, so it round-trips through the store, but %v, %s and %#v all yield
// the same redaction — which is what keeps a DKIM private key out of a log line
// that formats the struct holding it.
type Secret []byte

func (Secret) String() string { return "[redacted]" }

// GoString covers %#v, which would otherwise dump the bytes.
func (Secret) GoString() string { return "[redacted]" }

// Bytes exposes the raw value at the one point that needs it — signing, or a
// key parse. Naming it makes every such use greppable.
func (s Secret) Bytes() []byte { return []byte(s) }

// -----------------------------------------------------------------------------
// Stored model
// -----------------------------------------------------------------------------

// Domain is one hosted mail domain: the object behind x:Domain.
type Domain struct {
	V  int    `json:"v"`
	ID string `json:"id"`
	// Name is the domain, lowercased. It is the natural key: ID is derived
	// from it, so a lookup by name needs no index.
	Name string `json:"name"`
	// Description is opaque to this server and must round-trip byte for byte.
	// The facade writes "simple zone <zoneID>" there and adopts an existing
	// domain only when it matches exactly, so a domain somebody else created is
	// never quietly taken over.
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	// ReportAddressURI is the "mailto:…" the facade sends. It is where this
	// server should address its own DMARC/TLS reports; the rua= tag the facade
	// publishes is derived separately from its own configuration.
	ReportAddressURI string `json:"report_address_uri,omitempty"`
	// SubAddressing enables plus-addressing (user+tag@domain).
	SubAddressing bool `json:"sub_addressing,omitempty"`
	// AllowRelaying is always false from the facade; a true here would make this
	// domain an open relay for authenticated-elsewhere senders.
	AllowRelaying bool `json:"allow_relaying,omitempty"`
	// CatchAllLocalPart is empty when catchAllAddress is null, which is what the
	// facade always sends.
	CatchAllLocalPart string    `json:"catch_all_local_part,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at,omitzero"`
}

// Alias is a local part in the same domain that delivers into an account. It is
// how the facade expresses a forwarder whose single target is a local mailbox:
// the mail is stored, not relayed.
type Alias struct {
	LocalPart   string `json:"local_part"`
	DomainID    string `json:"domain_id"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// Account is one mailbox: the object behind x:Account of "@type" User.
type Account struct {
	V         int    `json:"v"`
	ID        string `json:"id"`
	DomainID  string `json:"domain_id"`
	LocalPart string `json:"local_part"`
	// Description is the display name. Empty means none; the facade clears it by
	// sending JSON null.
	Description string `json:"description,omitempty"`
	// Password is the derived credential. The plaintext arrives once, on
	// x:Account/set create or a credentials patch, and is dropped immediately.
	// No wire object carries this field.
	Password PasswordHash `json:"password,omitzero"`
	// QuotaBytes is quotas.maxDiskQuota; 0 means unlimited.
	QuotaBytes uint64 `json:"quota_bytes,omitempty"`
	// UsedBytes is reported live as usedDiskQuota. The console shows it as real
	// usage, so it must be the real figure or zero, never an estimate.
	UsedBytes uint64    `json:"used_bytes,omitempty"`
	Aliases   []Alias   `json:"aliases,omitempty"`
	Locale    string    `json:"locale,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// List is a mailing list: the object behind x:MailingList, and the mechanism
// behind a forwarder with an external target or several targets. Unlike an
// alias it relays, which is why the console shows the SRS caveat for it.
type List struct {
	V         int    `json:"v"`
	ID        string `json:"id"`
	DomainID  string `json:"domain_id"`
	LocalPart string `json:"local_part"`
	// Description is the marker "forwarder" for lists this product created.
	Description string    `json:"description,omitempty"`
	Recipients  []string  `json:"recipients,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
}

// DkimKey is one signing key: the object behind x:DkimSignature, plus the
// private half, which never leaves this process.
type DkimKey struct {
	V        int    `json:"v"`
	ID       string `json:"id"`
	DomainID string `json:"domain_id"`
	// Selector is the first label of the published record's owner
	// ("v1-rsa-20260903"), and the join key the facade matches on.
	Selector string `json:"selector"`
	// Algorithm is DkimAlgorithmEd25519 or DkimAlgorithmRSA.
	Algorithm string `json:"algorithm"`
	Stage     string `json:"stage"`
	// Version is the {version} substitution of the selector template, from 1.
	Version int `json:"version"`
	// PublicKey is the base64 the TXT record's p= tag carries: for RSA the DER
	// SubjectPublicKeyInfo, for Ed25519 the raw 32-byte key.
	PublicKey string `json:"public_key"`
	// PrivateKey is PKCS#8 DER. It is a Secret so a struct dump cannot leak it,
	// and no wire object includes it.
	PrivateKey Secret    `json:"private_key,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	RotateAt   time.Time `json:"rotate_at,omitzero"`
	RetireAt   time.Time `json:"retire_at,omitzero"`
	DeleteAt   time.Time `json:"delete_at,omitzero"`
}

// Active reports whether this key is one the domain currently signs with, and
// therefore one whose public key must be published.
func (k DkimKey) Active() bool { return k.Stage == DkimStageActive }

// QueuedRecipient is one recipient of a message still in the outbound queue.
type QueuedRecipient struct {
	Address string `json:"address"`
	// ORCPT is the original recipient of a forwarder fan-out: the external
	// target appears as Address and the customer's own address only here, so
	// without it the message is attributable to no hosted zone. Stored bare;
	// the wire form adds the "rfc822;" prefix.
	ORCPT      string    `json:"orcpt,omitempty"`
	Status     string    `json:"status"`
	QueueName  string    `json:"queue_name,omitempty"`
	RetryCount uint32    `json:"retry_count,omitempty"`
	RetryDue   time.Time `json:"retry_due,omitzero"`
	// LastCode and LastMessage are the last SMTP response. They are reported on
	// the wire for display and must never carry a credential.
	LastCode    int    `json:"last_code,omitempty"`
	LastMessage string `json:"last_message,omitempty"`
}

// QueuedMessage is one message still in the outbound queue.
//
// The store has a "queue" bucket reserved for these but no CRUD for them: the
// delivery phase owns that, and the bucket exists already so adding it needs no
// migration.
type QueuedMessage struct {
	V          int               `json:"v"`
	ID         string            `json:"id"`
	ReturnPath string            `json:"return_path"`
	Recipients []QueuedRecipient `json:"recipients,omitempty"`
	// MessagePath is where the spooled body lives, relative to the spool
	// directory. The body is never held in the store.
	MessagePath string    `json:"message_path,omitempty"`
	SizeBytes   int64     `json:"size_bytes,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	NextRetry   time.Time `json:"next_retry,omitzero"`
}

// SystemSettings is the x:SystemSettings singleton. DefaultHostname is the one
// value the whole facade gates on: it must equal MAIL_HOSTNAME or every mail
// operation refuses, because the MX record the facade publishes names
// MAIL_HOSTNAME and binding against a server that calls itself something else
// would route mail to a host that does not serve these mailboxes.
type SystemSettings struct {
	DefaultHostname string
	DefaultDomainID string
	MailExchangers  []MailExchanger
}

// MailExchanger is one advertised MX. It is read for display only: the record
// the facade publishes comes from MAIL_HOSTNAME and MAIL_MX_PRIORITY, never
// from here.
type MailExchanger struct {
	// Hostname empty means "the default hostname"; the wire form sends null.
	Hostname string
	Priority uint16
}

// -----------------------------------------------------------------------------
// Constructors
// -----------------------------------------------------------------------------

// NewDomain builds a domain with its derived id and normalised name.
func NewDomain(name string, now time.Time) Domain {
	normalized := NormalizeDomainName(name)
	return Domain{
		V:         DocVersion,
		ID:        DomainID(normalized),
		Name:      normalized,
		Enabled:   true,
		CreatedAt: now.UTC(),
	}
}

// NewAccount builds a mailbox with its derived id.
func NewAccount(domainID, localPart string, now time.Time) Account {
	normalized := NormalizeLocalPart(localPart)
	return Account{
		V:         DocVersion,
		ID:        AccountID(domainID, normalized),
		DomainID:  domainID,
		LocalPart: normalized,
		CreatedAt: now.UTC(),
	}
}

// NewList builds a mailing list with its derived id.
func NewList(domainID, localPart string, now time.Time) List {
	normalized := NormalizeLocalPart(localPart)
	return List{
		V:         DocVersion,
		ID:        ListID(domainID, normalized),
		DomainID:  domainID,
		LocalPart: normalized,
		CreatedAt: now.UTC(),
	}
}

// NewDkimKey builds a signing key with its derived id, pending until the caller
// makes it active.
func NewDkimKey(domainID, selector, algorithm string, now time.Time) DkimKey {
	return DkimKey{
		V:         DocVersion,
		ID:        DkimID(domainID, selector),
		DomainID:  domainID,
		Selector:  selector,
		Algorithm: algorithm,
		Stage:     DkimStagePending,
		Version:   1,
		CreatedAt: now.UTC(),
	}
}

// Address is the mailbox's full address. It needs the domain because an account
// stores only its local part.
func (a Account) Address(domainName string) string {
	return a.LocalPart + "@" + domainName
}

// Address is the list's full address.
func (l List) Address(domainName string) string {
	return l.LocalPart + "@" + domainName
}

// NormalizeDomainName lowercases a domain and strips the root dot and spaces.
// Every stored name is in this form, so DomainID is stable across spellings.
func NormalizeDomainName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// NormalizeLocalPart lowercases a local part. The facade already restricts the
// character set; this only makes the key stable.
func NormalizeLocalPart(local string) string {
	return strings.ToLower(strings.TrimSpace(local))
}

// SplitAddress splits an address into its normalised local part and domain.
// ok is false when there is no single "@".
func SplitAddress(address string) (local, domain string, ok bool) {
	local, domain, found := strings.Cut(strings.TrimSpace(address), "@")
	if !found || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", "", false
	}
	return NormalizeLocalPart(local), NormalizeDomainName(domain), true
}

// -----------------------------------------------------------------------------
// Wire objects
//
// One struct per JMAP object, with the exact JSON tags the frozen client
// decodes. Do not rename a tag: the client's payload structs are the contract.
// -----------------------------------------------------------------------------

// WireDomain is one x:Domain. Note isEnabled, not enabled — a domain and an
// alias spell it differently and both spellings are the client's.
type WireDomain struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Enabled     bool    `json:"isEnabled"`
	// DNSZoneFile is the rendered BIND text. It is omitted when the caller did
	// not ask for it: it is the expensive property, and the list query
	// deliberately leaves it out.
	DNSZoneFile string `json:"dnsZoneFile,omitempty"`
	CreatedAt   string `json:"createdAt"`
}

// Wire renders the domain. zoneFile is "" when dnsZoneFile was not requested.
func (d Domain) Wire(zoneFile string) WireDomain {
	return WireDomain{
		ID:          d.ID,
		Name:        d.Name,
		Description: nullableString(d.Description),
		Enabled:     d.Enabled,
		DNSZoneFile: zoneFile,
		CreatedAt:   wireTime(d.CreatedAt),
	}
}

// WireQuotas is x:Account.quotas.
type WireQuotas struct {
	MaxDiskQuota uint64 `json:"maxDiskQuota"`
}

// WireAlias is one entry of x:Account.aliases. Note enabled, not isEnabled.
type WireAlias struct {
	Name        string  `json:"name"`
	DomainID    string  `json:"domainId"`
	Description *string `json:"description"`
	Enabled     bool    `json:"enabled"`
}

// WireAccount is one x:Account. It carries no credential of any kind: the
// client never asks for one and a list response must never leak one.
type WireAccount struct {
	ID            string               `json:"id"`
	EmailAddress  string               `json:"emailAddress"`
	Description   *string              `json:"description"`
	DomainID      string               `json:"domainId"`
	Quotas        WireQuotas           `json:"quotas"`
	UsedDiskQuota uint64               `json:"usedDiskQuota"`
	Aliases       map[string]WireAlias `json:"aliases"`
	CreatedAt     string               `json:"createdAt"`
}

// Wire renders the mailbox. The aliases map is keyed by decimal index strings,
// which is the shape the client both sends and decodes.
func (a Account) Wire(domainName string) WireAccount {
	aliases := make(map[string]WireAlias, len(a.Aliases))
	for index, alias := range a.Aliases {
		aliases[strconv.Itoa(index)] = WireAlias{
			Name:        alias.LocalPart,
			DomainID:    alias.DomainID,
			Description: nullableString(alias.Description),
			Enabled:     alias.Enabled,
		}
	}
	return WireAccount{
		ID:            a.ID,
		EmailAddress:  a.Address(domainName),
		Description:   nullableString(a.Description),
		DomainID:      a.DomainID,
		Quotas:        WireQuotas{MaxDiskQuota: a.QuotaBytes},
		UsedDiskQuota: a.UsedBytes,
		Aliases:       aliases,
		CreatedAt:     wireTime(a.CreatedAt),
	}
}

// WireMailingList is one x:MailingList. recipients is a map address -> bool;
// the client keeps only the true entries.
type WireMailingList struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	DomainID     string          `json:"domainId"`
	Description  *string         `json:"description"`
	EmailAddress string          `json:"emailAddress"`
	Recipients   map[string]bool `json:"recipients"`
}

// Wire renders the mailing list.
func (l List) Wire(domainName string) WireMailingList {
	recipients := make(map[string]bool, len(l.Recipients))
	for _, address := range l.Recipients {
		recipients[address] = true
	}
	return WireMailingList{
		ID:           l.ID,
		Name:         l.LocalPart,
		DomainID:     l.DomainID,
		Description:  nullableString(l.Description),
		EmailAddress: l.Address(domainName),
		Recipients:   recipients,
	}
}

// WireDkimSignature is one x:DkimSignature. "@type" is the algorithm, and it
// carries the public half only.
type WireDkimSignature struct {
	ID        string `json:"id"`
	Selector  string `json:"selector"`
	Algorithm string `json:"@type"`
	Stage     string `json:"stage"`
	PublicKey string `json:"publicKey"`
	CreatedAt string `json:"createdAt"`
}

// Wire renders the signature. The private key is structurally absent.
func (k DkimKey) Wire() WireDkimSignature {
	return WireDkimSignature{
		ID:        k.ID,
		Selector:  k.Selector,
		Algorithm: k.Algorithm,
		Stage:     k.Stage,
		PublicKey: k.PublicKey,
		CreatedAt: wireTime(k.CreatedAt),
	}
}

// WireMailExchanger is one entry of x:SystemSettings.mailExchangers. A null
// hostname means the default one.
type WireMailExchanger struct {
	Hostname *string `json:"hostname"`
	Priority uint16  `json:"priority"`
}

// WireSystemSettings is the x:SystemSettings singleton.
type WireSystemSettings struct {
	ID              string                       `json:"id"`
	DefaultHostname string                       `json:"defaultHostname"`
	DefaultDomainID string                       `json:"defaultDomainId,omitempty"`
	MailExchangers  map[string]WireMailExchanger `json:"mailExchangers"`
}

// Wire renders the singleton.
func (s SystemSettings) Wire() WireSystemSettings {
	exchangers := make(map[string]WireMailExchanger, len(s.MailExchangers))
	for index, exchanger := range s.MailExchangers {
		exchangers[strconv.Itoa(index)] = WireMailExchanger{
			Hostname: nullableString(exchanger.Hostname),
			Priority: exchanger.Priority,
		}
	}
	return WireSystemSettings{
		ID:              SingletonID,
		DefaultHostname: s.DefaultHostname,
		DefaultDomainID: s.DefaultDomainID,
		MailExchangers:  exchangers,
	}
}

// WireQueueStatus is x:QueuedRecipient.status. Only "@type" is decoded.
type WireQueueStatus struct {
	Type    string `json:"@type"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// WireQueuedRecipient is one entry of x:QueuedMessage.recipients, which is
// keyed by the recipient address.
type WireQueuedRecipient struct {
	ORCPT      *string         `json:"orcpt"`
	QueueName  string          `json:"queueName"`
	RetryCount uint32          `json:"retryCount"`
	RetryDue   string          `json:"retryDue"`
	Status     WireQueueStatus `json:"status"`
}

// WireQueuedMessage is one x:QueuedMessage.
type WireQueuedMessage struct {
	ID         string                         `json:"id"`
	ReturnPath string                         `json:"returnPath"`
	CreatedAt  string                         `json:"createdAt"`
	NextRetry  *string                        `json:"nextRetry"`
	Recipients map[string]WireQueuedRecipient `json:"recipients"`
}

// Wire renders the queued message. ORCPT gets its SMTP type prefix back; the
// client strips it again.
func (m QueuedMessage) Wire() WireQueuedMessage {
	recipients := make(map[string]WireQueuedRecipient, len(m.Recipients))
	for _, recipient := range m.Recipients {
		var orcpt *string
		if recipient.ORCPT != "" {
			value := "rfc822;" + recipient.ORCPT
			orcpt = &value
		}
		recipients[recipient.Address] = WireQueuedRecipient{
			ORCPT:      orcpt,
			QueueName:  recipient.QueueName,
			RetryCount: recipient.RetryCount,
			RetryDue:   wireTime(recipient.RetryDue),
			Status: WireQueueStatus{
				Type:    recipient.Status,
				Code:    recipient.LastCode,
				Message: recipient.LastMessage,
			},
		}
	}
	return WireQueuedMessage{
		ID:         m.ID,
		ReturnPath: m.ReturnPath,
		CreatedAt:  wireTime(m.CreatedAt),
		NextRetry:  nullableTime(m.NextRetry),
		Recipients: recipients,
	}
}

// WireAccountInfo is the GET /api/account body. Only permissions and edition
// are decoded; locale is in the captured transcript and is sent for fidelity.
type WireAccountInfo struct {
	Permissions []string `json:"permissions"`
	Edition     string   `json:"edition"`
	Locale      string   `json:"locale"`
}

// AccountInfo is the GET /api/account response this server always returns.
func AccountInfo() WireAccountInfo {
	return WireAccountInfo{
		Permissions: append([]string(nil), GrantedPermissions...),
		Edition:     Edition,
		Locale:      "en-US",
	}
}

// -----------------------------------------------------------------------------
// Property projection
// -----------------------------------------------------------------------------

// Project renders one wire object as the JSON map a /get response's "list"
// carries, keeping only the requested properties. "id" is always kept, whatever
// properties says, because JMAP requires it and every caller reads it. A nil or
// empty properties list keeps everything.
//
// dnsZoneFile is the one property a caller must decide about before calling
// this: it is omitted from the struct when empty, so the expensive render is
// skipped by passing "" to Domain.Wire rather than by filtering here.
func Project(value any, properties []string) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("maild: render a wire object: %w", err)
	}
	object := map[string]any{}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("maild: render a wire object: %w", err)
	}
	if len(properties) == 0 {
		return object, nil
	}
	keep := make(map[string]struct{}, len(properties)+1)
	keep["id"] = struct{}{}
	for _, property := range properties {
		keep[property] = struct{}{}
	}
	for key := range object {
		if _, ok := keep[key]; !ok {
			delete(object, key)
		}
	}
	return object, nil
}

// ProjectAll is Project over a slice, producing the "list" of a /get response.
func ProjectAll[T any](values []T, properties []string) ([]map[string]any, error) {
	list := make([]map[string]any, 0, len(values))
	for _, value := range values {
		object, err := Project(value, properties)
		if err != nil {
			return nil, err
		}
		list = append(list, object)
	}
	return list, nil
}

// -----------------------------------------------------------------------------
// Wire helpers
// -----------------------------------------------------------------------------

// wireTime renders a timestamp the way the client parses it: RFC 3339 in UTC.
// The client tries RFC3339Nano then RFC3339 and silently yields the zero time
// for anything else, so an empty string is the honest rendering of a zero time.
func wireTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// nullableTime renders a timestamp that is JSON null when unset — the shape
// x:QueuedMessage.nextRetry has in the captured transcript.
func nullableTime(value time.Time) *string {
	if value.IsZero() {
		return nil
	}
	rendered := value.UTC().Format(time.RFC3339)
	return &rendered
}

// nullableString is JMAP's "no value": absent strings travel as null, which is
// also how the client clears a description.
func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// sortAliases orders aliases by local part so a stored account, and every wire
// rendering of it, is stable.
func sortAliases(aliases []Alias) {
	sort.Slice(aliases, func(i, j int) bool { return aliases[i].LocalPart < aliases[j].LocalPart })
}
