// Package maild is a small, self-contained mail server: SMTP in and out, IMAP,
// DKIM signing, and a management API. It exists so this platform can offer real
// email without operating a Stalwart deployment.
//
// # The design decision everything else follows from
//
// internal/mail is a finished facade over a mail server. Its Engine interface is
// expressed entirely in the internal/mail/stalwart object types, and
// stalwart.Client speaks a small JMAP subset over POST /jmap plus one
// GET /api/account.
//
// Rather than write a second Engine implementation and touch that facade, maild
// speaks the same JMAP subset stalwart.Client already calls. The entire existing
// facade — mailboxes, forwarders, DKIM records, autoconfig, the reconciler —
// then works against maild unchanged, selected purely by pointing MAIL_API_URL
// at this server. Nothing in internal/mail changes, and nothing in this package
// imports it outside of tests.
//
// The consequence is that the client is frozen: every method name, argument
// shape and response shape below is a hard requirement on this server, never a
// negotiation. CONTRACT.md in this directory is the exhaustive, exact
// specification, derived by reading the client. Read it before writing a handler
// — a wrong method name or arg shape does not fail a test, it silently degrades
// a customer's mail binding.
//
// # The zero-dependency rule
//
// This package adds no Go module dependency. Standard library only, plus what
// go.mod already has (notably go.etcd.io/bbolt for storage and
// github.com/miekg/dns where DNS is needed). SMTP and IMAP are line protocols;
// DKIM signing is crypto/rsa, crypto/ed25519 and crypto/sha256; the password KDF
// is PBKDF2-HMAC-SHA256 written out in store.go, because golang.org/x/crypto is
// not available here. If a dependency looks unavoidable, stop and report it
// rather than adding one.
//
// # Secrets
//
// Mailbox passwords, DKIM private keys and the admin token must never be logged,
// never appear in an error string, and never be returned by a list call. Two
// types carry that rule structurally rather than by convention: Secret redacts
// itself under every fmt verb, and PasswordHash holds a derived digest that no
// wire object ever includes. New secret-bearing state belongs inside one of
// them.
//
// # Layout
//
//	CONTRACT.md  the complete wire contract; the specification of record
//	doc.go       this file
//	model.go     the stored types, the JMAP wire objects, and the mapping between
//	store.go     the bbolt store, deterministic ids, and the password KDF
//
// Identifiers are deterministic: an object's id is a hash of its natural key
// (domain name; domain id plus local part; domain id plus selector). That is
// what lets a delivery path resolve an address to an account with one keyed read
// and no secondary index. Because a recreated domain would therefore reuse its
// old id, the store refuses to delete a domain while any account, list or DKIM
// key still references it — which is also the objectIsLinked refusal the facade
// expects.
package maild
