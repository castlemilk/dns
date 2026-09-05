package telemetry

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const instrumentationScope = "github.com/castlemilk/dns"

var (
	durationBuckets = []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	sizeBuckets     = []float64{64, 128, 256, 512, 768, 1024, 1232, 1500, 2048, 4096, 8192, 16384, 32768, 65535, 262144, 1048576, 8388608}
)

// Metrics owns all application instruments. Its methods defensively collapse
// attribute values to small allowlists so callers cannot accidentally put DNS
// names, record contents, credentials, addresses, or other unbounded values in
// telemetry.
type Metrics struct {
	httpRequests       metric.Int64Counter
	httpDuration       metric.Float64Histogram
	authFailures       metric.Int64Counter
	rpcRequests        metric.Int64Counter
	rpcDuration        metric.Float64Histogram
	controlMutations   metric.Int64Counter
	controlDuration    metric.Float64Histogram
	dnsQueries         metric.Int64Counter
	dnsDuration        metric.Float64Histogram
	dnsResponseSize    metric.Int64Histogram
	dnsTruncated       metric.Int64Counter
	dnsWriteFailures   metric.Int64Counter
	snapshotBuilds     metric.Int64Counter
	snapshotBuildTime  metric.Float64Histogram
	snapshotSize       metric.Int64Histogram
	snapshotFetches    metric.Int64Counter
	snapshotFetchTime  metric.Float64Histogram
	snapshotApplies    metric.Int64Counter
	snapshotApplyTime  metric.Float64Histogram
	snapshotAdmissions metric.Int64Counter
	snapshotAdmitTime  metric.Float64Histogram
	snapshotCache      metric.Int64Counter
	snapshotCacheTime  metric.Float64Histogram
	checksumFailures   metric.Int64Counter
	readinessChecks    metric.Int64Counter
	storeTransactions  metric.Int64Counter
	storeDuration      metric.Float64Histogram
	authorityCompiles  metric.Int64Counter
	authorityCompile   metric.Float64Histogram
	authorityPublishes metric.Int64Counter
	engineRequests     metric.Int64Counter
	engineDuration     metric.Float64Histogram
	gatewayResolutions metric.Int64Counter
	billingWebhooks    metric.Int64Counter
	billingReconciles  metric.Int64Counter
	hostingUploads     metric.Int64Counter
	activityEvents     metric.Int64Counter
	activityDropped    metric.Int64Counter
	dnsOptions         map[dnsLabels]dnsMeasurementOptions
	transportOptions   map[string][]metric.AddOption
	engineReachable    map[string]*atomic.Int64

	clock               func() time.Time
	inventoryZones      atomic.Int64
	inventoryRecords    atomic.Int64
	snapshotLastValidNS atomic.Int64
	snapshotMaxStaleNS  atomic.Int64
	snapshotLoaded      atomic.Bool
	tracer              trace.Tracer
	metricsEnabled      bool
	tracesEnabled       bool
}

type metricsConfig struct {
	clock          func() time.Time
	tracer         trace.Tracer
	metricsEnabled bool
	tracesEnabled  bool
}

// MetricsOption customizes metric collection. It is primarily useful for a
// deterministic collection clock in tests.
type MetricsOption func(*metricsConfig)

func WithClock(clock func() time.Time) MetricsOption {
	return func(config *metricsConfig) {
		if clock != nil {
			config.clock = clock
		}
	}
}

func WithTracer(tracer trace.Tracer) MetricsOption {
	return func(config *metricsConfig) {
		if tracer != nil {
			config.tracer = tracer
			config.tracesEnabled = true
		}
	}
}

func withSignals(metricsEnabled, tracesEnabled bool) MetricsOption {
	return func(config *metricsConfig) {
		config.metricsEnabled = metricsEnabled
		config.tracesEnabled = tracesEnabled
	}
}

// NewMetrics registers the complete simpledns metric set with meter.
func NewMetrics(meter metric.Meter, options ...MetricsOption) (*Metrics, error) {
	config := metricsConfig{
		clock:          time.Now,
		tracer:         tracenoop.NewTracerProvider().Tracer(instrumentationScope),
		metricsEnabled: true,
	}
	for _, option := range options {
		option(&config)
	}
	m := &Metrics{
		clock:           config.clock,
		tracer:          config.tracer,
		metricsEnabled:  config.metricsEnabled,
		tracesEnabled:   config.tracesEnabled,
		engineReachable: make(map[string]*atomic.Int64, len(engineKinds)),
	}
	for engine := range engineKinds {
		if engine == "other" {
			continue
		}
		m.engineReachable[engine] = new(atomic.Int64)
	}
	if !m.metricsEnabled {
		return m, nil
	}
	var err error
	if m.httpRequests, err = meter.Int64Counter("simpledns.http.server.requests", metric.WithDescription("HTTP requests received by the service")); err != nil {
		return nil, instrumentError("HTTP request counter", err)
	}
	if m.httpDuration, err = meter.Float64Histogram("simpledns.http.server.duration", metric.WithDescription("HTTP request duration"), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, instrumentError("HTTP duration histogram", err)
	}
	if m.authFailures, err = meter.Int64Counter("simpledns.http.auth.failures", metric.WithDescription("Rejected bearer authentication attempts")); err != nil {
		return nil, instrumentError("authentication failure counter", err)
	}
	if m.rpcRequests, err = meter.Int64Counter("simpledns.rpc.server.requests", metric.WithDescription("Connect RPC requests completed by the service")); err != nil {
		return nil, instrumentError("RPC request counter", err)
	}
	if m.rpcDuration, err = meter.Float64Histogram("simpledns.rpc.server.duration", metric.WithDescription("Connect RPC request duration"), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, instrumentError("RPC duration histogram", err)
	}
	if m.controlMutations, err = meter.Int64Counter("simpledns.control.mutations", metric.WithDescription("Control-plane zone and record mutations")); err != nil {
		return nil, instrumentError("control mutation counter", err)
	}
	if m.controlDuration, err = meter.Float64Histogram("simpledns.control.mutation.duration", metric.WithDescription("Control-plane mutation duration"), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, instrumentError("control mutation duration histogram", err)
	}
	if m.dnsQueries, err = meter.Int64Counter("simpledns.dns.queries", metric.WithDescription("Authoritative DNS queries completed")); err != nil {
		return nil, instrumentError("DNS query counter", err)
	}
	if m.dnsDuration, err = meter.Float64Histogram("simpledns.dns.query.duration", metric.WithDescription("Authoritative DNS query duration"), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, instrumentError("DNS query duration histogram", err)
	}
	if m.dnsResponseSize, err = meter.Int64Histogram("simpledns.dns.response.size", metric.WithDescription("Encoded DNS response size before transport framing"), metric.WithUnit("By"), metric.WithExplicitBucketBoundaries(sizeBuckets...)); err != nil {
		return nil, instrumentError("DNS response size histogram", err)
	}
	if m.dnsTruncated, err = meter.Int64Counter("simpledns.dns.responses.truncated", metric.WithDescription("Truncated authoritative DNS responses")); err != nil {
		return nil, instrumentError("DNS truncation counter", err)
	}
	if m.dnsWriteFailures, err = meter.Int64Counter("simpledns.dns.write.failures", metric.WithDescription("Failures writing authoritative DNS responses")); err != nil {
		return nil, instrumentError("DNS write failure counter", err)
	}
	if m.snapshotBuilds, err = meter.Int64Counter("simpledns.snapshot.builds", metric.WithDescription("Control-plane snapshot build attempts")); err != nil {
		return nil, instrumentError("snapshot build counter", err)
	}
	if m.snapshotBuildTime, err = meter.Float64Histogram("simpledns.snapshot.build.duration", metric.WithDescription("Control-plane snapshot build duration"), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, instrumentError("snapshot build duration histogram", err)
	}
	if m.snapshotSize, err = meter.Int64Histogram("simpledns.snapshot.size", metric.WithDescription("Snapshot payload size"), metric.WithUnit("By"), metric.WithExplicitBucketBoundaries(sizeBuckets...)); err != nil {
		return nil, instrumentError("snapshot size histogram", err)
	}
	if m.snapshotFetches, err = meter.Int64Counter("simpledns.snapshot.fetches", metric.WithDescription("Authority snapshot fetch attempts")); err != nil {
		return nil, instrumentError("snapshot fetch counter", err)
	}
	if m.snapshotFetchTime, err = meter.Float64Histogram("simpledns.snapshot.fetch.duration", metric.WithDescription("Authority snapshot fetch duration"), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, instrumentError("snapshot fetch duration histogram", err)
	}
	if m.snapshotApplies, err = meter.Int64Counter("simpledns.snapshot.applies", metric.WithDescription("Authority snapshot validation and apply attempts")); err != nil {
		return nil, instrumentError("snapshot apply counter", err)
	}
	if m.snapshotApplyTime, err = meter.Float64Histogram("simpledns.snapshot.apply.duration", metric.WithDescription("Authority snapshot validation and apply duration"), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, instrumentError("snapshot apply duration histogram", err)
	}
	if m.snapshotAdmissions, err = meter.Int64Counter("simpledns.snapshot.admissions", metric.WithDescription("Aggregate control-plane snapshot admission decisions")); err != nil {
		return nil, instrumentError("snapshot admission counter", err)
	}
	if m.snapshotAdmitTime, err = meter.Float64Histogram("simpledns.snapshot.admission.duration", metric.WithDescription("Aggregate snapshot admission duration"), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, instrumentError("snapshot admission duration histogram", err)
	}
	if m.snapshotCache, err = meter.Int64Counter("simpledns.snapshot.cache.operations", metric.WithDescription("Authority snapshot cache operations")); err != nil {
		return nil, instrumentError("snapshot cache operation counter", err)
	}
	if m.snapshotCacheTime, err = meter.Float64Histogram("simpledns.snapshot.cache.operation.duration", metric.WithDescription("Authority snapshot cache operation duration"), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, instrumentError("snapshot cache duration histogram", err)
	}
	if m.checksumFailures, err = meter.Int64Counter("simpledns.snapshot.checksum.failures", metric.WithDescription("Snapshots rejected because of an invalid checksum")); err != nil {
		return nil, instrumentError("snapshot checksum failure counter", err)
	}
	if m.readinessChecks, err = meter.Int64Counter("simpledns.readiness.checks", metric.WithDescription("Readiness probe decisions")); err != nil {
		return nil, instrumentError("readiness check counter", err)
	}
	if m.storeTransactions, err = meter.Int64Counter("simpledns.store.transactions", metric.WithDescription("bbolt transactions completed by the zone store")); err != nil {
		return nil, instrumentError("store transaction counter", err)
	}
	if m.storeDuration, err = meter.Float64Histogram("simpledns.store.transaction.duration", metric.WithDescription("bbolt transaction duration"), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, instrumentError("store transaction duration histogram", err)
	}
	if m.authorityCompiles, err = meter.Int64Counter("simpledns.authoritative.snapshot.compiles", metric.WithDescription("Authoritative in-memory snapshot compilation attempts")); err != nil {
		return nil, instrumentError("authoritative compile counter", err)
	}
	if m.authorityCompile, err = meter.Float64Histogram("simpledns.authoritative.snapshot.compile.duration", metric.WithDescription("Authoritative in-memory snapshot compilation duration"), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, instrumentError("authoritative compile duration histogram", err)
	}
	if m.authorityPublishes, err = meter.Int64Counter("simpledns.authoritative.snapshot.publishes", metric.WithDescription("Atomic authoritative snapshot publications")); err != nil {
		return nil, instrumentError("authoritative publish counter", err)
	}
	if m.engineRequests, err = meter.Int64Counter("simpledns.engine.requests", metric.WithDescription("Calls the facades made to a hosting, mail or billing engine")); err != nil {
		return nil, instrumentError("engine request counter", err)
	}
	if m.engineDuration, err = meter.Float64Histogram("simpledns.engine.request.duration", metric.WithDescription("Engine call duration"), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, instrumentError("engine request duration histogram", err)
	}
	if m.gatewayResolutions, err = meter.Int64Counter("simpledns.hosting.gateway_resolution", metric.WithDescription("Gateway hostname resolution outcomes")); err != nil {
		return nil, instrumentError("gateway resolution counter", err)
	}
	if m.billingWebhooks, err = meter.Int64Counter("simpledns.billing.webhooks", metric.WithDescription("Stripe webhook deliveries by outcome")); err != nil {
		return nil, instrumentError("billing webhook counter", err)
	}
	if m.billingReconciles, err = meter.Int64Counter("simpledns.billing.reconcile", metric.WithDescription("Billing reconciliation passes")); err != nil {
		return nil, instrumentError("billing reconcile counter", err)
	}
	if m.hostingUploads, err = meter.Int64Counter("simpledns.hosting.uploads", metric.WithDescription("Folder upload outcomes")); err != nil {
		return nil, instrumentError("hosting upload counter", err)
	}
	if m.activityEvents, err = meter.Int64Counter("simpledns.activity.events", metric.WithDescription("Activity events recorded")); err != nil {
		return nil, instrumentError("activity event counter", err)
	}
	if m.activityDropped, err = meter.Int64Counter("simpledns.activity.dropped", metric.WithDescription("Activity events that could not be stored")); err != nil {
		return nil, instrumentError("activity drop counter", err)
	}
	m.dnsOptions = make(map[dnsLabels]dnsMeasurementOptions, len(transports)*len(dnsTypes)*len(dnsResponseCodes))
	for transport := range transports {
		for questionType := range dnsTypes {
			for responseCode := range dnsResponseCodes {
				labels := dnsLabels{transport: transport, questionType: questionType, responseCode: responseCode}
				option := metric.WithAttributeSet(attribute.NewSet(
					attribute.String("network.transport", transport),
					attribute.String("dns.question.type", questionType),
					attribute.String("dns.response.code", responseCode),
				))
				m.dnsOptions[labels] = dnsMeasurementOptions{
					add:    []metric.AddOption{option},
					record: []metric.RecordOption{option},
				}
			}
		}
	}
	m.transportOptions = make(map[string][]metric.AddOption, len(transports))
	for transport := range transports {
		m.transportOptions[transport] = []metric.AddOption{
			metric.WithAttributeSet(attribute.NewSet(attribute.String("network.transport", transport))),
		}
	}

	if _, err = meter.Int64ObservableGauge(
		"simpledns.inventory.zones",
		metric.WithDescription("Zones in the active snapshot"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
			observer.Observe(m.inventoryZones.Load())
			return nil
		}),
	); err != nil {
		return nil, instrumentError("zone inventory gauge", err)
	}
	if _, err = meter.Int64ObservableGauge(
		"simpledns.inventory.records",
		metric.WithDescription("Records in the active snapshot, including managed records"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
			observer.Observe(m.inventoryRecords.Load())
			return nil
		}),
	); err != nil {
		return nil, instrumentError("record inventory gauge", err)
	}
	if _, err = meter.Int64ObservableGauge(
		"simpledns.snapshot.loaded",
		metric.WithDescription("Whether the authority has loaded a valid snapshot"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
			if m.snapshotLoaded.Load() {
				observer.Observe(1)
			} else {
				observer.Observe(0)
			}
			return nil
		}),
	); err != nil {
		return nil, instrumentError("snapshot loaded gauge", err)
	}
	if _, err = meter.Int64ObservableGauge(
		"simpledns.snapshot.ready",
		metric.WithDescription("Whether the authority snapshot is within its staleness budget"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
			lastValid := m.snapshotLastValidNS.Load()
			maxStaleness := m.snapshotMaxStaleNS.Load()
			if m.snapshotLoaded.Load() && maxStaleness > 0 && m.snapshotAge(lastValid) <= time.Duration(maxStaleness) {
				observer.Observe(1)
			} else {
				observer.Observe(0)
			}
			return nil
		}),
	); err != nil {
		return nil, instrumentError("snapshot ready gauge", err)
	}
	if _, err = meter.Float64ObservableGauge(
		"simpledns.snapshot.age",
		metric.WithDescription("Time since the last successfully validated snapshot response or cache"),
		metric.WithUnit("s"),
		metric.WithFloat64Callback(func(_ context.Context, observer metric.Float64Observer) error {
			if lastValid := m.snapshotLastValidNS.Load(); m.snapshotLoaded.Load() {
				observer.Observe(m.snapshotAge(lastValid).Seconds())
			}
			return nil
		}),
	); err != nil {
		return nil, instrumentError("snapshot age gauge", err)
	}
	engineGaugeOptions := make(map[string]metric.ObserveOption, len(m.engineReachable))
	for engine := range m.engineReachable {
		engineGaugeOptions[engine] = metric.WithAttributes(attribute.String("engine", engine))
	}
	if _, err = meter.Int64ObservableGauge(
		"simpledns.engine.reachable",
		metric.WithDescription("Whether the last probe of each engine succeeded (0 when unconfigured)"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
			for engine, state := range m.engineReachable {
				observer.Observe(state.Load(), engineGaugeOptions[engine])
			}
			return nil
		}),
	); err != nil {
		return nil, instrumentError("engine reachability gauge", err)
	}
	return m, nil
}

func instrumentError(name string, err error) error {
	return fmt.Errorf("create %s: %w", name, err)
}

var disabledMetrics = func() *Metrics {
	result, err := NewMetrics(noop.NewMeterProvider().Meter(instrumentationScope), withSignals(false, false))
	if err != nil {
		panic(err)
	}
	return result
}()

func Disabled() *Metrics {
	return disabledMetrics
}

func Select(values ...*Metrics) *Metrics {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return Disabled()
}

func (m *Metrics) MetricsEnabled() bool {
	return m != nil && m.metricsEnabled
}

func (m *Metrics) TracesEnabled() bool {
	return m != nil && m.tracesEnabled
}

func (m *Metrics) HTTPRequest(ctx context.Context, method, route string, status int, elapsed time.Duration) {
	if !m.MetricsEnabled() {
		return
	}
	attributes := metric.WithAttributes(
		attribute.String("http.request.method", allowed(method, httpMethods, "OTHER")),
		attribute.String("http.route", allowed(route, httpRoutes, "other")),
		attribute.Int("http.response.status_code", boundedHTTPStatus(status)),
	)
	m.httpRequests.Add(ctx, 1, attributes)
	m.httpDuration.Record(ctx, elapsed.Seconds(), attributes)
}

func (m *Metrics) AuthFailure(ctx context.Context, audience, reason string) {
	if !m.MetricsEnabled() {
		return
	}
	m.authFailures.Add(ctx, 1, metric.WithAttributes(
		attribute.String("auth.audience", allowed(audience, authAudiences, "other")),
		attribute.String("auth.reason", allowed(reason, authReasons, "invalid")),
	))
}

// RPCRequest records one completed Connect call. The service is bounded by the
// list of services this binary actually mounts, so a request to an unknown
// procedure cannot introduce a new attribute value.
func (m *Metrics) RPCRequest(ctx context.Context, service, method, code string, elapsed time.Duration) {
	if !m.MetricsEnabled() {
		return
	}
	attributes := metric.WithAttributes(
		attribute.String("rpc.system", "connect_rpc"),
		attribute.String("rpc.service", allowed(service, rpcServices, "unknown")),
		attribute.String("rpc.method", allowed(method, rpcMethods, "unknown")),
		attribute.String("rpc.connect.status_code", allowed(code, connectCodes, "unknown")),
	)
	m.rpcRequests.Add(ctx, 1, attributes)
	m.rpcDuration.Record(ctx, elapsed.Seconds(), attributes)
}

// EngineRequest records one call a facade made to its engine. operation is a
// short verb from the facade's own vocabulary, never an engine URL or object id.
func (m *Metrics) EngineRequest(ctx context.Context, engine, operation, outcome, errorType string, elapsed time.Duration) {
	if !m.MetricsEnabled() {
		return
	}
	attributes := metric.WithAttributes(
		attribute.String("engine", allowed(engine, engineKinds, "other")),
		attribute.String("operation", allowed(operation, engineOperations, "other")),
		attribute.String("outcome", allowed(outcome, outcomes, "error")),
		attribute.String("error.type", allowed(errorType, errorTypes, "other")),
	)
	m.engineRequests.Add(ctx, 1, attributes)
	m.engineDuration.Record(ctx, elapsed.Seconds(), attributes)
}

// SetEngineReachable publishes the prober's verdict for one engine. An engine
// that is not configured stays at 0, which is honest: nothing answered.
func (m *Metrics) SetEngineReachable(engine string, reachable bool) {
	if m == nil {
		return
	}
	state, ok := m.engineReachable[allowed(engine, engineKinds, "other")]
	if !ok {
		return
	}
	if reachable {
		state.Store(1)
		return
	}
	state.Store(0)
}

// GatewayResolution records one pass of the gateway hostname reconciler.
func (m *Metrics) GatewayResolution(ctx context.Context, outcome string) {
	if !m.MetricsEnabled() {
		return
	}
	m.gatewayResolutions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", allowed(outcome, gatewayOutcomes, "error")),
	))
}

// BillingWebhook records one delivery attempt. secretIndex is the position of
// the signing secret that verified the payload, or -1 when none did; the secret
// itself never reaches telemetry.
func (m *Metrics) BillingWebhook(ctx context.Context, outcome, eventType string, secretIndex int) {
	if !m.MetricsEnabled() {
		return
	}
	if secretIndex < 0 || secretIndex > 2 {
		secretIndex = 0
	}
	m.billingWebhooks.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", allowed(outcome, webhookOutcomes, "processing_error")),
		attribute.String("event.type", allowed(eventType, webhookEventTypes, "other")),
		attribute.Int("secret", secretIndex),
	))
}

// BillingReconcile records one reconciliation of a subscription row.
func (m *Metrics) BillingReconcile(ctx context.Context, outcome string) {
	if !m.MetricsEnabled() {
		return
	}
	m.billingReconciles.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", allowed(outcome, reconcileOutcomes, "error")),
	))
}

// HostingUpload records one folder upload request.
func (m *Metrics) HostingUpload(ctx context.Context, outcome string) {
	if !m.MetricsEnabled() {
		return
	}
	m.hostingUploads.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", allowed(outcome, uploadOutcomes, "invalid_path")),
	))
}

// ActivityEvent counts one recorded event. kind is bounded by the activity
// allowlist, which this list mirrors; an unknown kind is folded to "other" by
// the recorder before it gets here and again here.
func (m *Metrics) ActivityEvent(ctx context.Context, kind, severity string) {
	if !m.MetricsEnabled() {
		return
	}
	m.activityEvents.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", allowed(kind, activityKinds, "other")),
		attribute.String("severity", allowed(severity, activitySeverities, "info")),
	))
}

// ActivityDropped counts an event that could not be stored. Losing an audit
// line never fails the mutation that produced it, so this counter is the only
// signal that it happened.
func (m *Metrics) ActivityDropped(ctx context.Context, reason string) {
	if !m.MetricsEnabled() {
		return
	}
	m.activityDropped.Add(ctx, 1, metric.WithAttributes(
		attribute.String("reason", allowed(reason, activityDropReasons, "other")),
	))
}

func (m *Metrics) ControlMutation(ctx context.Context, entity, operation, outcome, errorType string, elapsed time.Duration) {
	if !m.MetricsEnabled() {
		return
	}
	attributes := metric.WithAttributes(
		attribute.String("entity", allowed(entity, entities, "other")),
		attribute.String("operation", allowed(operation, mutationOperations, "other")),
		attribute.String("outcome", allowed(outcome, outcomes, "error")),
		attribute.String("error.type", allowed(errorType, errorTypes, "other")),
	)
	m.controlMutations.Add(ctx, 1, attributes)
	m.controlDuration.Record(ctx, elapsed.Seconds(), attributes)
}

func (m *Metrics) DNSQuery(ctx context.Context, transport, questionType, responseCode string, elapsed time.Duration, responseBytes int, truncated bool) {
	if !m.MetricsEnabled() {
		return
	}
	labels := dnsLabels{
		transport:    allowed(transport, transports, "other"),
		questionType: allowed(questionType, dnsTypes, "OTHER"),
		responseCode: allowed(responseCode, dnsResponseCodes, "OTHER"),
	}
	attributes := m.dnsOptions[labels]
	m.dnsQueries.Add(ctx, 1, attributes.add...)
	m.dnsDuration.Record(ctx, elapsed.Seconds(), attributes.record...)
	m.dnsResponseSize.Record(ctx, int64(max(responseBytes, 0)), attributes.record...)
	if truncated {
		m.dnsTruncated.Add(ctx, 1, attributes.add...)
	}
}

func (m *Metrics) DNSWriteFailure(ctx context.Context, transport string) {
	if !m.MetricsEnabled() {
		return
	}
	m.dnsWriteFailures.Add(ctx, 1, m.transportOptions[allowed(transport, transports, "other")]...)
}

func (m *Metrics) SnapshotBuild(ctx context.Context, outcome, cacheResult string, elapsed time.Duration, size int) {
	if !m.MetricsEnabled() {
		return
	}
	attributes := metric.WithAttributes(
		attribute.String("outcome", allowed(outcome, snapshotBuildOutcomes, "build_error")),
		attribute.String("cache.result", allowed(cacheResult, cacheResults, "none")),
	)
	m.snapshotBuilds.Add(ctx, 1, attributes)
	m.snapshotBuildTime.Record(ctx, elapsed.Seconds(), attributes)
	if size >= 0 {
		m.snapshotSize.Record(ctx, int64(size), metric.WithAttributes(attribute.String("operation", "build")))
	}
}

func (m *Metrics) SnapshotFetch(ctx context.Context, outcome string, elapsed time.Duration, size int) {
	if !m.MetricsEnabled() {
		return
	}
	attributes := metric.WithAttributes(attribute.String("outcome", allowed(outcome, snapshotFetchOutcomes, "apply_error")))
	m.snapshotFetches.Add(ctx, 1, attributes)
	m.snapshotFetchTime.Record(ctx, elapsed.Seconds(), attributes)
	if size >= 0 {
		m.snapshotSize.Record(ctx, int64(size), metric.WithAttributes(attribute.String("operation", "fetch")))
	}
}

func (m *Metrics) SnapshotApply(ctx context.Context, source, outcome string, elapsed time.Duration) {
	if !m.MetricsEnabled() {
		return
	}
	attributes := metric.WithAttributes(
		attribute.String("source", allowed(source, snapshotSources, "other")),
		attribute.String("outcome", allowed(outcome, snapshotApplyOutcomes, "other")),
	)
	m.snapshotApplies.Add(ctx, 1, attributes)
	m.snapshotApplyTime.Record(ctx, elapsed.Seconds(), attributes)
	if outcome == "checksum_error" {
		m.checksumFailures.Add(ctx, 1)
	}
}

func (m *Metrics) SnapshotAdmission(ctx context.Context, outcome string, elapsed time.Duration) {
	if !m.MetricsEnabled() {
		return
	}
	attributes := metric.WithAttributes(attribute.String("outcome", allowed(outcome, outcomes, "error")))
	m.snapshotAdmissions.Add(ctx, 1, attributes)
	m.snapshotAdmitTime.Record(ctx, elapsed.Seconds(), attributes)
}

func (m *Metrics) SnapshotCache(ctx context.Context, operation, outcome string, elapsed time.Duration, size int) {
	if !m.MetricsEnabled() {
		return
	}
	attributes := metric.WithAttributes(
		attribute.String("operation", allowed(operation, cacheOperations, "other")),
		attribute.String("outcome", allowed(outcome, outcomes, "error")),
	)
	m.snapshotCache.Add(ctx, 1, attributes)
	m.snapshotCacheTime.Record(ctx, elapsed.Seconds(), attributes)
	if size >= 0 {
		m.snapshotSize.Record(ctx, int64(size), metric.WithAttributes(attribute.String("operation", allowed(operation, cacheOperations, "other"))))
	}
}

func (m *Metrics) Readiness(ctx context.Context, ready bool, reason string) {
	if !m.MetricsEnabled() {
		return
	}
	outcome := "not_ready"
	if ready {
		outcome = "ready"
	}
	m.readinessChecks.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", outcome),
		attribute.String("reason", readinessReason(reason, ready)),
	))
}

func (m *Metrics) StoreTransaction(ctx context.Context, operation, transactionType, outcome string, elapsed time.Duration) {
	if !m.MetricsEnabled() {
		return
	}
	attributes := metric.WithAttributes(
		attribute.String("db.operation", allowed(operation, storeOperations, "other")),
		attribute.String("db.transaction.type", allowed(transactionType, transactionTypes, "other")),
		attribute.String("outcome", allowed(outcome, outcomes, "error")),
	)
	m.storeTransactions.Add(ctx, 1, attributes)
	m.storeDuration.Record(ctx, elapsed.Seconds(), attributes)
}

func (m *Metrics) AuthoritativeCompile(ctx context.Context, outcome string, elapsed time.Duration) {
	if !m.MetricsEnabled() {
		return
	}
	attributes := metric.WithAttributes(attribute.String("outcome", allowed(outcome, outcomes, "error")))
	m.authorityCompiles.Add(ctx, 1, attributes)
	m.authorityCompile.Record(ctx, elapsed.Seconds(), attributes)
}

func (m *Metrics) AuthoritativePublish(ctx context.Context) {
	if !m.MetricsEnabled() {
		return
	}
	m.authorityPublishes.Add(ctx, 1)
}

func (m *Metrics) SetInventory(zones, records uint32) {
	m.inventoryZones.Store(int64(zones))
	m.inventoryRecords.Store(int64(records))
}

func (m *Metrics) SetSnapshotStatus(lastValid time.Time, maxStaleness time.Duration) {
	m.snapshotLastValidNS.Store(lastValid.UTC().UnixNano())
	m.snapshotMaxStaleNS.Store(int64(maxStaleness))
	m.snapshotLoaded.Store(true)
}

func (m *Metrics) snapshotAge(lastValidNS int64) time.Duration {
	age := m.clock().UTC().Sub(time.Unix(0, lastValidNS).UTC())
	if age < 0 {
		return 0
	}
	return age
}

func boundedHTTPStatus(status int) int {
	if status < 100 || status > 599 {
		return 0
	}
	return status
}

func readinessReason(reason string, ready bool) string {
	if ready {
		return "ready"
	}
	switch reason {
	case "no valid snapshot":
		return "no_valid_snapshot"
	case "snapshot is stale":
		return "stale"
	default:
		return "other"
	}
}

func allowed(value string, values map[string]struct{}, fallback string) string {
	if _, ok := values[value]; ok {
		return value
	}
	return fallback
}

func choices(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

// procedures is every Connect procedure this binary mounts. It is the single
// source for the route and method allowlists, and routes_test.go asserts it
// covers every generated procedure constant outside gen/go/deephost (whose
// procedures are client-only and are never served here).
var procedures = []string{
	"/dns.v1.DNSService/ListZones",
	"/dns.v1.DNSService/CreateZone",
	"/dns.v1.DNSService/DeleteZone",
	"/dns.v1.DNSService/CreateRecord",
	"/dns.v1.DNSService/UpdateRecord",
	"/dns.v1.DNSService/DeleteRecord",
	"/dns.v1.DNSService/ImportZone",
	"/dns.v1.DNSService/ExportZone",

	"/platform.v1.PlatformService/GetPlatformStatus",
	"/platform.v1.PlatformService/RebuildPlatformStore",

	"/hosting.v1.HostingService/GetHostingStatus",
	"/hosting.v1.HostingService/ListSites",
	"/hosting.v1.HostingService/GetSite",
	"/hosting.v1.HostingService/AttachSite",
	"/hosting.v1.HostingService/UpdateSite",
	"/hosting.v1.HostingService/DetachSite",
	"/hosting.v1.HostingService/ReapplySiteDns",
	"/hosting.v1.HostingService/CreateDeploy",
	"/hosting.v1.HostingService/ListDeploys",
	"/hosting.v1.HostingService/GetDeploy",
	"/hosting.v1.HostingService/GetDeployLog",
	"/hosting.v1.HostingService/RollbackSite",
	"/hosting.v1.HostingService/ConfirmGatewayAddresses",
	"/hosting.v1.HostingService/SetSiteEnvVar",
	"/hosting.v1.HostingService/DeleteSiteEnvVar",
	"/hosting.v1.HostingService/ListSiteEnvVars",

	"/mail.v1.MailService/GetMailStatus",
	"/mail.v1.MailService/ListMailDomains",
	"/mail.v1.MailService/GetMailDomain",
	"/mail.v1.MailService/BindMailDomain",
	"/mail.v1.MailService/UpdateMailDomain",
	"/mail.v1.MailService/UnbindMailDomain",
	"/mail.v1.MailService/ReapplyMailDns",
	"/mail.v1.MailService/ListMailboxes",
	"/mail.v1.MailService/CreateMailbox",
	"/mail.v1.MailService/UpdateMailbox",
	"/mail.v1.MailService/ResetMailboxPassword",
	"/mail.v1.MailService/DeleteMailbox",
	"/mail.v1.MailService/ListForwarders",
	"/mail.v1.MailService/CreateForwarder",
	"/mail.v1.MailService/DeleteForwarder",
	"/mail.v1.MailService/GetMailQueue",

	"/billing.v1.BillingService/GetBillingStatus",
	"/billing.v1.BillingService/GetBillingSummary",
	"/billing.v1.BillingService/CreateCheckoutSession",
	"/billing.v1.BillingService/ConfirmCheckout",
	"/billing.v1.BillingService/CreatePortalSession",
	"/billing.v1.BillingService/ListInvoices",
	"/billing.v1.BillingService/RetryDeadWebhooks",

	"/activity.v1.ActivityService/ListEvents",
	"/activity.v1.ActivityService/GetEvent",
}

// rawRoutes are the non-Connect HTTP routes the control plane serves.
var rawRoutes = []string{
	"/",
	"/healthz",
	"/readyz",
	"/internal/v1/snapshot",
	"/internal/v1/platform-backup",
	"/public/v1/plan",
	"/hosting/v1/uploads",
	"/billing/v1/stripe/webhook",
}

func routeChoices() map[string]struct{} {
	values := make([]string, 0, len(procedures)+len(rawRoutes)+1)
	values = append(values, rawRoutes...)
	values = append(values, procedures...)
	return choices(append(values, "other")...)
}

func serviceChoices() map[string]struct{} {
	result := choices("unknown")
	for _, procedure := range procedures {
		service, _, ok := splitProcedure(procedure)
		if ok {
			result[service] = struct{}{}
		}
	}
	return result
}

func methodChoices() map[string]struct{} {
	result := choices("unknown")
	for _, procedure := range procedures {
		_, method, ok := splitProcedure(procedure)
		if ok {
			result[method] = struct{}{}
		}
	}
	return result
}

var (
	httpMethods           = choices("GET", "POST", "OPTIONS", "HEAD", "PUT", "DELETE", "PATCH")
	httpRoutes            = routeChoices()
	authAudiences         = choices("api", "snapshot", "webhook", "other")
	authReasons           = choices("missing", "multiple", "malformed", "invalid")
	rpcServices           = serviceChoices()
	rpcMethods            = methodChoices()
	connectCodes          = choices("ok", "canceled", "unknown", "invalid_argument", "deadline_exceeded", "not_found", "already_exists", "permission_denied", "resource_exhausted", "failed_precondition", "aborted", "out_of_range", "unimplemented", "internal", "unavailable", "data_loss", "unauthenticated")
	entities              = choices("zone", "record", "site", "deploy", "mail_domain", "mailbox", "forwarder", "subscription", "other")
	mutationOperations    = choices("create", "update", "delete", "import", "engine_apply", "engine_remove", "attach", "detach", "bind", "unbind", "rollback", "reset", "rebuild", "other")
	outcomes              = choices("success", "error")
	errorTypes            = choices("none", "canceled", "unknown", "invalid_argument", "deadline_exceeded", "not_found", "already_exists", "permission_denied", "resource_exhausted", "failed_precondition", "aborted", "out_of_range", "unimplemented", "internal", "unavailable", "data_loss", "unauthenticated", "other")
	transports            = choices("udp", "tcp", "other")
	dnsTypes              = choices("A", "AAAA", "CAA", "CNAME", "DNSKEY", "DS", "HINFO", "MX", "NS", "RRSIG", "SOA", "SRV", "TXT", "ANY", "OTHER")
	dnsResponseCodes      = choices("NOERROR", "FORMERR", "SERVFAIL", "NXDOMAIN", "NOTIMP", "REFUSED", "BADVERS", "OTHER")
	snapshotBuildOutcomes = choices("success", "list_error", "build_error", "oversize")
	cacheResults          = choices("hit", "miss", "none")
	snapshotFetchOutcomes = choices("success", "not_modified", "request_error", "transport_error", "http_error", "read_error", "apply_error")
	snapshotSources       = choices("cache", "fetch", "other")
	snapshotApplyOutcomes = choices("success", "decode_error", "checksum_error", "clock_error", "rollback_error", "nameserver_error", "compile_error", "cache_write_error", "other")
	cacheOperations       = choices("load", "write", "other")
	transactionTypes      = choices("read", "write", "other")
	storeOperations       = choices("initialize", "validate_admission", "initialize_restore", "list_zones", "get_zone", "create_zone", "delete_zone", "import_zone", "restore_snapshot", "create_record", "update_record", "delete_record", "apply_record_set", "platform_read", "platform_write", "platform_backup", "other")

	engineKinds      = choices("dns", "hosting", "mail", "billing", "other")
	engineOperations = choices(
		"health", "probe", "create_app", "update_app", "get_app", "list_apps", "delete_app",
		"create_git_build", "deploy", "get_build", "list_builds", "get_build_logs",
		"list_releases", "promote_release", "create_domain", "list_domains", "delete_domain",
		"set_app_env_var", "delete_app_env_var", "list_app_env_vars",
		"account", "system_settings", "find_domain", "create_mail_domain", "get_mail_domain",
		"update_mail_domain", "destroy_mail_domain", "list_mail_domains", "list_dkim", "destroy_dkim",
		"list_accounts", "create_account", "update_account", "destroy_account",
		"list_lists", "create_list", "destroy_list", "query_queue",
		"get_price", "find_customer", "ensure_customer", "create_checkout", "get_checkout",
		"get_subscription", "list_subscriptions", "create_portal", "list_invoices",
		"other",
	)
	gatewayOutcomes   = choices("success", "unchanged", "changed", "pending", "filtered", "rejected", "error", "empty")
	reconcileOutcomes = choices("success", "unchanged", "changed", "skipped", "error")
	uploadOutcomes    = choices("accepted", "too_many_files", "too_large", "no_space", "invalid_path", "expired")
	webhookOutcomes   = choices(
		"accepted", "duplicate", "bad_signature", "too_old", "not_signed", "invalid_header",
		"api_version_mismatch", "rate_limited", "oversize", "origin_rejected", "unsupported_media",
		"processing_error",
	)
	webhookEventTypes = choices(
		"checkout.session.completed", "checkout.session.expired",
		"customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted",
		"invoice.paid", "invoice.payment_failed", "customer.updated", "payment_method.attached",
		"none", "other",
	)
	activitySeverities  = choices("info", "warn", "error")
	activityDropReasons = choices("no_store", "store_error", "other")
	// activityKinds mirrors internal/activity's allowlist. It is duplicated
	// rather than imported because activity depends on telemetry, never the
	// other way round; TestActivityKindsAreBounded in internal/activity keeps
	// the two in step.
	activityKinds = choices(
		"zone.created", "zone.deleted", "zone.imported",
		"dns.record.created", "dns.record.updated", "dns.record.deleted", "dns.records.rewritten",
		"dns.engine.restored", "dns.engine.orphaned",
		"site.attached", "site.ready", "site.updated", "site.detached", "site.detach.partial",
		"site.dns.reapplied", "site.hostname.registered", "site.hostname.ready", "site.zone_missing",
		"site.www_mode.changed", "site.env.set", "site.env.deleted",
		"deploy.requested", "deploy.building", "deploy.live", "deploy.superseded", "deploy.failed",
		"deploy.abandoned", "deploy.lost", "deploy.rollback", "deploy.recovered",
		"hosting.gateway.changed", "hosting.gateway.pending", "hosting.gateway.confirmed",
		"hosting.gateway.rejected", "hosting.gateway.resolve_failed",
		"mail.domain.bound", "mail.domain.updated", "mail.domain.unbound", "mail.records.changed",
		"mail.records.invalid", "mail.dns.reapplied", "mail.zone_missing",
		"mail.mailbox.created", "mail.mailbox.updated", "mail.mailbox.password_reset", "mail.mailbox.deleted",
		"mail.forwarder.created", "mail.forwarder.deleted",
		"billing.checkout.started", "billing.checkout.completed", "billing.checkout.expired",
		"billing.subscription.updated", "billing.subscription.canceled",
		"billing.invoice.paid", "billing.invoice.failed", "billing.event.ignored",
		"billing.webhook.rejected", "billing.webhook.dead", "billing.webhook.retried", "billing.reconciled",
		"engine.unreachable", "engine.recovered",
		"platform.started", "platform.rebuilt",
		"other",
	)
)

// ActivityKinds returns the bounded kind allowlist. internal/activity's test
// asserts its own constants are a subset of this set, so a new kind cannot ship
// without a bounded metric attribute.
func ActivityKinds() []string {
	result := make([]string, 0, len(activityKinds))
	for kind := range activityKinds {
		result = append(result, kind)
	}
	slices.Sort(result)
	return result
}

type dnsLabels struct {
	transport    string
	questionType string
	responseCode string
}

type dnsMeasurementOptions struct {
	add    []metric.AddOption
	record []metric.RecordOption
}
