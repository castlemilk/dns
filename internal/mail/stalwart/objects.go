// Package stalwart is the JMAP client for a Stalwart mail server (v0.16), and
// the Go shape of the Stalwart objects the facade uses.
//
// Why the value types live here rather than in internal/mail: internal/mail
// constructs a *Client (and a fakemail.Engine) inside its frozen New, so it
// imports this package. A type declared in internal/mail and used in this
// package's method signatures would therefore close an import cycle. The types
// are Stalwart's own vocabulary anyway — `Dkim1RsaSha256`, `dnsZoneFile`,
// `usedDiskQuota` — so this is where they belong. internal/mail re-exports them
// as aliases and declares the Engine interface both this package and
// internal/mail/fakemail satisfy.
//
// Everything here is v0.16-only: the REST management API and the TOML config
// were removed in that release, so every management action is a JMAP method
// `x:<Object>/get|set|query` under capability `urn:stalwart:jmap`, posted to
// POST /jmap. The only non-JMAP endpoint used is GET /api/account, which
// reports the edition and the permissions the credential actually holds.
package stalwart

import "time"

// AccountInfo is GET /api/account: what edition answered and which permissions
// the API key actually holds. The facade compares Permissions against the set
// it needs and reports the difference instead of assuming.
type AccountInfo struct {
	Edition     string
	Permissions []string
}

// MailExchanger is one entry of x:SystemSettings.mailExchangers. It is read for
// display only: the MX record the facade publishes comes from MAIL_HOSTNAME and
// MAIL_MX_PRIORITY, never from the server, so a changed or compromised mail
// server cannot redirect customer mail through the reconciler.
type MailExchanger struct {
	Hostname string
	Priority uint16
}

// SystemSettings is the x:SystemSettings singleton.
type SystemSettings struct {
	DefaultHostname string
	MailExchangers  []MailExchanger
}

// Domain is one x:Domain. DNSZoneFile is the server-rendered BIND text and is
// only populated by GetDomain.
type Domain struct {
	ID          string
	Name        string
	Description string
	Enabled     bool
	DNSZoneFile string
	CreatedAt   time.Time
}

// DomainInput creates a domain with automatic DKIM management and manual DNS
// and certificate management: this deployment publishes the records itself.
type DomainInput struct {
	Name             string
	Description      string
	ReportAddressURI string
}

// DomainPatch is a partial update. A nil field is left alone.
type DomainPatch struct {
	Description      *string
	ReportAddressURI *string
	Enabled          *bool
}

// DKIM signature stages, as reported by x:DkimSignature.stage.
const (
	DkimStagePending  = "pending"
	DkimStageActive   = "active"
	DkimStageRetiring = "retiring"
	DkimStageRetired  = "retired"
)

// DKIM algorithms requested for every domain. RSA is not optional: Gmail and
// Microsoft verify RSA only, so publishing Ed25519 alone would leave outbound
// mail unsigned for the receivers that matter.
const (
	DkimAlgorithmEd25519 = "Dkim1Ed25519Sha256"
	DkimAlgorithmRSA     = "Dkim1RsaSha256"
)

// RequestedDkimAlgorithms is the set every domain is created with, and the set
// the bind step waits for before it publishes.
var RequestedDkimAlgorithms = []string{DkimAlgorithmEd25519, DkimAlgorithmRSA}

// DkimSignature is one x:DkimSignature.
type DkimSignature struct {
	ID        string
	Selector  string
	Algorithm string
	Stage     string
	PublicKey string
	CreatedAt time.Time
}

// Active reports whether this signature is one the domain currently signs with,
// and therefore one whose public key must be published.
func (s DkimSignature) Active() bool { return s.Stage == DkimStageActive }

// Alias is one entry of x:Account.aliases: a local part in the same domain that
// delivers into this account.
type Alias struct {
	Name        string
	DomainID    string
	Description string
	Enabled     bool
}

// Account is one x:Account of type User — a mailbox.
type Account struct {
	ID           string
	EmailAddress string
	LocalPart    string
	DomainID     string
	Description  string
	QuotaBytes   uint64
	UsedBytes    uint64
	Aliases      []Alias
	CreatedAt    time.Time
}

// AccountInput creates a mailbox. Secret is the generated credential; it is
// sent once and is never stored, logged or returned by this package.
type AccountInput struct {
	LocalPart   string
	DomainID    string
	Description string
	Secret      string
	QuotaBytes  uint64
}

// AccountPatch is a partial update. A nil field is left alone.
type AccountPatch struct {
	Description *string
	QuotaBytes  *uint64
	Aliases     *[]Alias
	Secret      *string
}

// MailingList is one x:MailingList: the object behind a forwarder with an
// external target or several targets.
type MailingList struct {
	ID           string
	Name         string
	DomainID     string
	Description  string
	EmailAddress string
	Recipients   []string
}

// ListDescription marks the mailing lists this product created, so a list an
// operator made by hand in the Stalwart UI is listed but is recognisable.
const ListDescription = "forwarder"

// ListInput creates a mailing list.
type ListInput struct {
	LocalPart   string
	DomainID    string
	Description string
	Recipients  []string
}

// Queue statuses, from x:QueuedRecipient.status.@type.
const (
	QueueStatusScheduled        = "Scheduled"
	QueueStatusCompleted        = "Completed"
	QueueStatusTemporaryFailure = "TemporaryFailure"
	QueueStatusPermanentFailure = "PermanentFailure"
)

// QueuedRecipient is one recipient of a message still in the outbound queue.
// ORCPT carries the original recipient of a forwarder fan-out: the external
// target appears as Address, and the customer's own address only here.
type QueuedRecipient struct {
	Address    string
	ORCPT      string
	Status     string
	QueueName  string
	RetryCount uint32
	RetryDue   time.Time
}

// QueuedMessage is one x:QueuedMessage.
type QueuedMessage struct {
	ID         string
	ReturnPath string
	CreatedAt  time.Time
	NextRetry  time.Time
	Recipients []QueuedRecipient
}
