// Command deephost-mail is the Deep Hosting mail server: SMTP in and out, POP3,
// DKIM signing, and the management API the control plane drives.
//
// It exists so the platform can offer real email without operating a Stalwart
// deployment. It is not a second mail engine as far as the control plane is
// concerned: it speaks the same JMAP subset internal/mail/stalwart already
// calls, so pointing MAIL_API_URL at this process selects it and nothing in
// internal/mail changes. internal/maild/CONTRACT.md is the specification of
// record.
//
// The three values that must agree with the control plane:
//
//	MAIL_API_URL    the control plane's URL for --api-addr on this process
//	MAIL_API_TOKEN  must equal --admin-token here
//	MAIL_HOSTNAME   must equal --hostname here
//
// The hostname is the one that fails invisibly. The facade compares the
// hostname this server reports against MAIL_HOSTNAME before it will bind,
// publish or reconcile anything, and a mismatch is reported per operation as
// "the mail server calls itself X" rather than at startup — so this process
// refuses to start without a hostname rather than let that happen.
//
// Exit codes are 0 for a clean stop, 1 for a failure and 2 for a mistake in
// what was asked for.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// SIGTERM as well as SIGINT: a container runtime sends SIGTERM, and the
	// point of handling it is that mail in flight is delivered or queued rather
	// than dropped mid-transaction.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}
