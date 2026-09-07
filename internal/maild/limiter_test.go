package maild

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testLimiterClock is the injectable clock. Every deadline assertion below
// moves it instead of sleeping, so the suite is deterministic and the lockout
// arithmetic is tested at its exact boundary rather than near it.
type testLimiterClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestLimiterClock() *testLimiterClock {
	return &testLimiterClock{now: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
}

func (c *testLimiterClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testLimiterClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

func TestNewLimiterDefaultsAreTheProductionPolicy(t *testing.T) {
	config := NewLimiter(LimiterConfig{}).Config()
	if config.MaxSessions != 256 || config.MaxSessionsPerIP != 16 {
		t.Fatalf("session defaults = %d, %d; want 256, 16", config.MaxSessions, config.MaxSessionsPerIP)
	}
	if config.MaxMessageBytes != DefaultSMTPMaxMessageBytes {
		t.Fatalf("MaxMessageBytes = %d, want %d", config.MaxMessageBytes, DefaultSMTPMaxMessageBytes)
	}
	if config.MaxMessageMemory != 256<<20 {
		t.Fatalf("MaxMessageMemory = %d, want %d", config.MaxMessageMemory, 256<<20)
	}
	if config.AuthFailures != 5 || config.AuthWindow != 15*time.Minute {
		t.Fatalf("auth defaults = %d, %s", config.AuthFailures, config.AuthWindow)
	}
	if config.AuthLockout != time.Minute || config.AuthLockoutMax != 15*time.Minute {
		t.Fatalf("lockout defaults = %s, %s", config.AuthLockout, config.AuthLockoutMax)
	}
	if config.MaxTrackedAuthKeys != 4096 {
		t.Fatalf("MaxTrackedAuthKeys = %d, want 4096", config.MaxTrackedAuthKeys)
	}
	if config.Now == nil {
		t.Fatal("the default clock is nil")
	}
}

// TestLimiterClampsAnImpossibleConfiguration proves the two clamps that keep a
// misconfiguration from being either an outage or an unlimited server.
func TestLimiterClampsAnImpossibleConfiguration(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{
		MaxSessions:      4,
		MaxSessionsPerIP: 900,
		MaxMessageBytes:  10 << 20,
		MaxMessageMemory: 1 << 20, // below one message: cannot ever admit one
		AuthLockout:      time.Hour,
		AuthLockoutMax:   time.Minute,
	})
	config := limiter.Config()
	if config.MaxSessionsPerIP != 4 {
		t.Fatalf("MaxSessionsPerIP = %d, want it clamped to MaxSessions (4)", config.MaxSessionsPerIP)
	}
	if config.MaxMessageMemory != 10<<20 {
		t.Fatalf("MaxMessageMemory = %d, want it raised to MaxMessageBytes", config.MaxMessageMemory)
	}
	if config.AuthLockoutMax != time.Hour {
		t.Fatalf("AuthLockoutMax = %s, want it raised to AuthLockout", config.AuthLockoutMax)
	}
	if got := limiter.MaxConcurrentMessages(); got != 1 {
		t.Fatalf("MaxConcurrentMessages = %d, want 1", got)
	}
	lease, err := limiter.AcquireMessage()
	if err != nil {
		t.Fatalf("a limiter clamped to one message admitted none: %v", err)
	}
	lease.Release()
}

// -----------------------------------------------------------------------------
// Sessions
// -----------------------------------------------------------------------------

func TestSessionLimitIsReachedThenReleased(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{MaxSessions: 3, MaxSessionsPerIP: 3})
	held := make([]*Lease, 0, 3)
	for i := range 3 {
		lease, err := limiter.AcquireSession("192.0.2." + strconv.Itoa(i) + ":25")
		if err != nil {
			t.Fatalf("session %d was refused below the limit: %v", i, err)
		}
		held = append(held, lease)
	}
	if _, err := limiter.AcquireSession("192.0.2.9:25"); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("the fourth session returned %v, want ErrTooManySessions", err)
	}
	if got := limiter.Stats().Sessions; got != 3 {
		t.Fatalf("Sessions = %d, want 3", got)
	}

	held[0].Release()
	lease, err := limiter.AcquireSession("192.0.2.9:25")
	if err != nil {
		t.Fatalf("a slot was not reusable after Release: %v", err)
	}
	lease.Release()
	for _, lease := range held[1:] {
		lease.Release()
	}
	stats := limiter.Stats()
	if stats.Sessions != 0 || stats.TrackedIPs != 0 {
		t.Fatalf("after releasing everything: %+v, want no sessions and no tracked addresses", stats)
	}
}

// TestPerIPSessionLimitProtectsOtherPeers is the outage this exists to prevent:
// one host must not be able to occupy every slot.
func TestPerIPSessionLimitProtectsOtherPeers(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{MaxSessions: 8, MaxSessionsPerIP: 2})
	first, err := limiter.AcquireSession("198.51.100.7:2500")
	if err != nil {
		t.Fatalf("the first session from a peer was refused: %v", err)
	}
	second, err := limiter.AcquireSession("198.51.100.7:2501")
	if err != nil {
		t.Fatalf("the second session from a peer was refused: %v", err)
	}
	if _, err := limiter.AcquireSession("198.51.100.7:2502"); !errors.Is(err, ErrTooManySessionsFromIP) {
		t.Fatalf("the third session from one peer returned %v, want ErrTooManySessionsFromIP", err)
	}

	// A different peer is unaffected, which is the whole point.
	other, err := limiter.AcquireSession("203.0.113.4:25")
	if err != nil {
		t.Fatalf("an unrelated peer was refused while another peer was over its share: %v", err)
	}
	other.Release()

	first.Release()
	third, err := limiter.AcquireSession("198.51.100.7:2502")
	if err != nil {
		t.Fatalf("the peer could not reconnect after releasing a session: %v", err)
	}
	third.Release()
	second.Release()
}

// TestSessionKeysAreOnePeerPerAddressSpelling stops a peer from getting several
// shares of the limit by spelling its address differently.
func TestSessionKeysAreOnePeerPerAddressSpelling(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{MaxSessions: 8, MaxSessionsPerIP: 1})
	first, err := limiter.AcquireSession("192.0.2.10:25")
	if err != nil {
		t.Fatalf("the first session was refused: %v", err)
	}
	defer first.Release()
	for _, spelling := range []string{
		"192.0.2.10",
		"192.0.2.10:2525",
		"::ffff:192.0.2.10",
		"[::ffff:192.0.2.10]:587",
	} {
		if _, err := limiter.AcquireSession(spelling); !errors.Is(err, ErrTooManySessionsFromIP) {
			t.Fatalf("%q was treated as a different peer: %v", spelling, err)
		}
	}
	if got := limiter.Stats().TrackedIPs; got != 1 {
		t.Fatalf("TrackedIPs = %d, want 1", got)
	}
}

func TestReleaseIsIdempotentAndNilSafe(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{MaxSessions: 1, MaxSessionsPerIP: 1})
	lease, err := limiter.AcquireSession("192.0.2.1:25")
	if err != nil {
		t.Fatalf("AcquireSession = %v", err)
	}
	lease.Release()
	lease.Release()
	lease.Release()
	if got := limiter.Stats().Sessions; got != 0 {
		t.Fatalf("Sessions = %d after repeated Release, want 0", got)
	}

	// A refused acquisition hands back a nil lease, and the caller's defer
	// must survive it.
	refused, err := limiter.AcquireSession("192.0.2.1:25")
	if err != nil {
		t.Fatalf("AcquireSession = %v", err)
	}
	overLimit, err := limiter.AcquireSession("192.0.2.2:25")
	if !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("expected a refusal, got %v", err)
	}
	overLimit.Release()
	refused.Release()
	if got := limiter.Stats().Sessions; got != 0 {
		t.Fatalf("Sessions = %d, want 0: releasing a refused lease moved the counter", got)
	}
}

// -----------------------------------------------------------------------------
// The joint memory bound
// -----------------------------------------------------------------------------

// TestMessageMemoryBoundsTheProduct is the OOM this exists to prevent: DATA is
// buffered in memory, so concurrent bodies multiply.
func TestMessageMemoryBoundsTheProduct(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{
		MaxSessions:      64,
		MaxSessionsPerIP: 64,
		MaxMessageBytes:  10 << 20,
		MaxMessageMemory: 40 << 20,
	})
	if got := limiter.MaxConcurrentMessages(); got != 4 {
		t.Fatalf("MaxConcurrentMessages = %d, want 4", got)
	}
	held := make([]*Lease, 0, 4)
	for i := range 4 {
		lease, err := limiter.AcquireMessage()
		if err != nil {
			t.Fatalf("message %d was refused inside the bound: %v", i, err)
		}
		held = append(held, lease)
	}
	if got := limiter.Stats().MessageBytesHeld; got != 40<<20 {
		t.Fatalf("MessageBytesHeld = %d, want %d", got, 40<<20)
	}
	if _, err := limiter.AcquireMessage(); !errors.Is(err, ErrMessageMemoryBusy) {
		t.Fatalf("the fifth body returned %v, want ErrMessageMemoryBusy", err)
	}

	// Sessions are not charged for a body they are not holding: a peer may
	// still connect while the memory budget is spent.
	session, err := limiter.AcquireSession("192.0.2.1:25")
	if err != nil {
		t.Fatalf("a session was refused because the message budget was full: %v", err)
	}
	session.Release()

	held[0].Release()
	lease, err := limiter.AcquireMessage()
	if err != nil {
		t.Fatalf("the budget was not reusable after Release: %v", err)
	}
	lease.Release()
	for _, lease := range held[1:] {
		lease.Release()
	}
	if got := limiter.Stats().MessageBytesHeld; got != 0 {
		t.Fatalf("MessageBytesHeld = %d after releasing everything, want 0", got)
	}
}

// -----------------------------------------------------------------------------
// Authentication failures
// -----------------------------------------------------------------------------

func TestAuthLockoutAfterTheThresholdIsReached(t *testing.T) {
	clock := newTestLimiterClock()
	limiter := NewLimiter(LimiterConfig{AuthFailures: 3, AuthLockout: time.Minute, Now: clock.Now})

	const ip, account = "192.0.2.5:40000", "ada@acme.dev"
	for i := range 2 {
		if err := limiter.AllowAuth(ip, account); err != nil {
			t.Fatalf("attempt %d was refused below the threshold: %v", i, err)
		}
		limiter.RecordAuthFailure(ip, account)
	}
	if err := limiter.AllowAuth(ip, account); err != nil {
		t.Fatalf("the third attempt was refused before the third failure: %v", err)
	}
	limiter.RecordAuthFailure(ip, account)
	if err := limiter.AllowAuth(ip, account); !errors.Is(err, ErrTooManyAuthFailures) {
		t.Fatalf("AllowAuth after the threshold = %v, want ErrTooManyAuthFailures", err)
	}
	// The refusal must not say which of the two keys is locked.
	err := limiter.AllowAuth(ip, account)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, fragment := range []string{account, "192.0.2.5", "ada"} {
		if strings.Contains(err.Error(), fragment) {
			t.Fatalf("the lockout error %q names %q", err.Error(), fragment)
		}
	}
}

func TestAuthLockoutExpires(t *testing.T) {
	clock := newTestLimiterClock()
	limiter := NewLimiter(LimiterConfig{
		AuthFailures: 2,
		AuthLockout:  5 * time.Minute,
		AuthWindow:   time.Hour,
		Now:          clock.Now,
	})
	const ip, account = "192.0.2.6:1", "ada@acme.dev"
	limiter.RecordAuthFailure(ip, account)
	limiter.RecordAuthFailure(ip, account)
	if err := limiter.AllowAuth(ip, account); !errors.Is(err, ErrTooManyAuthFailures) {
		t.Fatalf("AllowAuth = %v, want a lockout", err)
	}

	clock.Advance(5*time.Minute - time.Nanosecond)
	if err := limiter.AllowAuth(ip, account); !errors.Is(err, ErrTooManyAuthFailures) {
		t.Fatalf("AllowAuth one nanosecond before expiry = %v, want a lockout", err)
	}
	clock.Advance(time.Nanosecond)
	if err := limiter.AllowAuth(ip, account); err != nil {
		t.Fatalf("AllowAuth at expiry = %v, want it allowed", err)
	}
}

// TestAuthBackoffDoublesAndIsCapped is the difference between a lockout and a
// backoff: guessing through an expiry costs more each time, up to a ceiling
// that keeps an account lockout from becoming a permanent denial of service.
func TestAuthBackoffDoublesAndIsCapped(t *testing.T) {
	clock := newTestLimiterClock()
	limiter := NewLimiter(LimiterConfig{
		AuthFailures:   1,
		AuthLockout:    time.Minute,
		AuthLockoutMax: 4 * time.Minute,
		AuthWindow:     time.Hour,
		Now:            clock.Now,
	})
	const ip, account = "192.0.2.7:1", "ada@acme.dev"
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 4 * time.Minute} {
		limiter.RecordAuthFailure(ip, account)
		if err := limiter.AllowAuth(ip, account); !errors.Is(err, ErrTooManyAuthFailures) {
			t.Fatalf("AllowAuth = %v, want a lockout", err)
		}
		clock.Advance(want - time.Nanosecond)
		if err := limiter.AllowAuth(ip, account); !errors.Is(err, ErrTooManyAuthFailures) {
			t.Fatalf("the lockout was shorter than %s", want)
		}
		clock.Advance(time.Nanosecond)
		if err := limiter.AllowAuth(ip, account); err != nil {
			t.Fatalf("the lockout was longer than %s: %v", want, err)
		}
	}
}

// TestAuthFailuresAreForgottenAfterTheWindow keeps an occasional typo from
// accumulating into a lockout over days.
func TestAuthFailuresAreForgottenAfterTheWindow(t *testing.T) {
	clock := newTestLimiterClock()
	limiter := NewLimiter(LimiterConfig{
		AuthFailures: 3,
		AuthWindow:   10 * time.Minute,
		AuthLockout:  time.Minute,
		Now:          clock.Now,
	})
	const ip, account = "192.0.2.8:1", "ada@acme.dev"
	for range 4 {
		limiter.RecordAuthFailure(ip, account)
		if err := limiter.AllowAuth(ip, account); err != nil {
			t.Fatalf("a failure an hour after the last one locked the key: %v", err)
		}
		clock.Advance(time.Hour)
	}
	// Three inside one window still locks.
	limiter.RecordAuthFailure(ip, account)
	limiter.RecordAuthFailure(ip, account)
	limiter.RecordAuthFailure(ip, account)
	if err := limiter.AllowAuth(ip, account); !errors.Is(err, ErrTooManyAuthFailures) {
		t.Fatalf("three failures inside the window = %v, want a lockout", err)
	}
}

// TestAuthLockoutIsolatesDifferentPeers: a locked-out source address must not
// take the whole server with it, and an account locked by one peer's guessing
// is the deliberate exception.
func TestAuthLockoutIsolatesDifferentPeers(t *testing.T) {
	clock := newTestLimiterClock()
	limiter := NewLimiter(LimiterConfig{AuthFailures: 2, AuthLockout: time.Minute, Now: clock.Now})
	const attacker = "192.0.2.66:1"
	limiter.RecordAuthFailure(attacker, "ada@acme.dev")
	limiter.RecordAuthFailure(attacker, "ada@acme.dev")

	if err := limiter.AllowAuth(attacker, "ada@acme.dev"); !errors.Is(err, ErrTooManyAuthFailures) {
		t.Fatalf("the guessing peer was not locked out: %v", err)
	}
	// A different address for a different account: untouched.
	if err := limiter.AllowAuth("203.0.113.1:1", "grace@acme.dev"); err != nil {
		t.Fatalf("an unrelated peer and account were locked out: %v", err)
	}
	// The same peer, a different account: still locked, because the source
	// address is a key of its own.
	if err := limiter.AllowAuth(attacker, "grace@acme.dev"); !errors.Is(err, ErrTooManyAuthFailures) {
		t.Fatalf("the guessing peer got a fresh budget by changing account: %v", err)
	}
	// A different peer, the guessed account: locked, because otherwise a
	// botnet gets an unlimited budget against one mailbox.
	if err := limiter.AllowAuth("203.0.113.2:1", "ada@acme.dev"); !errors.Is(err, ErrTooManyAuthFailures) {
		t.Fatalf("the guessed account got a fresh budget from a new address: %v", err)
	}
}

func TestAuthAccountKeyFoldsCaseAndSpacing(t *testing.T) {
	clock := newTestLimiterClock()
	limiter := NewLimiter(LimiterConfig{AuthFailures: 2, AuthLockout: time.Minute, Now: clock.Now})
	limiter.RecordAuthFailure("192.0.2.1:1", "Ada@Acme.Dev")
	limiter.RecordAuthFailure("192.0.2.2:1", " ada@acme.dev ")
	if err := limiter.AllowAuth("203.0.113.9:1", "ADA@ACME.DEV"); !errors.Is(err, ErrTooManyAuthFailures) {
		t.Fatalf("a different spelling of the account got a fresh budget: %v", err)
	}
}

func TestAuthSuccessClearsTheCounters(t *testing.T) {
	clock := newTestLimiterClock()
	limiter := NewLimiter(LimiterConfig{AuthFailures: 3, AuthLockout: time.Minute, Now: clock.Now})
	const ip, account = "192.0.2.11:1", "ada@acme.dev"
	limiter.RecordAuthFailure(ip, account)
	limiter.RecordAuthFailure(ip, account)
	limiter.RecordAuthSuccess(ip, account)
	if got := limiter.Stats().TrackedAuthKeys; got != 0 {
		t.Fatalf("TrackedAuthKeys = %d after a success, want 0", got)
	}
	// The budget is whole again: two more failures do not lock.
	limiter.RecordAuthFailure(ip, account)
	limiter.RecordAuthFailure(ip, account)
	if err := limiter.AllowAuth(ip, account); err != nil {
		t.Fatalf("the counter was not cleared by the success: %v", err)
	}
}

// TestAuthWithNoAccountStillTracksTheAddress covers the peer that sends a
// malformed SASL blob: there is no username to key on, but the source address
// still has to pay for the attempt.
func TestAuthWithNoAccountStillTracksTheAddress(t *testing.T) {
	clock := newTestLimiterClock()
	limiter := NewLimiter(LimiterConfig{AuthFailures: 2, AuthLockout: time.Minute, Now: clock.Now})
	limiter.RecordAuthFailure("192.0.2.12:1", "")
	limiter.RecordAuthFailure("192.0.2.12:1", "   ")
	if got := limiter.Stats().TrackedAuthKeys; got != 1 {
		t.Fatalf("TrackedAuthKeys = %d, want 1: only the address should be keyed", got)
	}
	if err := limiter.AllowAuth("192.0.2.12:1", "ada@acme.dev"); !errors.Is(err, ErrTooManyAuthFailures) {
		t.Fatalf("AllowAuth = %v, want the address to be locked out", err)
	}
}

// TestAuthTableIsBounded is the memory half of the auth defence: the peer picks
// the usernames, so the table it fills must not be able to grow without limit.
func TestAuthTableIsBounded(t *testing.T) {
	clock := newTestLimiterClock()
	limiter := NewLimiter(LimiterConfig{
		AuthFailures:       3,
		AuthWindow:         10 * time.Minute,
		AuthLockout:        time.Minute,
		MaxTrackedAuthKeys: 32,
		Now:                clock.Now,
	})
	for i := range 5000 {
		limiter.RecordAuthFailure("192.0.2."+strconv.Itoa(i%250)+":1", "u"+strconv.Itoa(i)+"@acme.dev")
	}
	if got := limiter.Stats().TrackedAuthKeys; got > 32 {
		t.Fatalf("TrackedAuthKeys = %d, want it bounded at 32", got)
	}

	// Once the flood stops and the entries age out, the table still records
	// normally: eviction must not have left it permanently full.
	clock.Advance(time.Hour)
	const ip, account = "192.0.2.200:1", "ada@acme.dev"
	limiter.RecordAuthFailure(ip, account)
	limiter.RecordAuthFailure(ip, account)
	limiter.RecordAuthFailure(ip, account)
	if err := limiter.AllowAuth(ip, account); !errors.Is(err, ErrTooManyAuthFailures) {
		t.Fatalf("the table stopped recording after the flood: %v", err)
	}
}

// TestAuthKeyLengthIsBounded: a peer that presents a megabyte-long username
// must not get a megabyte-long map key for it.
func TestAuthKeyLengthIsBounded(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{AuthFailures: 2})
	long := make([]byte, 1<<16)
	for i := range long {
		long[i] = 'a'
	}
	limiter.RecordAuthFailure("192.0.2.13:1", string(long))
	limiter.RecordAuthFailure("192.0.2.13:1", string(long)+"different-suffix")
	// Both truncate to the same key, so the second failure lands on the first
	// entry rather than allocating a new one.
	if got := limiter.Stats().TrackedAuthKeys; got != 2 {
		t.Fatalf("TrackedAuthKeys = %d, want 2 (one address, one truncated account)", got)
	}
	if err := limiter.AllowAuth("192.0.2.13:1", string(long)); !errors.Is(err, ErrTooManyAuthFailures) {
		t.Fatalf("AllowAuth = %v, want a lockout", err)
	}
}

// -----------------------------------------------------------------------------
// Concurrency
// -----------------------------------------------------------------------------

// TestLimiterIsConcurrencySafe is the -race test. It also asserts the invariant
// that matters under contention: the counters return to zero, so no acquisition
// path leaks a slot and no release path double-frees one.
func TestLimiterIsConcurrencySafe(t *testing.T) {
	clock := newTestLimiterClock()
	limiter := NewLimiter(LimiterConfig{
		MaxSessions:        32,
		MaxSessionsPerIP:   4,
		MaxMessageBytes:    1 << 20,
		MaxMessageMemory:   8 << 20,
		AuthFailures:       3,
		MaxTrackedAuthKeys: 64,
		Now:                clock.Now,
	})

	var wg sync.WaitGroup
	for worker := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ip := "192.0.2." + strconv.Itoa(worker%6) + ":25"
			account := "u" + strconv.Itoa(worker%9) + "@acme.dev"
			for range 200 {
				session, err := limiter.AcquireSession(ip)
				if err != nil && !errors.Is(err, ErrTooManySessions) && !errors.Is(err, ErrTooManySessionsFromIP) {
					t.Errorf("AcquireSession returned an unexpected error: %v", err)
				}
				message, err := limiter.AcquireMessage()
				if err != nil && !errors.Is(err, ErrMessageMemoryBusy) {
					t.Errorf("AcquireMessage returned an unexpected error: %v", err)
				}
				if err := limiter.AllowAuth(ip, account); err != nil && !errors.Is(err, ErrTooManyAuthFailures) {
					t.Errorf("AllowAuth returned an unexpected error: %v", err)
				}
				limiter.RecordAuthFailure(ip, account)
				limiter.RecordAuthSuccess(ip, account)
				limiter.Stats()
				message.Release()
				session.Release()
				// A second release from another goroutine's view of the same
				// lease must still be a no-op.
				session.Release()
			}
		}()
	}
	// Move the clock while the workers read it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 500 {
			clock.Advance(time.Second)
		}
	}()
	wg.Wait()

	stats := limiter.Stats()
	if stats.Sessions != 0 || stats.MessageBytesHeld != 0 || stats.TrackedIPs != 0 {
		t.Fatalf("after the race: %+v, want every counter back at zero", stats)
	}
}
