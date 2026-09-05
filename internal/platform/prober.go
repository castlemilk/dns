package platform

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/secretguard"
)

const (
	// ProbeInterval is the background cadence.
	ProbeInterval = 60 * time.Second
	// ProbeMinInterval bounds how often a client can force a re-probe of one
	// engine through GetPlatformStatus{probe:true}.
	ProbeMinInterval = 10 * time.Second
	// probeTimeout bounds one engine's probe so a hung engine cannot stall the
	// whole pass.
	probeTimeout = 10 * time.Second
	// probeAllBudget bounds how long ProbeAll waits for the pass it started.
	// The engines are probed concurrently, so a caller waits for the slowest
	// engine rather than for their sum; past the budget it returns with the
	// cache it has and the stragglers finish in the background. This is what
	// keeps GetPlatformStatus{probe:true} inside the web client's 30 s
	// transport timeout when every engine is slow at once.
	probeAllBudget = 5 * time.Second
	// maxProbeReason bounds the operator-facing failure text.
	maxProbeReason = 300
)

// Prober keeps each engine's last known status in memory. Nothing in a request
// path ever calls an engine to answer a status question: the RPC reads this
// cache, and `probe:true` only nudges the cadence.
type Prober struct {
	probes []Probe
	deps   Deps

	mu        sync.RWMutex
	onEvent   func(kind platformv1.EngineKind, reachable bool)
	statuses  map[platformv1.EngineKind]EngineStatus
	lastProbe map[platformv1.EngineKind]time.Time
	// probed records which engines this process has probed at least once. The
	// cache is seeded from Status() at construction (Reachable=false), so
	// without it the first successful probe of a healthy engine would look
	// like a false→true transition and every restart would record one
	// engine.recovered per engine. A restart is not a recovery.
	probed map[platformv1.EngineKind]bool
	// inflight keeps a second pass from stacking a goroutine on an engine that
	// is still answering the first one.
	inflight map[platformv1.EngineKind]bool
}

// NewProber wires the prober to the engines it watches. Order is preserved, so
// callers control the order engines appear in the status response.
func NewProber(probes []Probe, deps Deps) *Prober {
	prober := &Prober{
		probes:    probes,
		deps:      deps,
		statuses:  make(map[platformv1.EngineKind]EngineStatus, len(probes)),
		lastProbe: make(map[platformv1.EngineKind]time.Time, len(probes)),
		probed:    make(map[platformv1.EngineKind]bool, len(probes)),
		inflight:  make(map[platformv1.EngineKind]bool, len(probes)),
	}
	for _, probe := range probes {
		if probe == nil {
			continue
		}
		status := probe.Status()
		prober.statuses[probe.Kind()] = status
		prober.deps.Meter().SetEngineReachable(EngineName(probe.Kind()), status.Reachable)
	}
	return prober
}

// OnTransition registers a callback fired when an engine's reachability
// changes. The billing reconciler uses it to catch up after an outage.
//
// It is safe to call at any time — the callback is stored under the same mutex
// probeOne reads it with — but registering it before Run starts is what
// guarantees the first transition is delivered.
func (p *Prober) OnTransition(callback func(kind platformv1.EngineKind, reachable bool)) {
	p.mu.Lock()
	p.onEvent = callback
	p.mu.Unlock()
}

// Run probes every engine on a jittered 60 s cadence until ctx is done.
func (p *Prober) Run(ctx context.Context) error {
	p.ProbeAll(ctx, true)
	for {
		timer := time.NewTimer(jitter(ProbeInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
			p.ProbeAll(ctx, true)
		}
	}
}

// ProbeAll probes every engine concurrently and waits at most probeAllBudget
// for the pass. When force is false an engine probed within ProbeMinInterval is
// skipped, which is what makes `probe:true` safe to call from the UI.
//
// Concurrency and the budget together bound the request path: GetPlatformStatus
// serves the cache, so a slow engine costs the caller at most probeAllBudget,
// never the sum of every engine's probeTimeout. Each probe runs on a context
// detached from the caller's (bounded by probeTimeout) so a straggler still
// refreshes the cache after ProbeAll returned — the next status poll, seconds
// later, then reports it.
func (p *Prober) ProbeAll(ctx context.Context, force bool) {
	now := p.deps.Now()
	var running sync.WaitGroup
	for _, probe := range p.probes {
		if probe == nil {
			continue
		}
		kind := probe.Kind()
		p.mu.Lock()
		if !force {
			if last := p.lastProbe[kind]; !last.IsZero() && now.Sub(last) < ProbeMinInterval {
				p.mu.Unlock()
				continue
			}
		}
		if p.inflight[kind] {
			// A previous pass is still waiting on this engine. Starting a
			// second one would only queue more work against something already
			// too slow to answer.
			p.mu.Unlock()
			continue
		}
		p.inflight[kind] = true
		p.mu.Unlock()

		running.Add(1)
		go func(probe Probe) {
			defer running.Done()
			defer func() {
				p.mu.Lock()
				delete(p.inflight, kind)
				p.mu.Unlock()
			}()
			probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
			defer cancel()
			p.probeOne(probeCtx, probe)
		}(probe)
	}

	done := make(chan struct{})
	go func() {
		running.Wait()
		close(done)
	}()
	timer := time.NewTimer(probeAllBudget)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
}

func (p *Prober) probeOne(ctx context.Context, probe Probe) {
	kind := probe.Kind()
	engine := EngineName(kind)

	err := probe.Probe(ctx)

	status := probe.Status()
	status.Kind = kind
	if status.CheckedAt.IsZero() {
		status.CheckedAt = p.deps.Now()
	}
	if err != nil && status.Reason == "" {
		status.Reason = truncate(secretguard.Redact(err.Error()), maxProbeReason)
	}

	p.mu.Lock()
	previous := p.statuses[kind]
	first := !p.probed[kind]
	p.probed[kind] = true
	p.statuses[kind] = status
	p.lastProbe[kind] = p.deps.Now()
	onEvent := p.onEvent
	p.mu.Unlock()

	p.deps.Meter().SetEngineReachable(engine, status.Reachable)
	if !status.Configured {
		return
	}
	// Only transitions are worth an event: a permanently unreachable engine
	// would otherwise fill the log with one line a minute.
	//
	// The first probe of a process is not a transition. The cache starts at
	// Reachable=false, so treating it as one would record engine.recovered for
	// every healthy engine on every rollout, right beside platform.started. An
	// engine that is already down when we start is still news, so that case
	// records engine.unreachable and the later recovery reads correctly.
	changed := previous.Reachable != status.Reachable || !previous.Configured
	if first {
		changed = !status.Reachable
	}
	if !changed {
		return
	}
	if status.Reachable {
		p.deps.Record(ctx, activity.Event{
			Actor:    activity.ActorSystem,
			Kind:     activity.KindEngineRecovered,
			Severity: activity.SeverityInfo,
			Summary:  "The " + engine + " engine is reachable again",
			Details:  map[string]string{"engine": engine},
		})
	} else {
		p.deps.Record(ctx, activity.Event{
			Actor:    activity.ActorSystem,
			Kind:     activity.KindEngineUnreachable,
			Severity: activity.SeverityWarn,
			Summary:  "The " + engine + " engine is unreachable",
			Details:  map[string]string{"engine": engine, "reason": status.Reason},
		})
	}
	if onEvent != nil {
		onEvent(kind, status.Reachable)
	}
}

// Statuses returns the cached status of every engine, in registration order.
func (p *Prober) Statuses() []EngineStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]EngineStatus, 0, len(p.probes))
	for _, probe := range p.probes {
		if probe == nil {
			continue
		}
		status, ok := p.statuses[probe.Kind()]
		if !ok {
			status = EngineStatus{Kind: probe.Kind()}
		}
		result = append(result, status)
	}
	return result
}

// Status returns one engine's cached status.
func (p *Prober) Status(kind platformv1.EngineKind) (EngineStatus, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	status, ok := p.statuses[kind]
	return status, ok
}

// jitter spreads periodic work by ±10 % so a restart does not synchronise every
// worker onto the same instant.
func jitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return interval
	}
	spread := float64(interval) * 0.2
	return time.Duration(float64(interval)*0.9 + rand.Float64()*spread)
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
