package maild

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// -----------------------------------------------------------------------------
// Admission control
//
// One component, one mutex, three refusals that all have to be made before any
// work is done. This server holds a public MX on port 25, so the interesting
// question about any request is not "can we serve it" but "what does an
// unauthenticated stranger get to make us spend".
//
//  1. Sessions. The accept loop has a global slot count. Without a per-peer
//     share, one host opening connections and saying nothing occupies every
//     slot and legitimate mail is refused at the door — a complete outage
//     costing the attacker one socket per slot. MaxSessionsPerIP is the fix and
//     it is the whole fix: a peer over its share is refused, everyone else is
//     unaffected.
//
//  2. Authentication failures. Password verification is PBKDF2 at 600,000
//     iterations, about 52 ms of a core, and the delivery path deliberately
//     burns the same 52 ms on an account that does not exist so that timing
//     cannot enumerate mailboxes. That makes an unauthenticated guess both a
//     brute-force attempt and a CPU-exhaustion attack: twenty sessions guessing
//     in a loop is a busy machine. So the cheap check must come first.
//     AllowAuth is a map lookup under a mutex and must be called BEFORE the
//     hash; RecordAuthFailure after it. Locking out both the account and the
//     source address covers the two shapes of the attack — one host guessing
//     many passwords, and many hosts guessing one account.
//
//  3. Message memory. SMTP DATA is accumulated in a bytes.Buffer, so the memory
//     this process can be made to hold is the largest message multiplied by the
//     number of sessions in DATA at once. At the defaults that product is
//     256 x 25 MiB = 6.4 GiB, which is an OOM an attacker can schedule.
//     AcquireMessage reserves MaxMessageBytes for the duration of one body and
//     MaxMessageMemory caps the sum, so the product is bounded by construction
//     at MaxConcurrentMessages bodies. Reserving at DATA rather than at accept
//     is what keeps the bound honest without cutting the session count: idle
//     sessions cost no buffer, so they are not charged for one.
//
// Every deadline in here comes from an injectable clock, so a test asserts a
// lockout expiry by moving time rather than by sleeping.
// -----------------------------------------------------------------------------

// The refusals. Each is safe to show a peer as-is: it names a limit and never
// quotes a credential, an address or anything else the peer sent.
var (
	// ErrTooManySessions is the global slot count. It deserves a 421 in SMTP:
	// the condition is transient and the peer should come back.
	ErrTooManySessions = errors.New("maild: the server is at its connection limit")
	// ErrTooManySessionsFromIP is this peer's share of that count.
	ErrTooManySessionsFromIP = errors.New("maild: too many simultaneous connections from that address")
	// ErrTooManyAuthFailures is the lockout. The message does not say whether
	// the account or the address is the locked one, because saying so tells an
	// attacker whether the account exists.
	ErrTooManyAuthFailures = errors.New("maild: too many failed authentication attempts, try again later")
	// ErrMessageMemoryBusy is the joint memory bound. It deserves a 452 in
	// SMTP: insufficient system storage, try again.
	ErrMessageMemoryBusy = errors.New("maild: the server is at its message memory limit")
)

// LimiterConfig is the whole policy. Every field has a default, and a
// non-positive value takes it, so the zero LimiterConfig is the production
// policy rather than an unlimited one — the failure mode of a forgotten field
// must never be "no limit".
type LimiterConfig struct {
	// MaxSessions is the number of concurrent line-protocol connections across
	// every listener. It matches Limits.MaxSessions.
	MaxSessions int // 256
	// MaxSessionsPerIP is one peer's share of MaxSessions. It has to be
	// generous enough for a real mail server relaying a burst, and small
	// enough that a peer cannot take the lot: 16 of 256 means it takes
	// sixteen distinct source addresses to fill the server.
	MaxSessionsPerIP int // 16

	// MaxMessageBytes is the largest single message, and therefore the amount
	// AcquireMessage reserves. It matches SMTPConfig.MaxMessageBytes; if the
	// two disagree the smaller one is the real limit and the larger one is a
	// lie, so set them from one place.
	MaxMessageBytes int64 // 25 MiB
	// MaxMessageMemory caps the sum of those reservations. It is raised to
	// MaxMessageBytes if it is set below it, because a limiter that can never
	// admit a single message is a broken mail server rather than a safe one.
	MaxMessageMemory int64 // 256 MiB

	// AuthFailures is how many failures a key may accumulate before it is
	// locked. It is per account and, separately, per source address.
	AuthFailures int // 5
	// AuthWindow is how long a failure is remembered. A key whose last failure
	// is older than this starts counting again, so an occasional typo never
	// accumulates into a lockout.
	AuthWindow time.Duration // 15m
	// AuthLockout is the first lockout. Each further failure doubles it, which
	// is what turns a lockout into a backoff: a client that retries once a
	// minute recovers quickly, and a client guessing in a loop is refused for
	// AuthLockoutMax within a few attempts.
	AuthLockout time.Duration // 1m
	// AuthLockoutMax caps that doubling. Nothing is locked out forever: an
	// account lockout is also a way to deny a customer their mail, and a
	// permanent one would make that attack permanent too.
	AuthLockoutMax time.Duration // 15m
	// MaxTrackedAuthKeys bounds the failure table. A peer chooses the
	// usernames it presents, so without this bound the table is an
	// unbounded allocation an attacker controls. Over the bound, expired
	// entries are swept and then the entry closest to expiry is evicted, so
	// the table never grows and never simply stops recording.
	MaxTrackedAuthKeys int // 4096

	// Now is the clock. Tests replace it; production leaves it nil for
	// time.Now.
	Now func() time.Time
}

func (c LimiterConfig) withDefaults() LimiterConfig {
	if c.MaxSessions <= 0 {
		c.MaxSessions = 256
	}
	if c.MaxSessionsPerIP <= 0 {
		c.MaxSessionsPerIP = 16
	}
	if c.MaxSessionsPerIP > c.MaxSessions {
		c.MaxSessionsPerIP = c.MaxSessions
	}
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = DefaultSMTPMaxMessageBytes
	}
	if c.MaxMessageMemory <= 0 {
		c.MaxMessageMemory = 256 << 20
	}
	if c.MaxMessageMemory < c.MaxMessageBytes {
		c.MaxMessageMemory = c.MaxMessageBytes
	}
	if c.AuthFailures <= 0 {
		c.AuthFailures = 5
	}
	if c.AuthWindow <= 0 {
		c.AuthWindow = 15 * time.Minute
	}
	if c.AuthLockout <= 0 {
		c.AuthLockout = time.Minute
	}
	if c.AuthLockoutMax <= 0 {
		c.AuthLockoutMax = 15 * time.Minute
	}
	if c.AuthLockoutMax < c.AuthLockout {
		c.AuthLockoutMax = c.AuthLockout
	}
	if c.MaxTrackedAuthKeys <= 0 {
		c.MaxTrackedAuthKeys = 4096
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Limiter is the admission control for one server. It is safe for concurrent
// use; every method takes the same mutex and holds it for a map operation, so
// contention costs less than the work it is protecting.
type Limiter struct {
	config LimiterConfig

	mu           sync.Mutex
	sessions     int
	perIP        map[string]int
	messageBytes int64
	auth         map[string]*authFailures
}

// authFailures is one key's history: an account, or a source address.
type authFailures struct {
	count int
	last  time.Time
	until time.Time
}

// expiry is when this entry stops mattering and may be dropped. Both halves
// count: a locked entry outlives its window, and a counting entry outlives its
// (zero) lock.
func (a *authFailures) expiry(window time.Duration) time.Time {
	forgotten := a.last.Add(window)
	if a.until.After(forgotten) {
		return a.until
	}
	return forgotten
}

// NewLimiter builds a Limiter. It cannot fail: an out-of-range field takes its
// default rather than refusing to start a mail server over a configuration
// typo. Config reports what was actually chosen, which is what startup logging
// should print.
func NewLimiter(config LimiterConfig) *Limiter {
	return &Limiter{
		config: config.withDefaults(),
		perIP:  make(map[string]int),
		auth:   make(map[string]*authFailures),
	}
}

// Config is the effective policy, defaults applied.
func (l *Limiter) Config() LimiterConfig { return l.config }

// MaxConcurrentMessages is the joint memory bound made legible: the number of
// message bodies that can be in flight at once, which is what actually bounds
// this process's memory. It is at least 1.
func (l *Limiter) MaxConcurrentMessages() int {
	return int(l.config.MaxMessageMemory / l.config.MaxMessageBytes)
}

// -----------------------------------------------------------------------------
// Leases
// -----------------------------------------------------------------------------

type leaseKind int

const (
	leaseSession leaseKind = iota
	leaseMessage
)

// Lease is a held reservation. Release is idempotent and nil-safe, so
//
//	lease, err := limiter.AcquireSession(remote)
//	defer lease.Release()
//	if err != nil { ... }
//
// is correct: a refused acquisition returns a nil *Lease, and releasing it
// twice — once by a defer and once by hand — is not a double free. Getting that
// wrong would either leak a slot for the process's lifetime or let the counter
// go negative, and both are silent.
type Lease struct {
	limiter  *Limiter
	kind     leaseKind
	ip       string
	bytes    int64
	released atomic.Bool
}

// Release returns the reservation.
func (l *Lease) Release() {
	if l == nil || !l.released.CompareAndSwap(false, true) {
		return
	}
	l.limiter.mu.Lock()
	defer l.limiter.mu.Unlock()
	switch l.kind {
	case leaseMessage:
		l.limiter.messageBytes -= l.bytes
	case leaseSession:
		l.limiter.sessions--
		if remaining := l.limiter.perIP[l.ip] - 1; remaining > 0 {
			l.limiter.perIP[l.ip] = remaining
		} else {
			// Deleting rather than leaving a zero is what keeps this map
			// bounded by the number of live sessions instead of by the number
			// of addresses that have ever connected.
			delete(l.limiter.perIP, l.ip)
		}
	}
}

// -----------------------------------------------------------------------------
// Sessions
// -----------------------------------------------------------------------------

// AcquireSession reserves a session slot for the peer at remoteAddr, which may
// be either a bare address or the "host:port" form net.Conn.RemoteAddr gives.
//
// It never blocks. A connection that would have to wait is refused, because a
// peer that is refused retries somewhere useful and a peer that is queued holds
// a socket while it times out.
func (l *Limiter) AcquireSession(remoteAddr string) (*Lease, error) {
	ip := limiterIP(remoteAddr)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sessions >= l.config.MaxSessions {
		return nil, ErrTooManySessions
	}
	if l.perIP[ip] >= l.config.MaxSessionsPerIP {
		return nil, ErrTooManySessionsFromIP
	}
	l.sessions++
	l.perIP[ip]++
	return &Lease{limiter: l, kind: leaseSession, ip: ip}, nil
}

// AcquireMessage reserves room for one message body, for the whole time that
// body is held in memory. Call it before reading DATA and release it once the
// body has been handed to delivery.
//
// It reserves the maximum rather than the announced SIZE. The announced size is
// the client's claim, and the bound has to hold when the claim is a lie.
func (l *Limiter) AcquireMessage() (*Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.messageBytes+l.config.MaxMessageBytes > l.config.MaxMessageMemory {
		return nil, ErrMessageMemoryBusy
	}
	l.messageBytes += l.config.MaxMessageBytes
	return &Lease{limiter: l, kind: leaseMessage, bytes: l.config.MaxMessageBytes}, nil
}

// -----------------------------------------------------------------------------
// Authentication failures
// -----------------------------------------------------------------------------

// AllowAuth reports whether an authentication attempt from remoteAddr for
// account may proceed. account is whatever the peer presented as a username; it
// does not have to exist, and it is never logged or returned by this package.
//
// Call this BEFORE verifying the password. That is the entire point: the
// verification is 52 ms of CPU and this is a map lookup, so the cheap answer
// has to come first or the lockout is itself the attack.
func (l *Limiter) AllowAuth(remoteAddr, account string) error {
	now := l.config.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range l.authKeys(remoteAddr, account) {
		if entry, ok := l.auth[key]; ok && now.Before(entry.until) {
			return ErrTooManyAuthFailures
		}
	}
	return nil
}

// RecordAuthFailure records one failed attempt against both the source address
// and the presented account. Call it for every failure, including a username
// that does not exist — an attacker enumerating names is the case this is for.
func (l *Limiter) RecordAuthFailure(remoteAddr, account string) {
	now := l.config.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range l.authKeys(remoteAddr, account) {
		entry := l.trackLocked(key, now)
		if entry == nil {
			continue
		}
		if !entry.last.IsZero() && now.Sub(entry.last) > l.config.AuthWindow {
			entry.count = 0
		}
		entry.count++
		entry.last = now
		if entry.count >= l.config.AuthFailures {
			entry.until = now.Add(l.backoff(entry.count))
		}
	}
}

// RecordAuthSuccess clears both keys. Clearing the source address as well as
// the account is deliberate: a peer that has proved it holds a credential is
// not the peer this is defending against, and leaving its counter half full
// would lock out a customer whose client retried an old password before being
// corrected.
func (l *Limiter) RecordAuthSuccess(remoteAddr, account string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range l.authKeys(remoteAddr, account) {
		delete(l.auth, key)
	}
}

// backoff is the lockout for the nth failure: the first lockout doubled once
// per failure past the threshold, capped at AuthLockoutMax.
//
// The doubling is a loop that stops at the cap rather than a shift, because
// the peer chooses how many times it fails and therefore chooses the shift
// distance. A shift by a peer-chosen amount overflows into a *shorter* lockout,
// which would turn "kept guessing" into "got let back in sooner".
func (l *Limiter) backoff(count int) time.Duration {
	lockout := l.config.AuthLockout
	for i := l.config.AuthFailures; i < count; i++ {
		if lockout >= l.config.AuthLockoutMax {
			break
		}
		lockout *= 2
	}
	if lockout > l.config.AuthLockoutMax {
		return l.config.AuthLockoutMax
	}
	return lockout
}

// authKeys is the pair of keys one attempt touches. The prefixes keep the two
// namespaces apart in one map — an account may not be an address at all, and
// "1.2.3.4" is a legal thing for a peer to present as a username.
func (l *Limiter) authKeys(remoteAddr, account string) []string {
	keys := make([]string, 0, 2)
	keys = append(keys, "ip\x00"+limiterIP(remoteAddr))
	if account = limiterAccount(account); account != "" {
		keys = append(keys, "account\x00"+account)
	}
	return keys
}

// trackLocked returns the entry for key, creating it if there is room. The
// caller holds the mutex.
func (l *Limiter) trackLocked(key string, now time.Time) *authFailures {
	if entry, ok := l.auth[key]; ok {
		return entry
	}
	if len(l.auth) >= l.config.MaxTrackedAuthKeys {
		l.sweepLocked(now)
	}
	if len(l.auth) >= l.config.MaxTrackedAuthKeys {
		// Still full of live entries, so the table is under attack from more
		// distinct keys than it can hold. Drop the one closest to expiring:
		// that is the entry whose loss protects the fewest future attempts,
		// and it keeps the table recording rather than failing open.
		l.evictLocked()
	}
	if len(l.auth) >= l.config.MaxTrackedAuthKeys {
		return nil
	}
	entry := &authFailures{}
	l.auth[key] = entry
	return entry
}

// sweepLocked drops every entry that is neither locked nor still inside its
// window. The caller holds the mutex.
func (l *Limiter) sweepLocked(now time.Time) {
	for key, entry := range l.auth {
		if now.After(entry.expiry(l.config.AuthWindow)) {
			delete(l.auth, key)
		}
	}
}

// evictLocked drops the single entry closest to expiry. The caller holds the
// mutex.
func (l *Limiter) evictLocked() {
	var oldestKey string
	var oldest time.Time
	for key, entry := range l.auth {
		expiry := entry.expiry(l.config.AuthWindow)
		if oldestKey == "" || expiry.Before(oldest) {
			oldestKey, oldest = key, expiry
		}
	}
	if oldestKey != "" {
		delete(l.auth, oldestKey)
	}
}

// -----------------------------------------------------------------------------
// Introspection
// -----------------------------------------------------------------------------

// LimiterStats is a snapshot for logging and for tests. It is deliberately
// counters only: nothing here identifies an account or a password.
type LimiterStats struct {
	Sessions         int
	MaxSessions      int
	MessageBytesHeld int64
	MaxMessageMemory int64
	TrackedAuthKeys  int
	TrackedIPs       int
}

// Stats reads the counters.
func (l *Limiter) Stats() LimiterStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return LimiterStats{
		Sessions:         l.sessions,
		MaxSessions:      l.config.MaxSessions,
		MessageBytesHeld: l.messageBytes,
		MaxMessageMemory: l.config.MaxMessageMemory,
		TrackedAuthKeys:  len(l.auth),
		TrackedIPs:       len(l.perIP),
	}
}

// -----------------------------------------------------------------------------
// Keys
// -----------------------------------------------------------------------------

// limiterIP reduces whatever the caller has to one stable key per peer. It
// takes "host:port" or a bare host, and canonicalises the address so that
// "::ffff:192.0.2.1", "192.0.2.1" and "[::FFFF:192.0.2.1]:25" are one peer
// rather than three shares of the limit.
//
// Something that is not an address at all — a Unix socket path, an empty
// string from a test conn — becomes a key of its own, truncated, so that a
// bad address cannot become an unbounded map key or a shared bucket that
// merges unrelated peers.
func limiterIP(remoteAddr string) string {
	address := strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	address = strings.TrimSuffix(strings.TrimPrefix(address, "["), "]")
	if parsed, err := netip.ParseAddr(address); err == nil {
		return parsed.Unmap().WithZone("").String()
	}
	if address == "" {
		return "unknown"
	}
	const maxKey = 64
	if len(address) > maxKey {
		address = address[:maxKey]
	}
	return address
}

// limiterAccount folds a presented username to the same form the store keys
// on, and bounds its length: the peer chooses this string, and it becomes a
// map key.
func limiterAccount(account string) string {
	account = strings.ToLower(strings.Trim(account, " "))
	if len(account) > MaxAddressBytes {
		account = account[:MaxAddressBytes]
	}
	return account
}
