package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/castlemilk/dns/internal/maild"
)

// outbound is everything the sending half of this server is configured with:
// where MX records are looked up, how an outbound session is bounded, and how
// hard the queue tries before it gives up and bounces. It is a struct rather
// than a dozen arguments so that the flags, the defaults and the startup log
// all read off one thing.
type outbound struct {
	dnsServers []string
	dnsTimeout time.Duration

	relayPort       string
	relayConnect    time.Duration
	relayCommand    time.Duration
	relayData       time.Duration
	relayDisableTLS bool

	queueInterval    time.Duration
	queueBaseBackoff time.Duration
	queueMaxBackoff  time.Duration
	queueLifetime    time.Duration
	queueConcurrency int
}

// services is the one place the protocol handlers are wired together. The
// Server hands it an open store and asks for the handlers built from it; a nil
// handler in the returned set disables the listeners that need it, and the
// startup log names each one.
//
// Everything shares one root directory: the store file, the mailboxes the
// delivery path writes and POP3 reads, and the outbound spool. That is what
// makes a mailbox POP3 serves the same bytes SMTP delivered.
//
// Everything also shares one Limiter and one queue runner. Both are shared for
// the same reason: a budget that exists per listener is not a budget. Two
// limiters mean a password guesser gets one lockout allowance on SMTP and
// another on POP3; a queue runner that only a test builds means every message
// this server accepts for somewhere else is answered 250 and then spooled
// forever.
func services(pathPrefix string, out outbound) func(maild.Deps) (maild.Services, error) {
	return func(deps maild.Deps) (maild.Services, error) {
		// --- The sending half ------------------------------------------------
		//
		// It is built first because the delivery path is wired to wake it: a
		// forwarded message that is queued and then waits out a 60 s poll is a
		// forwarder that looks broken to the customer who set it up.
		queue, err := outboundQueue(deps, out)
		if err != nil {
			return maild.Services{}, err
		}

		// The delivery path is the SMTP server's Mailer: it authenticates a
		// submission, decides what a recipient address means, and writes or
		// queues an accepted message.
		deliverer, err := maild.NewDeliverer(deps.Store, maild.DeliveryConfig{
			Root:     deps.DataDir,
			Hostname: deps.Hostname,
			Logger:   deps.Logger,
			Now:      deps.Clock,
		})
		if err != nil {
			return maild.Services{}, err
		}
		mail, err := maild.NewSMTPServer(kicking{Mailer: deliverer, queue: queue}, maild.SMTPConfig{
			Hostname: deps.Hostname,
			// Without a certificate this is an inbound-only server: STARTTLS
			// is not offered and AUTH is refused on every cleartext session,
			// so submission does not work. The startup log says as much.
			TLSConfig: deps.TLS,
			// The size limit and the admission control come from the Server's
			// one Limiter. Passing the limit separately from the Limiter that
			// enforces it is how the two come to disagree, and when they do the
			// smaller is the truth and the larger is what EHLO announces.
			MaxMessageBytes: maxMessageBytes(deps.Limiter),
			Limiter:         deps.Limiter,
			Logger:          deps.Logger,
			Now:             deps.Clock,
		})
		if err != nil {
			return maild.Services{}, err
		}
		// POP3 is given no TLS configuration on purpose: the Server wraps the
		// implicit-TLS port itself, and POP3WithTLS is only read by the POP3
		// server's own accept loop, which is not the one running here. The
		// session still sees a *tls.Conn on port 995 and so still allows a
		// password there and nowhere else.
		//
		// It is given the same Limiter as SMTP, which is what makes the
		// failed-password lockout the server's rather than the port's: a peer
		// guessing on 110 and on 587 at once spends one budget, not two.
		mailbox := maild.NewPOP3Server(deps.Store, maild.NewMaildir(deps.DataDir),
			maild.POP3WithHostname(deps.Hostname),
			maild.POP3WithLogger(deps.Logger),
			maild.POP3WithLimiter(deps.Limiter),
		)

		api, err := maild.NewHandler(maild.Options{
			Store:      deps.Store,
			Token:      string(deps.AdminToken.Bytes()),
			Hostname:   deps.Hostname,
			PathPrefix: pathPrefix,
			Logger:     deps.Logger,
			Now:        deps.Clock,
			// RenderZoneFile produces x:Domain.dnsZoneFile, which is where the
			// DKIM TXT records the facade publishes actually come from — they
			// are parsed out of this text, not read from
			// DkimSignature.publicKey. A domain whose zone file comes back
			// empty binds without DKIM (CONTRACT.md §4).
			RenderZoneFile: func(ctx context.Context, domain maild.Domain) (string, error) {
				keys, err := deps.Store.ListDkimKeys(ctx, domain.ID)
				if err != nil {
					return "", err
				}
				return maild.DkimZoneRecords(keys, domain.Name), nil
			},
			// ProvisionDkim runs synchronously inside x:Domain/set create,
			// because the facade's bind step waits at most 20 s for one active
			// signature per algorithm and a key generated later leaves the
			// binding degraded until the reconciler catches up.
			ProvisionDkim: func(ctx context.Context, domain maild.Domain, algorithms []string, selectorTemplate string) error {
				_, err := maild.ProvisionDkimKeys(ctx, deps.Store, domain.ID, maild.DkimOptions{
					SelectorTemplate: selectorTemplate,
					Algorithms:       algorithms,
					Now:              deps.Clock(),
				})
				return err
			},
			ListQueue: func(ctx context.Context) ([]maild.QueuedMessage, error) {
				return deps.Store.ListQueuedMessages(ctx, maild.MaxObjectsPerCall)
			},
		})
		if err != nil {
			return maild.Services{}, err
		}

		return maild.Services{
			API: api,
			// One SMTP server serves both roles. The difference between port 25
			// and the submission ports is not a different protocol, it is
			// whether the session authenticated: an unauthenticated session may
			// only address a mailbox this server hosts, which is what keeps it
			// from being an open relay.
			SMTP:       smtpSessions(mail),
			Submission: smtpSessions(mail),
			POP3:       pop3Sessions(mailbox),
			// The queue runner is started with the listeners and drained before
			// the store closes, which is what makes an accepted message this
			// process is still holding at SIGTERM either sent or still queued,
			// and never neither.
			Workers: []maild.NamedWorker{{Name: maild.WorkerQueue, Worker: queue}},
		}, nil
	}
}

// outboundQueue builds the sending half: a resolver that finds a domain's mail
// hosts, a relay that talks to them, and the runner that walks the spool.
//
// A resolver that cannot be built is a refusal to start rather than a warning,
// because a mail server that cannot look up an MX record cannot deliver
// anything at all, and discovering that at the first message means discovering
// it from a customer.
func outboundQueue(deps maild.Deps, out outbound) (*maild.Queue, error) {
	resolver, err := maild.NewDNSResolver(maild.ResolverConfig{
		Servers: out.dnsServers,
		Timeout: out.dnsTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("outbound delivery needs a recursive resolver; name one with --dns-server: %w", err)
	}
	relay, err := maild.NewRelay(maild.RelayConfig{
		// The EHLO name has to be the name the MX record points at, because a
		// receiver checking forward-confirmed reverse DNS compares exactly
		// this against the address it sees the connection from.
		Hostname:       deps.Hostname,
		Resolver:       resolver,
		Port:           out.relayPort,
		DisableTLS:     out.relayDisableTLS,
		ConnectTimeout: out.relayConnect,
		CommandTimeout: out.relayCommand,
		DataTimeout:    out.relayData,
		Logger:         deps.Logger,
	})
	if err != nil {
		return nil, err
	}
	queue, err := maild.NewQueue(deps.Store, maild.QueueConfig{
		// The spool the delivery path writes bodies to is the spool the runner
		// reads them from. Deps.SpoolDir is the one the Server created.
		SpoolDir:      deps.SpoolDir,
		Hostname:      deps.Hostname,
		Sender:        relay,
		Interval:      out.queueInterval,
		BaseBackoff:   out.queueBaseBackoff,
		MaxBackoff:    out.queueMaxBackoff,
		Lifetime:      out.queueLifetime,
		MaxConcurrent: out.queueConcurrency,
		Logger:        deps.Logger,
		Now:           deps.Clock,
	})
	if err != nil {
		return nil, err
	}
	deps.Logger.Info("maild outbound delivery configured",
		"poll_interval", out.queueInterval.String(),
		"base_backoff", out.queueBaseBackoff.String(),
		"max_backoff", out.queueMaxBackoff.String(),
		"lifetime", out.queueLifetime.String(),
		"concurrency", out.queueConcurrency,
		"relay_port", out.relayPort,
		"starttls", !out.relayDisableTLS,
		"connect_timeout", out.relayConnect.String(),
	)
	return queue, nil
}

// maxMessageBytes is the size limit the Limiter enforces, which is the one the
// SMTP server must announce. A zero Limiter cannot happen through the Server —
// it always builds one — so the zero here only ever means "a caller assembled
// Deps by hand", and zero makes the SMTP server take its own default.
func maxMessageBytes(limiter *maild.Limiter) int64 {
	if limiter == nil {
		return 0
	}
	return limiter.Config().MaxMessageBytes
}

// kicking is the delivery path with one thing added: a message it accepted
// wakes the outbound queue runner, so a forwarded message leaves now instead of
// at the next poll. Without it every forwarder adds up to a full poll interval
// of latency to mail that is otherwise ready to go.
//
// Kick never blocks and two kicks before a pass are the same pass, so the cost
// of doing this for a purely local delivery — which queues nothing — is one
// non-blocking channel send and one pass over an empty due list.
type kicking struct {
	maild.Mailer
	queue *maild.Queue
}

// Deliver hands the message to the real delivery path and wakes the runner if
// it was accepted. A rejected message queued nothing, so there is nothing to
// wake for.
func (k kicking) Deliver(ctx context.Context, envelope *maild.Envelope) error {
	if err := k.Mailer.Deliver(ctx, envelope); err != nil {
		return err
	}
	k.queue.Kick()
	return nil
}

// smtpSessions adapts the SMTP server's per-connection entry point to the
// Server's session handler. The Server owns the accept loop, the TLS wrap on
// the implicit-TLS port and the connection's close; the SMTP server owns the
// conversation. It closes the connection itself as well, which is harmless —
// the second close is the one that reports an error nobody acts on.
func smtpSessions(server *maild.SMTPServer) maild.SessionHandler {
	return maild.SessionHandlerFunc(func(ctx context.Context, conn net.Conn) {
		releaseSessionSlot(ctx)
		server.ServeConn(conn)
	})
}

// pop3Sessions is the same adaptation for POP3, which already takes the
// session's context.
func pop3Sessions(server *maild.POP3Server) maild.SessionHandler {
	return maild.SessionHandlerFunc(func(ctx context.Context, conn net.Conn) {
		releaseSessionSlot(ctx)
		server.ServeConn(ctx, conn)
	})
}

// releaseSessionSlot hands the accept loop's session reservation to the handler
// about to run.
//
// Both line protocol servers take their own slot from the same Limiter, because
// both can also be driven from their own accept loops. Holding the Server's
// reservation as well would charge one connection twice, which would quietly
// halve --max-sessions and --max-sessions-per-ip and make the startup log's
// numbers a lie. Release is idempotent and nil-safe.
func releaseSessionSlot(ctx context.Context) {
	maild.SessionLease(ctx).Release()
}
