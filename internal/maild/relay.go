package maild

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/smtp"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// This file is the outbound half of the server: finding the hosts that accept a
// domain's mail, and handing one message to one of them.
//
// Everything here answers one question — should the sender try again? A reply
// the remote server actually sent decides it: 2xx means it took responsibility,
// 5xx means it never will, 4xx means later. Anything that is not a reply — a
// refused connection, a timeout, a DNS server that did not answer — is
// temporary, because a server that never spoke has not refused the message.
// Getting that backwards either bounces mail that would have gone through, or
// retries for five days over an address that will never exist.
//
// The two seams a test injects at are Resolver (so no MX lookup leaves the
// process) and RelayConfig.DialContext (so every session lands on a loopback
// listener). Deadlines deliberately use the real clock and not an injected one:
// a socket deadline computed from a fake clock is a socket with no deadline.
//
// Between the MX answer and the socket sits the one check that treats that
// answer as hostile: mailHostDialer refuses any destination that is not on the
// public internet, so an MX record naming 127.0.0.1, 10.0.0.0/8 or
// 169.254.169.254 cannot turn this server into a client of its own
// infrastructure.

// -----------------------------------------------------------------------------
// Finding the hosts that accept a domain's mail
// -----------------------------------------------------------------------------

// ErrNoMailHost is the permanent DNS answer: this domain accepts no mail at all.
// It is the one lookup outcome that must not be retried — the name does not
// exist, or it publishes RFC 7505's null MX, and five days of retries will find
// the same thing. Every other lookup failure is temporary.
var ErrNoMailHost = errors.New("maild: the recipient domain accepts no mail")

// Resolver finds the hosts that accept mail for a domain, best first.
//
// It is an interface so that a test never touches the network: the queue's
// tests resolve every domain to their own loopback listener.
type Resolver interface {
	// LookupMailHosts returns the host names to try in preference order.
	// A domain that accepts no mail is ErrNoMailHost, which is permanent;
	// every other error is temporary.
	LookupMailHosts(ctx context.Context, domain string) ([]string, error)
}

// ResolverFunc adapts a plain function to Resolver.
type ResolverFunc func(ctx context.Context, domain string) ([]string, error)

// LookupMailHosts calls f.
func (f ResolverFunc) LookupMailHosts(ctx context.Context, domain string) ([]string, error) {
	return f(ctx, domain)
}

const (
	// DefaultResolverTimeout bounds one query to one recursive resolver. It is
	// short because there is a retry schedule behind it: a slow resolver should
	// defer the message, not hold an outbound worker.
	DefaultResolverTimeout = 5 * time.Second
	// DefaultMaxMailHosts bounds how many of a domain's MX hosts one attempt
	// dials. A domain may publish dozens; trying them all in one pass turns a
	// single message into a long outbound stall.
	DefaultMaxMailHosts = 5

	// resolvConfPath is where the recursive resolvers come from when none are
	// configured.
	resolvConfPath = "/etc/resolv.conf"
	// dnsUDPBufferSize is the EDNS(0) advertised buffer. 1232 is the DNS Flag
	// Day 2020 figure: it fits the smallest plausible path MTU, so an answer is
	// truncated into TCP rather than fragmented and lost.
	dnsUDPBufferSize = 1232
	// nullMXTarget is the target RFC 7505 reserves to mean "no mail here".
	nullMXTarget = "."
)

// ResolverConfig configures a DNSResolver.
type ResolverConfig struct {
	// Servers are recursive resolvers, as host or host:port. Empty reads
	// /etc/resolv.conf, which is what a container gets from its cluster DNS.
	Servers []string
	// Timeout bounds one query. Zero takes DefaultResolverTimeout.
	Timeout time.Duration
	// MaxHosts bounds the hosts returned. Zero takes DefaultMaxMailHosts.
	MaxHosts int
}

// DNSResolver answers from recursive resolvers over UDP, retrying over TCP when
// an answer is truncated.
type DNSResolver struct {
	servers  []string
	maxHosts int
	timeout  time.Duration
	udp      *dns.Client
	tcp      *dns.Client
}

// NewDNSResolver prepares a resolver. It fails rather than starting with no
// recursive resolver at all: a mail server that cannot look up an MX cannot
// deliver anything, and finding that out at the first message is worse than
// finding it out at startup.
func NewDNSResolver(config ResolverConfig) (*DNSResolver, error) {
	servers := make([]string, 0, len(config.Servers))
	for _, server := range config.Servers {
		if address, ok := resolverAddress(server); ok {
			servers = append(servers, address)
		}
	}
	if len(servers) == 0 {
		discovered, err := systemResolvers()
		if err != nil {
			return nil, err
		}
		servers = discovered
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = DefaultResolverTimeout
	}
	maxHosts := config.MaxHosts
	if maxHosts <= 0 {
		maxHosts = DefaultMaxMailHosts
	}
	return &DNSResolver{
		servers:  servers,
		maxHosts: maxHosts,
		timeout:  timeout,
		udp:      &dns.Client{Net: "udp", Timeout: timeout},
		tcp:      &dns.Client{Net: "tcp", Timeout: timeout},
	}, nil
}

// systemResolvers reads /etc/resolv.conf.
func systemResolvers() ([]string, error) {
	config, err := dns.ClientConfigFromFile(resolvConfPath)
	if err != nil {
		return nil, fmt.Errorf("maild: read the system resolver configuration: %w", err)
	}
	port := strings.TrimSpace(config.Port)
	if port == "" {
		port = "53"
	}
	servers := make([]string, 0, len(config.Servers))
	for _, server := range config.Servers {
		if address, ok := resolverAddress(net.JoinHostPort(server, port)); ok {
			servers = append(servers, address)
		}
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("%w: no recursive resolver is configured", ErrInvalid)
	}
	return servers, nil
}

// resolverAddress puts the default port on a bare address. A bare IPv6 literal
// has colons of its own, so the port is decided by whether the string already
// splits, not by counting colons.
func resolverAddress(raw string) (string, bool) {
	server := strings.TrimSpace(raw)
	if server == "" {
		return "", false
	}
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server, true
	}
	return net.JoinHostPort(server, "53"), true
}

// LookupMailHosts implements Resolver against the configured recursive
// resolvers.
func (r *DNSResolver) LookupMailHosts(ctx context.Context, domain string) ([]string, error) {
	// The domain is validated here rather than trusted, because it reaches a
	// wire format: an unchecked label would be encoded into a DNS question.
	name, err := ParseDomainName(domain)
	if err != nil {
		return nil, fmt.Errorf("%w: the recipient domain is not a valid name", ErrNoMailHost)
	}
	fqdn := dns.Fqdn(name)

	answer, err := r.query(ctx, fqdn, dns.TypeMX)
	if err != nil {
		return nil, err
	}
	if err := rcodeError(answer.Rcode, "MX"); err != nil {
		return nil, err
	}
	hosts, nullMX := mailHostsFromMX(answer, r.maxHosts)
	switch {
	case nullMX:
		return nil, ErrNoMailHost
	case len(hosts) > 0:
		return hosts, nil
	}
	// RFC 5321 §5.1: a domain with no MX is its own mail host, but only if it
	// has an address. Without that check every typo'd domain would be dialled.
	return r.implicitMailHost(ctx, name, fqdn)
}

// implicitMailHost applies the implicit-MX rule.
func (r *DNSResolver) implicitMailHost(ctx context.Context, name, fqdn string) ([]string, error) {
	for _, question := range []uint16{dns.TypeA, dns.TypeAAAA} {
		answer, err := r.query(ctx, fqdn, question)
		if err != nil {
			return nil, err
		}
		if err := rcodeError(answer.Rcode, "address"); err != nil {
			return nil, err
		}
		for _, record := range answer.Answer {
			switch record.(type) {
			case *dns.A, *dns.AAAA:
				return []string{name}, nil
			}
		}
	}
	return nil, ErrNoMailHost
}

// rcodeError maps a response code onto the retry decision. NXDOMAIN is the only
// permanent one; a SERVFAIL is a resolver having a bad day, not an answer.
func rcodeError(rcode int, question string) error {
	switch rcode {
	case dns.RcodeSuccess:
		return nil
	case dns.RcodeNameError:
		return ErrNoMailHost
	default:
		name, ok := dns.RcodeToString[rcode]
		if !ok {
			name = strconv.Itoa(rcode)
		}
		return fmt.Errorf("maild: the %s lookup was answered %s", question, name)
	}
}

// mailHostsFromMX orders the MX targets and drops the ones that name nothing.
//
// Equal preferences are ordered by name rather than shuffled: this is one small
// server, spreading load over a peer's equal-preference hosts is not its job,
// and a deterministic order is one less thing that can differ between a test and
// production.
func mailHostsFromMX(answer *dns.Msg, limit int) (hosts []string, nullMX bool) {
	records := make([]*dns.MX, 0, len(answer.Answer))
	for _, record := range answer.Answer {
		if mx, ok := record.(*dns.MX); ok {
			records = append(records, mx)
		}
	}
	if len(records) == 1 && records[0].Preference == 0 &&
		strings.TrimSpace(records[0].Mx) == nullMXTarget {
		return nil, true
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Preference != records[j].Preference {
			return records[i].Preference < records[j].Preference
		}
		return records[i].Mx < records[j].Mx
	})
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(record.Mx), "."))
		if host == "" {
			continue
		}
		if _, duplicate := seen[host]; duplicate {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
		if limit > 0 && len(hosts) >= limit {
			break
		}
	}
	return hosts, false
}

// query asks each configured resolver in turn and returns the first answer.
func (r *DNSResolver) query(ctx context.Context, name string, question uint16) (*dns.Msg, error) {
	request := new(dns.Msg)
	request.SetQuestion(name, question)
	request.RecursionDesired = true
	request.SetEdns0(dnsUDPBufferSize, false)

	var lastErr error
	for _, server := range r.servers {
		queryCtx, cancel := context.WithTimeout(ctx, r.timeout)
		answer, _, err := r.udp.ExchangeContext(queryCtx, request, server)
		if err == nil && answer.Truncated {
			answer, _, err = r.tcp.ExchangeContext(queryCtx, request, server)
		}
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		return answer, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no recursive resolver answered")
	}
	return nil, fmt.Errorf("maild: the DNS query failed: %w", lastErr)
}

// -----------------------------------------------------------------------------
// Handing a message to another server
// -----------------------------------------------------------------------------

const (
	// DefaultRelayPort is the port every MX is dialled on.
	DefaultRelayPort = "25"
	// DefaultRelayConnectTimeout bounds one TCP connect. A host that is not
	// answering should cost one attempt, not one worker.
	DefaultRelayConnectTimeout = 30 * time.Second
	// DefaultRelayCommandTimeout and DefaultRelayDataTimeout are the RFC 5321
	// §4.5.3.2 client timeouts, rounded to the two that matter.
	DefaultRelayCommandTimeout = 5 * time.Minute
	DefaultRelayDataTimeout    = 10 * time.Minute
)

// RelayReport is what one attempt concluded for one recipient. A nil Err means
// the receiving server took responsibility for the message.
type RelayReport struct {
	Address string
	Err     error
}

// Sender hands one message to the servers that accept one domain's mail. It is
// the seam between the queue runner and the network; *Relay is the
// implementation.
//
// Send always returns one report per element of to, in the same order, so a
// caller can index the two together without checking the length.
type Sender interface {
	Send(ctx context.Context, domain, from string, to []string, body []byte) []RelayReport
}

// RelayConfig configures outbound delivery. Every zero value has a default.
type RelayConfig struct {
	// Hostname is the name given in EHLO. It should be the name the MX record
	// points at, because a receiver that checks forward-confirmed reverse DNS
	// compares exactly this.
	Hostname string
	// Resolver finds the hosts that accept a domain's mail. Required.
	Resolver Resolver
	// DialContext opens one outbound connection. Nil uses a plain net.Dialer,
	// and that is the production shape: the relay then resolves each mail host
	// itself and refuses every address that is not on the public internet.
	//
	// A transport supplied here replaces both the socket and the resolution, so
	// a name is passed to it unresolved and reaching whatever it likes is the
	// point — it is how this package's tests land every session on a loopback
	// listener. An address literal is still refused. Nothing in
	// cmd/deephost-mail sets this field.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
	// Port is dialled on every mail host. Empty is DefaultRelayPort.
	Port string
	// TLSConfig overrides the opportunistic STARTTLS configuration. Nil builds
	// the default described on tlsConfigFor.
	TLSConfig *tls.Config
	// DisableTLS never offers STARTTLS. It exists for a test and for an
	// operator who has to reach a peer whose TLS is broken in a new way.
	DisableTLS bool
	// allowInternalMailHosts lets the relay dial addresses that are not
	// routable on the public internet.
	//
	// It is unexported on purpose. A test in this package sets it because this
	// package's fake receiving servers listen on 127.0.0.1; no other package —
	// cmd/deephost-mail included — can name the field at all, so the production
	// path cannot turn the check off however its configuration is written.
	allowInternalMailHosts bool

	ConnectTimeout time.Duration
	CommandTimeout time.Duration
	DataTimeout    time.Duration

	// MaxHosts bounds the mail hosts dialled per attempt whatever the resolver
	// returned. Zero takes DefaultMaxMailHosts.
	MaxHosts int
	Logger   *slog.Logger
}

// Relay delivers a message to another server.
type Relay struct {
	resolver   Resolver
	dial       func(ctx context.Context, network, address string) (net.Conn, error)
	hostname   string
	port       string
	tlsConfig  *tls.Config
	disableTLS bool

	connectTimeout time.Duration
	commandTimeout time.Duration
	dataTimeout    time.Duration

	maxHosts int
	logger   *slog.Logger
}

// NewRelay prepares an outbound client.
func NewRelay(config RelayConfig) (*Relay, error) {
	if config.Resolver == nil {
		return nil, fmt.Errorf("%w: the relay needs a resolver", ErrInvalid)
	}
	relay := &Relay{
		resolver:       config.Resolver,
		hostname:       sanitizeHeaderValue(config.Hostname),
		port:           strings.TrimSpace(config.Port),
		tlsConfig:      config.TLSConfig,
		disableTLS:     config.DisableTLS,
		connectTimeout: config.ConnectTimeout,
		commandTimeout: config.CommandTimeout,
		dataTimeout:    config.DataTimeout,
		maxHosts:       config.MaxHosts,
		logger:         config.Logger,
	}
	if relay.logger == nil {
		relay.logger = slog.Default()
	}
	// Every outbound connection goes through the guard, whichever transport is
	// underneath it. Without a caller-supplied transport the guard also does the
	// resolution, which is the only way it can know what will really be dialled.
	guard := &mailHostDialer{
		dial:          config.DialContext,
		allowInternal: config.allowInternalMailHosts,
		logger:        relay.logger,
	}
	if guard.dial == nil {
		dialer := &net.Dialer{}
		guard.dial = dialer.DialContext
		guard.lookup = lookupMailHostAddrs
	}
	relay.dial = guard.DialContext
	if relay.hostname == "" {
		relay.hostname = "localhost"
	}
	if relay.port == "" {
		relay.port = DefaultRelayPort
	}
	if relay.connectTimeout <= 0 {
		relay.connectTimeout = DefaultRelayConnectTimeout
	}
	if relay.commandTimeout <= 0 {
		relay.commandTimeout = DefaultRelayCommandTimeout
	}
	if relay.dataTimeout <= 0 {
		relay.dataTimeout = DefaultRelayDataTimeout
	}
	if relay.maxHosts <= 0 {
		relay.maxHosts = DefaultMaxMailHosts
	}
	return relay, nil
}

// -----------------------------------------------------------------------------
// Refusing a mail host that is not on the public internet
// -----------------------------------------------------------------------------

// errUnroutableMailHost is what the relay's dialer answers for a mail host
// whose only addresses are ones no message should be carried to.
//
// It names the host and never the address that host resolved to. The address is
// the one piece of internal topology this refusal learns, and the error it is
// put in travels towards a bounce written for a sender nobody vouched for.
var errUnroutableMailHost = errors.New("maild: the mail host has no address on the public internet")

// unroutablePrefixes are the ranges a mail host must not be reached at. They
// are the IANA special-purpose registries, kept as one list because the reason
// each entry is here is the same: an address in it either means something only
// inside a network, or means nothing at all, so a message carried to it is
// either a request against internal infrastructure or a message thrown away.
//
// Every IPv6 range that embeds an IPv4 address is refused whole rather than
// unwrapped, because 2002:7f00:1:: and 64:ff9b::7f00:1 are both 127.0.0.1 with
// a hat on, and no real MX is published in either.
var unroutablePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),      // RFC 1918 private
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC 6598 carrier-grade NAT
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local, and every cloud metadata service
	netip.MustParsePrefix("172.16.0.0/12"),   // RFC 1918 private
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("192.168.0.0/16"),  // RFC 1918 private
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and the 255.255.255.255 broadcast
	netip.MustParsePrefix("::/96"),           // unspecified, loopback, IPv4-compatible
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001::/32"),       // Teredo
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4
	netip.MustParsePrefix("fc00::/7"),        // unique local
	netip.MustParsePrefix("fe80::/10"),       // link-local
	netip.MustParsePrefix("ff00::/8"),        // multicast
}

// routableMailHost reports whether one address is somewhere mail may be carried
// to.
func routableMailHost(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	// Unmap is why this is net/netip and not net.IP. ::ffff:127.0.0.1 is
	// loopback in a costume, and it is the classic way past a check that looks
	// at the two families separately.
	addr = addr.Unmap().WithZone("")
	for _, prefix := range unroutablePrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// lookupMailHostAddrs resolves a mail host through the system resolver.
//
// This is a second resolution, after the MX lookup, and it is deliberate: the
// MX answer is a name, and the thing that has to be checked is the address the
// kernel would open a socket to.
func lookupMailHostAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// mailHostDialer opens the relay's outbound connections and refuses the ones
// that would leave the public internet.
type mailHostDialer struct {
	// dial opens the socket once a destination has been allowed.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
	// lookup resolves a mail host to the addresses that will be dialled. It is
	// nil when dial is a transport the caller supplied, because such a
	// transport does its own resolution and reaching whatever it likes is the
	// point of supplying it; an address literal is checked even then.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// allowInternal turns the check off for RelayConfig.allowInternalMailHosts.
	allowInternal bool
	logger        *slog.Logger
}

// DialContext resolves the destination, refuses it unless some address of it is
// on the public internet, and dials that address.
func (d *mailHostDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d.allowInternal {
		return d.dial(ctx, network, address)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
		if !routableMailHost(literal) {
			return nil, d.refuse(host, []netip.Addr{literal})
		}
		return d.dial(ctx, network, address)
	}
	if d.lookup == nil {
		return d.dial(ctx, network, address)
	}
	resolved, err := d.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	allowed := make([]netip.Addr, 0, len(resolved))
	for _, candidate := range resolved {
		if routableMailHost(candidate) {
			allowed = append(allowed, candidate.Unmap().WithZone(""))
		}
	}
	if len(allowed) == 0 {
		return nil, d.refuse(host, resolved)
	}
	// The allowed address is dialled, not the name. Resolving a second time
	// inside the dialer would let a DNS answer that changes between the two
	// hand the socket to the address this just refused.
	var lastErr error
	for _, allowedAddr := range allowed {
		conn, dialErr := d.dial(ctx, network, net.JoinHostPort(allowedAddr.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, lastErr
}

// refuse logs the refusal where the addresses are useful and returns an error
// that carries only the host name, because that error goes on to a bounce.
func (d *mailHostDialer) refuse(host string, resolved []netip.Addr) error {
	d.logger.Warn("maild: refused to relay to a mail host that is not on the public internet",
		"host", host, "addresses", fmt.Sprint(resolved))
	return fmt.Errorf("%w: %q", errUnroutableMailHost, host)
}

// Send delivers one message to one domain's recipients.
//
// The mail hosts are tried best first. A host that fails temporarily makes the
// next one worth trying; a host that answers 5xx before any recipient was named
// ends the attempt, because that answer is about the message and the next host
// would give the same one.
func (r *Relay) Send(ctx context.Context, domain, from string, to []string, body []byte) []RelayReport {
	reports := make([]RelayReport, len(to))
	for i, address := range to {
		reports[i] = RelayReport{Address: address}
	}
	if len(to) == 0 {
		return reports
	}

	hosts, err := r.resolver.LookupMailHosts(ctx, domain)
	if err != nil {
		return fillReports(reports, resolverFailure(err))
	}
	if len(hosts) == 0 {
		return fillReports(reports, &DeliveryError{
			Code: 550, Enhanced: "5.1.2",
			Message: "The recipient's domain has no mail server",
		})
	}
	if len(hosts) > r.maxHosts {
		hosts = hosts[:r.maxHosts]
	}

	var last *DeliveryError
	for _, host := range hosts {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fillReports(reports, temporaryf(451, "4.3.0", ctxErr,
				"The server is shutting down, this will be retried"))
		}
		results, failure := r.attempt(ctx, host, from, to, body)
		if results != nil {
			for i := range reports {
				reports[i].Err = results[i]
			}
			return reports
		}
		// A refused destination outranks nothing. It is about one host, not
		// about the message, so it never overwrites another host's temporary
		// failure: a domain whose second MX points inside must still be
		// retried at the first, and a message that might yet deliver is never
		// bounced.
		if last == nil || !last.Temporary() || !errors.Is(failure, errUnroutableMailHost) {
			last = failure
		}
		// A permanent answer is about the message, so no other host would
		// answer differently — except that refusal, which leaves the next MX
		// worth trying. When every host is refused the failure is still
		// permanent, so the message bounces instead of being retried for five
		// days at an address that will never be dialled.
		if !failure.Temporary() && !errors.Is(failure, errUnroutableMailHost) {
			break
		}
	}
	return fillReports(reports, last)
}

// fillReports gives every recipient that has no answer of its own the same one.
func fillReports(reports []RelayReport, failure *DeliveryError) []RelayReport {
	for i := range reports {
		if reports[i].Err == nil {
			reports[i].Err = failure
		}
	}
	return reports
}

// attempt runs one session against one host, retrying once in cleartext when
// STARTTLS itself failed.
//
// That retry is what "opportunistic" means: TLS we could not negotiate must
// leave us no worse off than a peer that never offered it. A peer that offers a
// broken STARTTLS and no fallback would otherwise hold the message until it
// expired, which trades a confidentiality nicety for lost mail.
func (r *Relay) attempt(ctx context.Context, host, from string, to []string, body []byte) ([]error, *DeliveryError) {
	results, failure, tlsFailed := r.session(ctx, host, from, to, body, !r.disableTLS)
	if !tlsFailed {
		return results, failure
	}
	r.logger.Warn("maild: STARTTLS failed, retrying in the clear",
		"host", host, "error", failure.Message)
	results, failure, _ = r.session(ctx, host, from, to, body, false)
	return results, failure
}

// session runs one SMTP conversation.
//
// results is non-nil exactly when the conversation reached RCPT, in which case
// it holds one answer per recipient and no other host should be tried: the
// message has been offered. When results is nil the session failed before any
// recipient was named and failure says whether another host is worth a try.
func (r *Relay) session(ctx context.Context, host, from string, to []string, body []byte, useTLS bool) (results []error, failure *DeliveryError, tlsFailed bool) {
	dialCtx, cancelDial := context.WithTimeout(ctx, r.connectTimeout)
	conn, err := r.dial(dialCtx, "tcp", net.JoinHostPort(host, r.port))
	cancelDial()
	if err != nil {
		if errors.Is(err, errUnroutableMailHost) {
			// Permanent: an MX that points inside is a configuration this
			// server will refuse identically for the next five days. The
			// message says nothing about what was resolved — it is read back
			// to the sender in a bounce.
			return nil, &DeliveryError{
				Code: 550, Enhanced: "5.1.2",
				Message: "The recipient's mail server is not reachable on the public internet",
				cause:   err,
			}, false
		}
		return nil, temporaryf(421, "4.4.1", err,
			"Could not connect to the recipient's mail server"), false
	}
	// net/smtp is deadline-driven and takes no context, so closing the
	// connection is how a cancelled context reaches a session in progress.
	stopCancel := context.AfterFunc(ctx, func() { r.close(conn) })
	defer stopCancel()
	defer r.close(conn)

	if deadlineErr := r.extend(conn, r.commandTimeout); deadlineErr != nil {
		return nil, temporaryf(421, "4.4.2", deadlineErr, "Could not bound the outbound session"), false
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return nil, remoteFailure(err, 421, "4.4.2",
			"The recipient's mail server did not greet us"), false
	}
	defer r.quit(client)

	if err := client.Hello(r.hostname); err != nil {
		return nil, remoteFailure(err, 421, "4.4.2",
			"The recipient's mail server refused our greeting"), false
	}
	if useTLS {
		if offered, _ := client.Extension("STARTTLS"); offered {
			if err := client.StartTLS(r.tlsConfigFor(host)); err != nil {
				return nil, temporaryf(421, "4.7.0", err,
					"The recipient's mail server could not negotiate TLS"), true
			}
		}
	}

	if deadlineErr := r.extend(conn, r.commandTimeout); deadlineErr != nil {
		return nil, temporaryf(421, "4.4.2", deadlineErr, "Could not bound the outbound session"), false
	}
	if err := client.Mail(from); err != nil {
		return nil, remoteFailure(err, 451, "4.3.0",
			"The recipient's mail server refused the sender"), false
	}

	results = make([]error, len(to))
	accepted := 0
	for i, address := range to {
		if deadlineErr := r.extend(conn, r.commandTimeout); deadlineErr != nil {
			results[i] = temporaryf(421, "4.4.2", deadlineErr, "Could not bound the outbound session")
			continue
		}
		if err := client.Rcpt(address); err != nil {
			results[i] = remoteFailure(err, 451, "4.3.0",
				"The recipient's mail server refused the recipient")
			continue
		}
		accepted++
	}
	if accepted == 0 {
		return results, nil, false
	}

	if deadlineErr := r.extend(conn, r.dataTimeout); deadlineErr != nil {
		return spread(results, temporaryf(421, "4.4.2", deadlineErr, "Could not bound the outbound session")), nil, false
	}
	if err := r.writeMessage(client, body); err != nil {
		return spread(results, remoteFailure(err, 451, "4.3.0",
			"The recipient's mail server did not take the message")), nil, false
	}
	return results, nil, false
}

// writeMessage sends DATA and the body. Close is what reads the final reply, so
// its error is the one that says whether the message was accepted — a write that
// succeeded proves nothing.
func (r *Relay) writeMessage(client *smtp.Client, body []byte) error {
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(body); err != nil {
		return errors.Join(err, writer.Close())
	}
	return writer.Close()
}

// spread gives the recipients that were accepted the outcome of the body, and
// leaves the ones already refused at RCPT with their own answer.
func spread(results []error, failure *DeliveryError) []error {
	for i := range results {
		if results[i] == nil {
			results[i] = failure
		}
	}
	return results
}

// extend sets the deadline for the next step from the real clock. An injected
// clock must never reach here: a deadline computed from a frozen clock is a
// deadline in the past, and a deadline from a fast-forwarded one is no deadline
// at all.
func (r *Relay) extend(conn net.Conn, timeout time.Duration) error {
	return conn.SetDeadline(time.Now().Add(timeout))
}

func (r *Relay) close(conn net.Conn) {
	if err := conn.Close(); err != nil {
		r.logger.Debug("maild: closing an outbound connection failed", "error", err)
	}
}

func (r *Relay) quit(client *smtp.Client) {
	if err := client.Quit(); err != nil {
		r.logger.Debug("maild: closing an outbound SMTP session failed", "error", err)
	}
}

// tlsConfigFor builds the opportunistic STARTTLS configuration.
//
// Verification is off by default, and that is deliberate rather than sloppy.
// RFC 7435 opportunistic security: almost every public MX presents a
// certificate that does not chain to a public root or does not name the host the
// MX record pointed at, so verifying would refuse TLS with the majority of the
// internet and fall back to cleartext — strictly worse than an unverified
// session, which still defeats a passive observer. An operator who wants
// verified delivery sets RelayConfig.TLSConfig and gets exactly what they ask
// for.
func (r *Relay) tlsConfigFor(host string) *tls.Config {
	if r.tlsConfig != nil {
		config := r.tlsConfig.Clone()
		if config.ServerName == "" {
			config.ServerName = host
		}
		return config
	}
	return &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
	}
}

// -----------------------------------------------------------------------------
// Turning a failure into a retry decision
// -----------------------------------------------------------------------------

// remoteFailure turns a client error into the failure it means.
//
// Only a reply the remote server actually sent can make a failure permanent.
// A dropped connection, a timeout or a TLS error produces the temporary answer
// the caller supplied, because none of them is a refusal.
func remoteFailure(err error, code int, enhanced, message string) *DeliveryError {
	var reply *textproto.Error
	if errors.As(err, &reply) && reply.Code >= 400 && reply.Code < 600 {
		return &DeliveryError{
			Code:     reply.Code,
			Enhanced: enhancedStatus(reply.Code, reply.Msg),
			Message:  sanitizeReplyText(reply.Msg),
			cause:    err,
		}
	}
	return temporaryf(code, enhanced, err, "%s", message)
}

// resolverFailure turns a lookup error into the failure it means.
func resolverFailure(err error) *DeliveryError {
	if errors.Is(err, ErrNoMailHost) {
		return &DeliveryError{
			Code: 550, Enhanced: "5.1.2",
			Message: "The recipient's domain has no mail server",
			cause:   err,
		}
	}
	return temporaryf(451, "4.4.3", err, "Could not look up the recipient's mail server")
}

// enhancedStatus is the DSN status a bounce reports. RFC 3463 puts it at the
// front of the reply text, so it is taken from there when the remote server sent
// one and derived from the reply class when it did not.
func enhancedStatus(code int, message string) string {
	if status, ok := leadingStatus(message); ok {
		return status
	}
	if code >= 500 {
		return "5.0.0"
	}
	return "4.0.0"
}

// leadingStatus reads an RFC 3463 "class.subject.detail" at the start of a reply.
func leadingStatus(message string) (string, bool) {
	candidate, _, _ := strings.Cut(strings.TrimSpace(message), " ")
	class, rest, found := strings.Cut(candidate, ".")
	if !found || (class != "2" && class != "4" && class != "5") {
		return "", false
	}
	subject, detail, found := strings.Cut(rest, ".")
	if !found || !isStatusNumber(subject) || !isStatusNumber(detail) {
		return "", false
	}
	return candidate, true
}

// isStatusNumber accepts the one-to-three digit subject and detail fields.
func isStatusNumber(field string) bool {
	if field == "" || len(field) > 3 {
		return false
	}
	for i := 0; i < len(field); i++ {
		if field[i] < '0' || field[i] > '9' {
			return false
		}
	}
	return true
}
