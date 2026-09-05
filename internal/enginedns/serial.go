package enginedns

import (
	"context"
	"fmt"
	"sync"
)

// Serializer gives every zone its own queue. Attach, the periodic reconcilers,
// a gateway change and a mail bind all plan against the zone they read, so two
// of them running concurrently on one zone would compute their plans against
// stale state and flap the records between two answers. Callers queue instead.
//
// The zero value is not usable; call NewSerializer.
type Serializer struct {
	mu    sync.Mutex
	gates map[string]*gate
}

type gate struct {
	held chan struct{}
	refs int
}

func NewSerializer() *Serializer {
	return &Serializer{gates: make(map[string]*gate)}
}

// Run calls fn while holding the queue for zoneID. It returns ctx.Err() if the
// context is done before the queue is free, and fn's error otherwise. fn must
// not call Run for the same zone.
func (s *Serializer) Run(ctx context.Context, zoneID string, fn func(ctx context.Context) error) error {
	if fn == nil {
		return fmt.Errorf("serialize zone %q: no work given", zoneID)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("serialize zone %q: %w", zoneID, err)
	}

	entry := s.acquire(zoneID)
	select {
	case entry.held <- struct{}{}:
	case <-ctx.Done():
		s.release(zoneID, entry)
		return fmt.Errorf("serialize zone %q: %w", zoneID, ctx.Err())
	}
	defer func() {
		<-entry.held
		s.release(zoneID, entry)
	}()
	return fn(ctx)
}

// Pending reports how many callers hold or wait for a zone's queue. It exists
// for tests and for the janitor's leak check.
func (s *Serializer) Pending(zoneID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.gates[zoneID]
	if !ok {
		return 0
	}
	return entry.refs
}

func (s *Serializer) acquire(zoneID string) *gate {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.gates[zoneID]
	if !ok {
		entry = &gate{held: make(chan struct{}, 1)}
		s.gates[zoneID] = entry
	}
	entry.refs++
	return entry
}

func (s *Serializer) release(zoneID string, entry *gate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.refs--
	if entry.refs <= 0 {
		delete(s.gates, zoneID)
	}
}
