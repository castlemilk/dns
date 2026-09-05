// Package mail is the facade over a Stalwart mail server: it binds a zone to a
// mail domain, publishes exactly the DNS records §3.3 lists, creates mailboxes
// and forwarders, and reports only what the engine can actually answer.
//
// Nothing here fabricates a number. Community Stalwart exposes no per-domain
// delivery history, so no 24-hour delivered/bounced figure is produced anywhere
// — the queue is what is honestly knowable and it is what is shown.
package mail

import (
	"context"

	"github.com/castlemilk/dns/internal/mail/stalwart"
)

// The Stalwart object model, re-exported so the rest of the facade has one
// import for the whole mail vocabulary. The types are declared in
// internal/mail/stalwart because this package constructs a *stalwart.Client
// inside New; see that package's doc comment.
type (
	AccountInfo     = stalwart.AccountInfo
	SystemSettings  = stalwart.SystemSettings
	MailExchanger   = stalwart.MailExchanger
	Domain          = stalwart.Domain
	DomainInput     = stalwart.DomainInput
	DomainPatch     = stalwart.DomainPatch
	DkimSignature   = stalwart.DkimSignature
	Alias           = stalwart.Alias
	Account         = stalwart.Account
	AccountInput    = stalwart.AccountInput
	AccountPatch    = stalwart.AccountPatch
	MailingList     = stalwart.MailingList
	ListInput       = stalwart.ListInput
	QueuedMessage   = stalwart.QueuedMessage
	QueuedRecipient = stalwart.QueuedRecipient
)

// Engine is everything the facade needs from a mail server. Both
// *stalwart.Client and *fakemail.Engine satisfy it structurally; the assertions
// live here, where the interface does, because neither implementation may
// import this package.
type Engine interface {
	// Account reports the edition and the permissions the credential holds.
	Account(ctx context.Context) (AccountInfo, error)
	// SystemSettings reports the server's own hostname. The facade refuses to
	// bind, and stops writing, when it differs from MAIL_HOSTNAME.
	SystemSettings(ctx context.Context) (SystemSettings, error)

	FindDomain(ctx context.Context, name string) (Domain, bool, error)
	ListDomains(ctx context.Context) ([]Domain, error)
	CreateDomain(ctx context.Context, in DomainInput) (Domain, error)
	// GetDomain includes the rendered dnsZoneFile.
	GetDomain(ctx context.Context, id string) (Domain, error)
	UpdateDomain(ctx context.Context, id string, patch DomainPatch) error
	DestroyDomain(ctx context.Context, id string) error

	ListDkim(ctx context.Context, domainID string) ([]DkimSignature, error)
	DestroyDkim(ctx context.Context, id string) error

	ListAccounts(ctx context.Context, domainID string) ([]Account, error)
	CreateAccount(ctx context.Context, in AccountInput) (Account, error)
	UpdateAccount(ctx context.Context, id string, patch AccountPatch) error
	DestroyAccount(ctx context.Context, id string) error

	// ListLists returns one domain's mailing lists, or every list when
	// domainID is empty. The server has no domainId filter for this object, so
	// the implementation queries unfiltered and matches the domain itself.
	ListLists(ctx context.Context, domainID string) ([]MailingList, error)
	CreateList(ctx context.Context, in ListInput) (MailingList, error)
	DestroyList(ctx context.Context, id string) error

	// QueryQueue returns the queued messages attributable to zoneName. The
	// second result is false when the server's page came back full, which means
	// attribution is incomplete and no count may be shown.
	QueryQueue(ctx context.Context, zoneName string, limit int) ([]QueuedMessage, bool, error)
}

var _ Engine = (*stalwart.Client)(nil)

// RequiredPermissions is the exact `sys*` set the facade uses. The prober
// compares it against the names GET /api/account reports and lists the
// difference; an absent name is reported, never assumed present.
var RequiredPermissions = []string{
	"sysDomainGet", "sysDomainCreate", "sysDomainUpdate", "sysDomainDestroy", "sysDomainQuery",
	"sysAccountGet", "sysAccountCreate", "sysAccountUpdate", "sysAccountDestroy", "sysAccountQuery",
	"sysAccountPasswordUpdate",
	"sysMailingListGet", "sysMailingListCreate", "sysMailingListUpdate", "sysMailingListDestroy", "sysMailingListQuery",
	"sysDkimSignatureGet", "sysDkimSignatureQuery", "sysDkimSignatureDestroy",
	"sysQueuedMessageGet", "sysQueuedMessageQuery",
	"sysSystemSettingsGet",
}
