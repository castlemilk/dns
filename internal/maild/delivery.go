package maild

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.etcd.io/bbolt"
)

// This file is the path a message takes once the SMTP session has accepted it:
// what a recipient address means, where a local copy is written, and what is
// recorded for a recipient this server cannot deliver itself.
//
// Two rules shape everything here.
//
// A partially written message must never become visible. Every file is written
// into a sibling "tmp" directory, fsynced, and only then renamed into place —
// rename within a filesystem is atomic, so a reader (the IMAP phase, or a queue
// runner) sees the whole message or no message at all.
//
// An unknown recipient is permanent and a storage problem is temporary. Getting
// that backwards either loses mail that would have been deliverable on the next
// attempt, or makes a sender retry for five days over an address that will never
// exist. Every failure in this file therefore carries the SMTP reply it means,
// as a *DeliveryError, rather than leaving the session to guess.

// Directory layout under DeliveryConfig.Root.
//
//	<root>/mailboxes/<account id>/{tmp,new,cur}   one maildir per mailbox
//	<root>/spool/<message id>.eml                 the body of a queued message
//
// Mailboxes are keyed by account id, not by address. An id is derived
// (`deriveID`) and so is base32 over a fixed alphabet: it cannot contain a
// separator, a dot, or anything else that would let a crafted address escape the
// root. It is also stable, which matters less here only because an account's id
// is derived from its address in the first place.
const (
	MailboxesDir = "mailboxes"
	SpoolDir     = "spool"

	maildirTmp = "tmp"
	maildirNew = "new"
	maildirCur = "cur"
)

const (
	// remoteQueueName is the queueName reported for a recipient this server
	// hands on rather than stores. The facade shows it; nothing branches on it.
	remoteQueueName = "remote"

	// maxFanOut bounds how many targets one mailing list expands to. The facade
	// creates lists with a handful of recipients; the bound exists so a list
	// edited by hand cannot turn one inbound message into thousands of
	// deliveries.
	maxFanOut = 100

	// maxReceivedHops is how many Received: trace headers a message may carry,
	// counting the one this server has just added, before it is refused as a
	// loop. It is the only thing that ends a loop running through somebody
	// else's forwarder and back, because nothing in a message says "you have
	// seen me before" except the trace it accumulates.
	//
	// RFC 5321 §6.3 asks for a limit of at least 100. 25 is deliberately lower,
	// and is the number Sendmail's MaxHopCount has defaulted to for decades
	// (Postfix's hopcount_limit is 50). The reasoning is that the limit is not a
	// budget for legitimate mail — the longest honest path anyone has measured,
	// across several mailing lists and a couple of forwarders, is a dozen or so
	// hops — it is the bound on an amplifier. Each lap of a loop costs an
	// outbound session, an inbound session and roughly another 170 bytes of
	// message, so 100 is four times the traffic and four times the growth for no
	// mail that 25 would not already have carried.
	maxReceivedHops = 25

	mailDirPerm  os.FileMode = 0o700
	mailFilePerm os.FileMode = 0o600
)

// -----------------------------------------------------------------------------
// Failures that carry their own SMTP reply
// -----------------------------------------------------------------------------

// DeliveryError is a failure that already knows the reply it should produce.
//
// Message is sent verbatim to a remote client, so it must stay a plain,
// secret-free sentence. The wrapped cause is for the log only and is never put
// on the wire.
type DeliveryError struct {
	Code     int
	Enhanced string
	Message  string
	cause    error
}

func (e *DeliveryError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%d %s %s: %v", e.Code, e.Enhanced, e.Message, e.cause)
	}
	return fmt.Sprintf("%d %s %s", e.Code, e.Enhanced, e.Message)
}

func (e *DeliveryError) Unwrap() error { return e.cause }

// Temporary reports whether a sender should try again. It is the 4xx/5xx split,
// named so a caller cannot get it backwards by comparing numbers itself.
func (e *DeliveryError) Temporary() bool { return e.Code >= 400 && e.Code < 500 }

func permanentf(code int, enhanced, format string, args ...any) *DeliveryError {
	return &DeliveryError{Code: code, Enhanced: enhanced, Message: fmt.Sprintf(format, args...)}
}

func temporaryf(code int, enhanced string, cause error, format string, args ...any) *DeliveryError {
	return &DeliveryError{
		Code:     code,
		Enhanced: enhanced,
		Message:  fmt.Sprintf(format, args...),
		cause:    cause,
	}
}

// replyFor turns any error into the reply to send. An error that does not say
// what it means is treated as temporary on purpose: a bug in this server should
// make a sender retry, not make it destroy the message and bounce it.
func replyFor(err error) (code int, enhanced, message string) {
	var delivery *DeliveryError
	if errors.As(err, &delivery) {
		return delivery.Code, delivery.Enhanced, delivery.Message
	}
	return 451, "4.3.0", "Temporary local problem, please try again later"
}

// The failures a recipient lookup can produce. They are values rather than
// constructions at the call site so the codes are visible in one place and stay
// consistent between the RCPT-time check and the delivery-time one.
var (
	errBadAddress      = permanentf(501, "5.1.3", "That recipient address is not valid")
	errUnknownUser     = permanentf(550, "5.1.1", "No such user here")
	errRelayDenied     = permanentf(550, "5.7.1", "Relay access denied")
	errDomainDisabled  = temporaryf(450, "4.2.1", nil, "That domain is not accepting mail at the moment")
	errMailboxFull     = temporaryf(452, "4.2.2", nil, "The mailbox is full")
	errAuthUnavailable = temporaryf(454, "4.7.0", nil, "Cannot verify those credentials right now")
	// errAuthFailed is deliberately the same answer for an unknown mailbox and a
	// wrong password: telling the two apart turns a login form into a directory.
	errAuthFailed = permanentf(535, "5.7.8", "Authentication credentials invalid")
	// errTooManyHops ends a loop. It is permanent — 5.4.6 is "routing loop
	// detected" — because a loop is not a condition that clears: retrying is
	// another lap. Refusing at the end of DATA is what makes it visible, too:
	// the server that offered the message is told 554 and reports that to
	// whoever wrote it, rather than this server accepting responsibility for
	// mail it will only send in a circle.
	errTooManyHops = permanentf(554, "5.4.6", "Too many hops, this message is looping")
)

// -----------------------------------------------------------------------------
// The vocabulary the SMTP session and the delivery path share
// -----------------------------------------------------------------------------

// RecipientKind is what a recipient address turned out to be.
type RecipientKind uint8

const (
	// RecipientUnknown is the zero value and never resolves.
	RecipientUnknown RecipientKind = iota
	// RecipientMailbox is stored locally in this server's maildir.
	RecipientMailbox
	// RecipientList is a mailing list: it fans out to its targets.
	RecipientList
	// RecipientRelay is an address on another server, only ever reachable from
	// an authenticated submission.
	RecipientRelay
)

func (k RecipientKind) String() string {
	switch k {
	case RecipientMailbox:
		return "mailbox"
	case RecipientList:
		return "list"
	case RecipientRelay:
		return "relay"
	default:
		return "unknown"
	}
}

// Recipient is one accepted envelope recipient and what this server will do
// with it.
//
// Address is what the sender wrote (normalised), which is not always where the
// mail lands: a sub-address (user+tag@domain), an alias and a catch-all all
// resolve to a mailbox whose own address is different. Keeping both is what lets
// the stored copy carry a truthful Delivered-To and a fan-out carry a truthful
// ORCPT.
type Recipient struct {
	Address   string
	Kind      RecipientKind
	AccountID string
	ListID    string
}

// Identity is an authenticated submission credential.
type Identity struct {
	AccountID string
	// Addresses are every address this credential may put in MAIL FROM: the
	// mailbox itself and its aliases, normalised.
	Addresses []string
}

// MayUse reports whether this credential owns an envelope sender. A sub-address
// of an owned address counts, because a submission client that sends as
// user+tag@domain is doing something ordinary.
func (i Identity) MayUse(address string) bool {
	local, domain, ok := SplitAddress(address)
	if !ok {
		return false
	}
	bare, _, _ := strings.Cut(local, "+")
	for _, allowed := range i.Addresses {
		if allowed == local+"@"+domain || allowed == bare+"@"+domain {
			return true
		}
	}
	return false
}

// Envelope is one accepted message, handed from the session to the delivery
// path. Data is the DATA content exactly as it will be stored: dot-unstuffed,
// CRLF line endings, without the trailing "." line.
type Envelope struct {
	ID         string
	ReturnPath string
	Recipients []Recipient
	// Received is the trace header the session built, already CRLF-terminated.
	// It is prepended to every stored and spooled copy.
	Received   string
	Data       []byte
	ReceivedAt time.Time
	// Identity is non-nil when the message arrived on an authenticated
	// submission, which is the only way a relay recipient can appear.
	Identity   *Identity
	RemoteAddr string
	HELO       string
	TLS        bool
}

// Mailer is everything the SMTP server needs from the delivery path. It is an
// interface only so a test can drive the session into a failure that is awkward
// to arrange for real; *Deliverer is the implementation.
type Mailer interface {
	// Authenticate verifies a submission credential. It must answer the same
	// error for an unknown mailbox and a wrong password.
	Authenticate(ctx context.Context, username, password string) (Identity, error)
	// Resolve decides what a recipient address means. authenticated allows a
	// relay recipient; without it an address this server does not host is
	// refused rather than relayed.
	Resolve(ctx context.Context, address string, authenticated bool) (Recipient, error)
	// Deliver writes an accepted message. It is called once, after the final
	// dot, and its error carries the reply to send.
	Deliver(ctx context.Context, envelope *Envelope) error
}

// -----------------------------------------------------------------------------
// Deliverer
// -----------------------------------------------------------------------------

// DeliveryConfig configures the delivery path.
type DeliveryConfig struct {
	// Root is the directory holding mailboxes and the spool. It is created if
	// it does not exist.
	Root string
	// Hostname names this server in generated file names. It is cosmetic.
	Hostname string
	Logger   *slog.Logger
	Now      func() time.Time
}

// Deliverer implements Mailer against a *Store and a directory.
type Deliverer struct {
	store    *Store
	root     string
	hostname string
	logger   *slog.Logger
	now      func() time.Time
	seq      atomic.Uint64
}

// NewDeliverer prepares the delivery path, creating the root layout.
func NewDeliverer(store *Store, config DeliveryConfig) (*Deliverer, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: delivery needs a store", ErrInvalid)
	}
	if strings.TrimSpace(config.Root) == "" {
		return nil, fmt.Errorf("%w: delivery needs a root directory", ErrInvalid)
	}
	deliverer := &Deliverer{
		store:    store,
		root:     filepath.Clean(config.Root),
		hostname: sanitizeFileToken(config.Hostname),
		logger:   config.Logger,
		now:      config.Now,
	}
	if deliverer.hostname == "" {
		deliverer.hostname = "maild"
	}
	if deliverer.logger == nil {
		deliverer.logger = slog.Default()
	}
	if deliverer.now == nil {
		deliverer.now = time.Now
	}
	for _, dir := range []string{deliverer.root, deliverer.mailboxesRoot(), deliverer.spoolRoot()} {
		if err := os.MkdirAll(dir, mailDirPerm); err != nil {
			return nil, fmt.Errorf("maild: create the delivery directory: %w", err)
		}
	}
	return deliverer, nil
}

func (d *Deliverer) mailboxesRoot() string { return filepath.Join(d.root, MailboxesDir) }

func (d *Deliverer) spoolRoot() string { return filepath.Join(d.root, SpoolDir) }

// MailboxDir is the maildir of one account. The IMAP phase reads from here.
func (d *Deliverer) MailboxDir(accountID string) string {
	return filepath.Join(d.mailboxesRoot(), accountID)
}

// SpoolPath resolves a QueuedMessage.MessagePath, which is stored relative to
// the spool directory so the root can move.
func (d *Deliverer) SpoolPath(relative string) string {
	return filepath.Join(d.spoolRoot(), filepath.FromSlash(relative))
}

// -----------------------------------------------------------------------------
// Authentication
// -----------------------------------------------------------------------------

// decoyHash is verified against when no mailbox matches, so a failed login costs
// the same time whether or not the address exists. Deriving it once keeps that
// cost off the successful path.
var decoyHash = sync.OnceValue(func() PasswordHash {
	hash, err := HashPassword("maild decoy credential")
	if err != nil {
		return PasswordHash{}
	}
	return hash
})

// Authenticate verifies a submission credential. username is a full address;
// an alias of the mailbox is accepted, because an alias is an address the
// customer was given.
//
// Neither argument is ever logged or returned, and the failure is the same
// value for every cause.
func (d *Deliverer) Authenticate(ctx context.Context, username, password string) (Identity, error) {
	local, domainName, ok := SplitAddress(username)
	if !ok || password == "" {
		verifyDecoy(password)
		return Identity{}, errAuthFailed
	}
	domain, err := d.store.FindDomain(ctx, domainName)
	if errors.Is(err, ErrNotFound) {
		verifyDecoy(password)
		return Identity{}, errAuthFailed
	}
	if err != nil {
		return Identity{}, temporaryf(454, "4.7.0", err, "%s", errAuthUnavailable.Message)
	}
	account, err := d.store.FindAccount(ctx, domain.ID, local)
	if errors.Is(err, ErrNotFound) {
		account, err = d.store.AccountByAlias(ctx, domain.ID, local)
	}
	switch {
	case errors.Is(err, ErrNotFound):
		verifyDecoy(password)
		return Identity{}, errAuthFailed
	case err != nil:
		return Identity{}, temporaryf(454, "4.7.0", err, "%s", errAuthUnavailable.Message)
	}
	if !account.Password.Verify(password) {
		return Identity{}, errAuthFailed
	}
	if !domain.Enabled {
		return Identity{}, permanentf(535, "5.7.8", "That mailbox is not currently allowed to send")
	}
	identity := Identity{
		AccountID: account.ID,
		Addresses: []string{account.Address(domain.Name)},
	}
	for _, alias := range account.Aliases {
		if !alias.Enabled || alias.DomainID != domain.ID {
			continue
		}
		identity.Addresses = append(identity.Addresses, alias.LocalPart+"@"+domain.Name)
	}
	return identity, nil
}

// verifyDecoy burns the same work a real verification would, so the answer for
// an address that does not exist takes as long as the answer for one that does.
func verifyDecoy(password string) {
	hash := decoyHash()
	if hash.IsZero() {
		return
	}
	if hash.Verify(password) {
		// Unreachable: the decoy secret is a constant in this file. The branch
		// exists so the compiler cannot discard the verification.
		panic("maild: the decoy credential verified")
	}
}

// -----------------------------------------------------------------------------
// Recipient resolution
// -----------------------------------------------------------------------------

// Resolve decides what a recipient address means.
//
// The order is deliberate: an exact mailbox beats an alias, an alias beats a
// list, and only when none of them match does a sub-address or the catch-all get
// a turn. A customer who creates info@ as a mailbox and also has a catch-all
// expects the mailbox to win.
func (d *Deliverer) Resolve(ctx context.Context, address string, authenticated bool) (Recipient, error) {
	local, domainName, ok := SplitAddress(address)
	if !ok || len(local) > 64 || len(domainName) > 255 {
		return Recipient{}, errBadAddress
	}
	normalized := local + "@" + domainName

	domain, err := d.store.FindDomain(ctx, domainName)
	switch {
	case errors.Is(err, ErrNotFound):
		// Not a domain this server hosts. Only an authenticated submission may
		// send through here; everyone else is refused, which is the difference
		// between a mail server and an open relay.
		//
		// The local part keeps the case the sender wrote. Every address this
		// server owns is lowercased, but RFC 5321 §2.4 makes a remote local part
		// case-sensitive and the remote server is the only one entitled to
		// decide it does not care.
		if authenticated {
			rawLocal, _, _ := strings.Cut(strings.TrimSpace(address), "@")
			return Recipient{Address: rawLocal + "@" + domainName, Kind: RecipientRelay}, nil
		}
		return Recipient{}, errRelayDenied
	case err != nil:
		return Recipient{}, temporaryf(451, "4.3.0", err, "Cannot check that recipient right now")
	}
	if !domain.Enabled {
		return Recipient{}, errDomainDisabled
	}

	candidates := []string{local}
	if domain.SubAddressing {
		if bare, _, found := strings.Cut(local, "+"); found && bare != "" && bare != local {
			candidates = append(candidates, bare)
		}
	}
	if domain.CatchAllLocalPart != "" {
		candidates = append(candidates, NormalizeLocalPart(domain.CatchAllLocalPart))
	}
	for _, candidate := range candidates {
		recipient, found, err := d.resolveLocal(ctx, domain, candidate)
		if err != nil {
			return Recipient{}, err
		}
		if found {
			// The address stays what the sender wrote, so Delivered-To and
			// ORCPT name the address the customer published rather than the
			// mailbox that happens to hold it.
			recipient.Address = normalized
			return recipient, nil
		}
	}
	return Recipient{}, errUnknownUser
}

// resolveLocal looks one local part up in one domain: mailbox, then alias, then
// mailing list.
func (d *Deliverer) resolveLocal(ctx context.Context, domain Domain, local string) (Recipient, bool, error) {
	account, err := d.store.FindAccount(ctx, domain.ID, local)
	if err == nil {
		if full, over := mailboxIsFull(account); over {
			return Recipient{}, false, full
		}
		return Recipient{Kind: RecipientMailbox, AccountID: account.ID}, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Recipient{}, false, temporaryf(451, "4.3.0", err, "Cannot check that recipient right now")
	}

	account, err = d.store.AccountByAlias(ctx, domain.ID, local)
	if err == nil {
		if full, over := mailboxIsFull(account); over {
			return Recipient{}, false, full
		}
		return Recipient{Kind: RecipientMailbox, AccountID: account.ID}, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Recipient{}, false, temporaryf(451, "4.3.0", err, "Cannot check that recipient right now")
	}

	list, err := d.store.FindList(ctx, domain.ID, local)
	if err == nil {
		return Recipient{Kind: RecipientList, ListID: list.ID}, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Recipient{}, false, temporaryf(451, "4.3.0", err, "Cannot check that recipient right now")
	}
	return Recipient{}, false, nil
}

// mailboxIsFull reports an already-full mailbox at RCPT time, before a sender
// has spent bandwidth on a body that cannot be stored. It is temporary: the
// customer can delete mail and the next attempt succeeds.
func mailboxIsFull(account Account) (*DeliveryError, bool) {
	if account.QuotaBytes > 0 && account.UsedBytes >= account.QuotaBytes {
		return errMailboxFull, true
	}
	return nil, false
}

// -----------------------------------------------------------------------------
// Delivery
// -----------------------------------------------------------------------------

// Deliver writes an accepted message.
//
// Failure is all or nothing: if any recipient fails, the whole message fails and
// the sender retries. With several recipients that can duplicate the copies that
// did land, which is the lesser of the two evils — the alternative is accepting
// responsibility for a message and then dropping part of it, and a copy this
// function never wrote is a copy the queue runner has no entry to bounce.
//
// A message that has been round too many times is refused here rather than
// delivered: a Received: ceiling is the only thing that ends a loop running out
// through somebody else's forwarder and back in again, because nothing else in a
// message says "you have seen me before".
func (d *Deliverer) Deliver(ctx context.Context, envelope *Envelope) error {
	if envelope == nil || len(envelope.Recipients) == 0 {
		return permanentf(554, "5.5.0", "No valid recipients")
	}
	now := d.now().UTC()
	if !envelope.ReceivedAt.IsZero() {
		now = envelope.ReceivedAt.UTC()
	}

	// Every copy this server writes carries a trace header, and this is the last
	// point at which one can be added: after here the message is on disk. A
	// caller that built its own (the SMTP session does) keeps it; anything else
	// gets one here, because a hop that leaves no trace is a hop no downstream
	// server — including this one on the next lap — can count.
	if strings.TrimSpace(envelope.Received) == "" {
		envelope.Received = d.receivedHeader(envelope, now)
	}
	if hops := countReceived([]byte(envelope.Received)) + countReceived(envelope.Data); hops > maxReceivedHops {
		return errTooManyHops
	}

	var queued []QueuedRecipient
	for _, recipient := range envelope.Recipients {
		if err := ctx.Err(); err != nil {
			return temporaryf(451, "4.3.0", err, "The server is shutting down, please try again")
		}
		switch recipient.Kind {
		case RecipientMailbox:
			if err := d.storeLocal(ctx, envelope, recipient, now); err != nil {
				return err
			}
		case RecipientRelay:
			queued = append(queued, QueuedRecipient{
				Address:   recipient.Address,
				Status:    QueueStatusScheduled,
				QueueName: remoteQueueName,
				RetryDue:  now,
			})
		case RecipientList:
			expanded, err := d.expandList(ctx, envelope, recipient, now)
			if err != nil {
				return err
			}
			queued = append(queued, expanded...)
		default:
			return permanentf(554, "5.5.0", "That recipient could not be resolved")
		}
	}
	if len(queued) == 0 {
		return nil
	}
	return d.enqueue(ctx, envelope, queued, now)
}

// expandList fans a mailing list out. A target this server hosts as a mailbox is
// delivered immediately; anything else is queued for the runner.
//
// The one thing that is never queued is a target on a domain this server hosts
// that did not resolve. That used to be logged at debug and appended to the
// outbound queue anyway, which is a relay of the customer's own address: the
// queue delivers to whatever the MX for that domain says, the facade publishes
// this server as the MX for every domain it hosts, so a typo in a forwarder
// became an outbound session to ourselves for an address we had just decided
// does not exist. It is refused instead, with the reply the lookup produced —
// 550 for an address that is not there, a temporary code for a disabled domain
// or a full mailbox — so the server that offered the message reports it to
// whoever sent it.
//
// A target that resolves to another forwarder is still handed to the queue. It
// is a hop through this server's own MX and back, which is a legitimate if
// wasteful way for a customer to chain two forwarders, and it terminates:
// maxReceivedHops counts the trace headers such a message accumulates and
// Deliver refuses it permanently past the limit.
func (d *Deliverer) expandList(ctx context.Context, envelope *Envelope, recipient Recipient, now time.Time) ([]QueuedRecipient, error) {
	list, err := d.store.GetList(ctx, recipient.ListID)
	if errors.Is(err, ErrNotFound) {
		return nil, errUnknownUser
	}
	if err != nil {
		return nil, temporaryf(451, "4.3.0", err, "Cannot expand that address right now")
	}
	if len(list.Recipients) > maxFanOut {
		return nil, permanentf(550, "5.5.3", "That address has too many recipients")
	}

	var queued []QueuedRecipient
	for _, target := range list.Recipients {
		// Resolve as an authenticated sender would, which is the only way a
		// target on a domain this server does not host comes back as a relay
		// rather than as "relay access denied".
		resolved, resolveErr := d.Resolve(ctx, target, true)
		switch {
		case resolveErr != nil:
			// The address is malformed, or it is on a hosted domain and does not
			// exist there. Either way it carries its own reply — 501, 550, or a
			// temporary code for a disabled domain or a full mailbox — and that
			// reply is the answer to the whole message.
			return nil, resolveErr
		case resolved.Kind == RecipientMailbox:
			if err := d.storeLocal(ctx, envelope, resolved, now); err != nil {
				return nil, err
			}
		default:
			queued = append(queued, QueuedRecipient{
				Address: strings.TrimSpace(target),
				// ORCPT carries the customer's own address. Without it a fan-out
				// is attributable to nobody: the queue would show only the
				// external target and the facade could not tell whose zone it
				// belongs to.
				ORCPT:     recipient.Address,
				Status:    QueueStatusScheduled,
				QueueName: remoteQueueName,
				RetryDue:  now,
			})
		}
	}
	return queued, nil
}

// countReceived counts the Received: trace headers in a message's header block.
//
// Only the header block counts: a "Received:" line inside a body, which any
// forwarded or quoted message has, is not a hop this message took, and counting
// it would let a sender make their own mail undeliverable by quoting enough of
// somebody else's. Continuation lines are skipped for the same reason — a folded
// header field is one field however many lines it occupies.
func countReceived(message []byte) int {
	const field = "received:"
	count, rest := 0, message
	for len(rest) > 0 {
		line := rest
		if index := bytes.IndexByte(rest, '\n'); index >= 0 {
			line, rest = rest[:index], rest[index+1:]
		} else {
			rest = nil
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		switch {
		case len(line) == 0:
			// The empty line ends the headers; everything after it is body.
			return count
		case line[0] == ' ' || line[0] == '\t':
			continue
		case len(line) >= len(field) && bytes.EqualFold(line[:len(field)], []byte(field)):
			count++
		}
	}
	return count
}

// receivedHeader is the trace header this server adds for a caller that did not
// build one. Every value that came off the wire is sanitised: a header this
// server writes from client-supplied text is an injection point, and a bare CR
// or LF in a HELO name would end the field and start another.
func (d *Deliverer) receivedHeader(envelope *Envelope, now time.Time) string {
	from := sanitizeHeaderValue(envelope.HELO)
	if from == "" {
		from = "unknown"
	}
	line := "Received: from " + from
	if remote := sanitizeHeaderValue(envelope.RemoteAddr); remote != "" {
		line += " (" + remote + ")"
	}
	line += "\r\n\tby " + d.hostname + " with SMTP"
	if id := sanitizeHeaderValue(envelope.ID); id != "" {
		line += " id " + id
	}
	return line + ";\r\n\t" + now.Format(time.RFC1123Z) + "\r\n"
}

// storeLocal writes one copy into one mailbox.
func (d *Deliverer) storeLocal(ctx context.Context, envelope *Envelope, recipient Recipient, now time.Time) error {
	account, err := d.store.GetAccount(ctx, recipient.AccountID)
	if errors.Is(err, ErrNotFound) {
		// The mailbox existed at RCPT time and is gone now. That is a race, not
		// a bad address, so the sender should retry rather than be told the
		// address never existed.
		return temporaryf(450, "4.2.1", err, "That mailbox is not available right now")
	}
	if err != nil {
		return temporaryf(451, "4.3.0", err, "Cannot store that message right now")
	}
	body := composeLocal(envelope, recipient)
	if account.QuotaBytes > 0 && account.UsedBytes+uint64(len(body)) > account.QuotaBytes {
		return errMailboxFull
	}

	directory := d.MailboxDir(account.ID)
	for _, sub := range []string{maildirTmp, maildirNew, maildirCur} {
		if err := os.MkdirAll(filepath.Join(directory, sub), mailDirPerm); err != nil {
			return temporaryf(451, "4.3.0", err, "Cannot store that message right now")
		}
	}
	name := d.uniqueName(now, len(body))
	if err := writeThenRename(
		filepath.Join(directory, maildirTmp, name),
		filepath.Join(directory, maildirNew, name),
		body,
	); err != nil {
		return temporaryf(451, "4.3.0", err, "Cannot store that message right now")
	}
	if err := d.store.AddAccountUsage(ctx, account.ID, int64(len(body))); err != nil {
		// The message is on disk and the customer will see it; a usage figure
		// that is briefly low is not worth making the sender send it twice.
		d.logger.Warn("maild: could not record mailbox usage", "account", account.ID, "error", err)
	}
	return nil
}

// composeLocal builds the stored copy: the envelope facts a message body cannot
// carry, then the message.
func composeLocal(envelope *Envelope, recipient Recipient) []byte {
	var builder strings.Builder
	builder.Grow(len(envelope.Data) + 256)
	returnPath := envelope.ReturnPath
	if returnPath == "" {
		returnPath = "<>"
	} else {
		returnPath = "<" + returnPath + ">"
	}
	builder.WriteString("Return-Path: " + returnPath + "\r\n")
	if recipient.Address != "" {
		builder.WriteString("Delivered-To: " + recipient.Address + "\r\n")
	}
	builder.WriteString(envelope.Received)
	builder.Write(envelope.Data)
	return []byte(builder.String())
}

// enqueue spools the body once and records the queue entry the
// x:QueuedMessage JMAP methods read.
func (d *Deliverer) enqueue(ctx context.Context, envelope *Envelope, recipients []QueuedRecipient, now time.Time) error {
	id, err := randomID("q")
	if err != nil {
		return temporaryf(451, "4.3.0", err, "Cannot queue that message right now")
	}
	body := make([]byte, 0, len(envelope.Received)+len(envelope.Data))
	body = append(body, envelope.Received...)
	body = append(body, envelope.Data...)
	// Signing here, before the body is spooled, is what makes the signature
	// cover exactly the bytes that leave: the queue reads this file and hands it
	// to the relay unchanged, so nothing can rewrite a signed header afterwards.
	body = d.signOutbound(ctx, envelope, recipients, body)

	name := id + ".eml"
	if err := writeThenRename(
		filepath.Join(d.spoolRoot(), maildirTmp, name),
		filepath.Join(d.spoolRoot(), name),
		body,
	); err != nil {
		return temporaryf(451, "4.3.0", err, "Cannot queue that message right now")
	}
	message := QueuedMessage{
		V:           DocVersion,
		ID:          id,
		ReturnPath:  envelope.ReturnPath,
		Recipients:  recipients,
		MessagePath: name,
		SizeBytes:   int64(len(body)),
		CreatedAt:   now,
		NextRetry:   now,
	}
	if _, err := d.store.PutQueuedMessage(ctx, message); err != nil {
		if removeErr := os.Remove(filepath.Join(d.spoolRoot(), name)); removeErr != nil {
			d.logger.Debug("maild: could not remove an orphaned spool file", "error", removeErr)
		}
		return temporaryf(451, "4.3.0", err, "Cannot queue that message right now")
	}
	// The entry is durable, so the runner may look at it. Kick never blocks and
	// never starts anything, so the SMTP session that is waiting on this call to
	// answer the final dot pays a channel send for it and nothing else.
	wakeRunner(d.store)
	return nil
}

// -----------------------------------------------------------------------------
// DKIM
// -----------------------------------------------------------------------------

// signOutbound adds a DKIM-Signature for the hosted domain on whose behalf this
// message is leaving.
//
// It has to happen. The facade publishes a DKIM TXT record into the customer's
// zone the moment a domain is bound (CONTRACT.md §4.2), which tells every
// receiver in the world to expect a signature on mail from that domain. Sending
// unsigned mail under a published key is worse than publishing nothing: a
// receiver that finds no signature where the DNS said there would be one has
// been given a reason to distrust the message.
//
// Every failure here is deliberately non-fatal. Unsigned mail is delivered and
// usually accepted; mail that was not sent because its signature could not be
// computed is not delivered at all, and a domain with no active key — a
// brand-new binding whose keys are still being generated — must not have its
// mail held hostage to one.
func (d *Deliverer) signOutbound(ctx context.Context, envelope *Envelope, recipients []QueuedRecipient, body []byte) []byte {
	domain, found := d.signingDomain(ctx, envelope, recipients)
	if !found {
		return body
	}
	keys, err := d.store.ListDkimKeys(ctx, domain.ID)
	if err != nil {
		d.logger.Warn("maild: could not read the DKIM keys for an outbound message",
			"domain", domain.ID, "error", err)
		return body
	}
	// SignMessage signs once per active key and returns the message unchanged
	// when none is active, which is the "send unsigned" case rather than a
	// failure. It signs DefaultSignedHeaders with relaxed/relaxed
	// canonicalisation; nothing here chooses either, so the choice cannot drift
	// away from what dkim_test.go proves.
	signed, err := SignMessage(body, domain.Name, keys, SignOptions{Now: d.now().UTC()})
	if err != nil {
		// The error names the domain and, inside dkim.go, at most a selector.
		// Neither is a secret; the private key never reaches an error string.
		d.logger.Warn("maild: could not DKIM-sign an outbound message",
			"domain", domain.ID, "error", err)
		return body
	}
	return signed
}

// signingDomain picks whose key signs.
//
// Two shapes of outbound mail reach here and they name the customer in different
// places. An authenticated submission has the customer's own address in MAIL
// FROM, so the return path names the domain. A forwarder fan-out has the
// original sender's address in MAIL FROM — the envelope sender is preserved so
// the bounce reaches whoever wrote the message — and the customer's address only
// in the ORCPT this server recorded. Trying the return path first and the ORCPT
// second covers both without either having to know which it is.
//
// An address on a domain this server does not host signs nothing: there is no
// key, and claiming a d= this server cannot back is a signature that fails.
func (d *Deliverer) signingDomain(ctx context.Context, envelope *Envelope, recipients []QueuedRecipient) (Domain, bool) {
	candidates := make([]string, 0, 1+len(recipients))
	if _, name, ok := SplitAddress(envelope.ReturnPath); ok {
		candidates = append(candidates, name)
	}
	for _, recipient := range recipients {
		if _, name, ok := SplitAddress(recipient.ORCPT); ok {
			candidates = append(candidates, name)
		}
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, name := range candidates {
		if _, repeated := seen[name]; repeated {
			continue
		}
		seen[name] = struct{}{}
		domain, err := d.store.FindDomain(ctx, name)
		switch {
		case err == nil:
			return domain, true
		case errors.Is(err, ErrNotFound):
			continue
		default:
			d.logger.Warn("maild: could not look up the signing domain", "error", err)
			return Domain{}, false
		}
	}
	return Domain{}, false
}

// uniqueName builds a maildir file name. The ",S=" suffix is the size hint every
// maildir reader understands, so a mailbox listing does not have to stat.
func (d *Deliverer) uniqueName(now time.Time, size int) string {
	return fmt.Sprintf("%d.M%dP%dQ%d.%s,S=%d",
		now.Unix(), now.Nanosecond()/1000, os.Getpid(), d.seq.Add(1), d.hostname, size)
}

// sanitizeFileToken keeps a hostname usable inside a maildir file name: the
// maildir specification forbids ":" and "/" there, and a name that carried a
// separator would be a path, not a name.
func sanitizeFileToken(value string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	return strings.Trim(builder.String(), "-.")
}

// writeThenRename is the only way this file puts bytes on disk. The message is
// written and fsynced under a temporary name and then renamed into place, so a
// reader never sees a partial message, and the containing directory is fsynced
// so the rename survives a crash.
func writeThenRename(temporaryPath, finalPath string, body []byte) (err error) {
	if mkErr := os.MkdirAll(filepath.Dir(temporaryPath), mailDirPerm); mkErr != nil {
		return fmt.Errorf("maild: create the temporary directory: %w", mkErr)
	}
	if mkErr := os.MkdirAll(filepath.Dir(finalPath), mailDirPerm); mkErr != nil {
		return fmt.Errorf("maild: create the delivery directory: %w", mkErr)
	}
	file, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mailFilePerm)
	if err != nil {
		return fmt.Errorf("maild: create the temporary message: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if _, writeErr := file.Write(body); writeErr != nil {
		return errors.Join(fmt.Errorf("maild: write the message: %w", writeErr), file.Close())
	}
	if syncErr := file.Sync(); syncErr != nil {
		return errors.Join(fmt.Errorf("maild: flush the message: %w", syncErr), file.Close())
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("maild: close the message: %w", closeErr)
	}
	if renameErr := os.Rename(temporaryPath, finalPath); renameErr != nil {
		return fmt.Errorf("maild: publish the message: %w", renameErr)
	}
	if syncErr := syncDir(filepath.Dir(finalPath)); syncErr != nil {
		return syncErr
	}
	return nil
}

// syncDir makes a rename durable. A failure to open the directory for reading is
// not fatal on every platform, so only a failed sync is reported.
func syncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return nil
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return errors.Join(fmt.Errorf("maild: flush the delivery directory: %w", syncErr), closeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("maild: close the delivery directory: %w", closeErr)
	}
	return nil
}

// randomID mints an opaque identifier with the same alphabet the derived ids
// use, so every id in this system is safe as a file name and as a JMAP id.
func randomID(prefix string) (string, error) {
	buffer := make([]byte, 10)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("maild: generate an identifier: %w", err)
	}
	return prefix + idEncoding.EncodeToString(buffer), nil
}

// -----------------------------------------------------------------------------
// The queue bucket
//
// store.go creates the "queue" bucket and leaves it empty; CONTRACT.md §7
// reserves it for this phase, so its accessors live here rather than there.
// -----------------------------------------------------------------------------

// PutQueuedMessage writes a queue entry, creating or replacing it.
func (s *Store) PutQueuedMessage(ctx context.Context, message QueuedMessage) (QueuedMessage, error) {
	if strings.TrimSpace(message.ID) == "" {
		return QueuedMessage{}, fmt.Errorf("%w: a queued message needs an id", ErrInvalid)
	}
	if message.V == 0 {
		message.V = DocVersion
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = s.now().UTC()
	}
	message.CreatedAt = message.CreatedAt.UTC()
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		return putJSON(tx, queueBucket, []byte(message.ID), message)
	})
	if err != nil {
		return QueuedMessage{}, err
	}
	return message, nil
}

// GetQueuedMessage reads one queue entry.
func (s *Store) GetQueuedMessage(ctx context.Context, id string) (QueuedMessage, error) {
	var message QueuedMessage
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return getJSON(tx, queueBucket, []byte(id), &message)
	})
	return message, err
}

// ListQueuedMessages returns the queue oldest first, at most limit entries;
// limit <= 0 returns everything.
//
// The bound matters to the facade rather than to this server: x:QueuedMessage/query
// asks for 500, and a full page makes it report "not available" instead of a
// count, so returning fewer than the limit when fewer exist is what makes the
// number showable.
func (s *Store) ListQueuedMessages(ctx context.Context, limit int) ([]QueuedMessage, error) {
	var messages []QueuedMessage
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return eachJSON(tx, queueBucket, func(decode func(any) error) error {
			var message QueuedMessage
			if err := decode(&message); err != nil {
				return err
			}
			messages = append(messages, message)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(messages, func(i, j int) bool {
		if !messages[i].CreatedAt.Equal(messages[j].CreatedAt) {
			return messages[i].CreatedAt.Before(messages[j].CreatedAt)
		}
		return messages[i].ID < messages[j].ID
	})
	if limit > 0 && len(messages) > limit {
		messages = messages[:limit]
	}
	return messages, nil
}

// DeleteQueuedMessage removes a queue entry. Deleting one that is not there is
// success.
func (s *Store) DeleteQueuedMessage(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(queueBucket).Delete([]byte(id))
	})
}

// AddAccountUsage moves a mailbox's reported disk usage by delta and returns the
// new figure. Delivery adds; the phase that expunges a message must subtract, or
// usedDiskQuota drifts up and the console shows a mailbox fuller than it is.
func (s *Store) AddAccountUsage(ctx context.Context, id string, delta int64) error {
	if delta == 0 {
		return nil
	}
	return s.update(ctx, func(tx *bbolt.Tx) error {
		var account Account
		if err := getJSON(tx, accountBucket, []byte(id), &account); err != nil {
			return err
		}
		switch {
		case delta > 0:
			account.UsedBytes += uint64(delta)
		case uint64(-delta) >= account.UsedBytes:
			account.UsedBytes = 0
		default:
			account.UsedBytes -= uint64(-delta)
		}
		return putJSON(tx, accountBucket, []byte(id), account)
	})
}
