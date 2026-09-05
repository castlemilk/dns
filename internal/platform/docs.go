// Package platform owns the durable state the engine facades need but the DNS
// zone store must never hold: which zones have a site or a mail binding, the
// deploy history, subscriptions, the webhook queue and the activity log. It
// lives in its own bbolt file so the zone store's bucket invariants, the
// snapshot feed and dns-restore stay exactly as they were in phase 1.
//
// It depends on internal/activity (whose EventStore it implements),
// internal/enginedns, internal/zone, internal/config and internal/telemetry —
// never on the hosting, mail or billing facades, which it receives as Probe and
// Rebuilder values.
package platform

import "time"

// DocVersion is stamped on every stored document. Readers ignore unknown
// fields, so adding a field is a no-op migration; a higher version than this
// binary understands refuses to open the store.
const DocVersion = 1

// SiteDoc is one attached website, keyed by zone id. The row is written before
// the first engine call and before the first DNS write, so a crash always
// leaves something for the reconcilers to finish or release.
type SiteDoc struct {
	V            int    `json:"v"`
	ZoneID       string `json:"zone_id"`
	ZoneName     string `json:"zone_name"`
	App          string `json:"app"`
	AppRef       string `json:"app_ref"`
	Framework    string `json:"framework"` // static | node | nextjs
	Repository   string `json:"repository,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Path         string `json:"path,omitempty"`
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	WWW          bool   `json:"www"`
	AutoHostname string `json:"auto_hostname,omitempty"`
	// WWWMode is what www.<zone> should do: "serve" or "redirect". Empty means
	// serve, so every row written before this field existed keeps behaving the
	// way it always has.
	WWWMode string `json:"www_mode,omitempty"`

	Hostnames     []HostnameDoc `json:"hostnames,omitempty"`
	RecordIDs     []string      `json:"record_ids,omitempty"`
	ApexAddresses []string      `json:"apex_addresses,omitempty"`
	DNSInSync     bool          `json:"dns_in_sync"`
	DNSProblems   []string      `json:"dns_problems,omitempty"`

	LiveDeployID   string `json:"live_deploy_id,omitempty"`
	LatestDeployID string `json:"latest_deploy_id,omitempty"`
	ZoneMissing    bool   `json:"zone_missing,omitempty"`

	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	ReconciledAt    time.Time  `json:"reconciled_at,omitzero"`
	DetachStartedAt *time.Time `json:"detach_started_at,omitempty"`
}

// HostnameDoc is one host registered with the hosting engine for a site.
type HostnameDoc struct {
	Host       string    `json:"host"`
	Role       string    `json:"role"` // apex | www | auto
	State      string    `json:"state"`
	TLSMode    string    `json:"tls_mode,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
	Retrying   bool      `json:"retrying"`
	Failures   int       `json:"failures,omitempty"`
	// RedirectTo is the redirect the engine last reported for this host, not
	// the one that was asked for. Empty means the engine reported none.
	RedirectTo string `json:"redirect_to,omitempty"`
}

// DeploySourceDoc records where a deploy's bytes came from. It never holds a
// git token: the token lives only in the request scope.
type DeploySourceDoc struct {
	Repository string `json:"repository,omitempty"` // canonical https://github.com/<owner>/<name>
	Revision   string `json:"revision,omitempty"`
	Path       string `json:"path,omitempty"`
	// PrivateRepository records that the request carried a per-deploy git
	// token, never the token itself. Unknown fields are ignored on read, so a
	// document written before this field existed simply reports false.
	PrivateRepository bool   `json:"private_repository,omitempty"`
	UploadName        string `json:"upload_name,omitempty"`
	UploadFiles       int    `json:"upload_files,omitempty"`
	UploadBytes       int64  `json:"upload_bytes,omitempty"`
}

// DeployDoc is one deploy attempt. The facade owns every "observed" timestamp,
// so a server that reports no build timestamps still produces an honest, if
// approximate, duration — labelled as observed in the UI.
type DeployDoc struct {
	V              int             `json:"v"`
	ID             string          `json:"id"`
	ZoneID         string          `json:"zone_id"`
	ZoneName       string          `json:"zone_name"`
	Kind           string          `json:"kind"` // git | upload | rollback
	BuildID        string          `json:"build_id,omitempty"`
	ReleaseID      string          `json:"release_id,omitempty"`
	Source         DeploySourceDoc `json:"source"`
	Framework      string          `json:"framework,omitempty"`
	Phase          string          `json:"phase"`
	Reason         string          `json:"reason,omitempty"`
	Live           bool            `json:"live"`
	RolledBackFrom string          `json:"rolled_back_from,omitempty"`
	UploadID       string          `json:"upload_id,omitempty"`
	// RevisionResolved is the commit the engine reports for this deploy's
	// release. It is empty until the engine reports one, and stays empty on a
	// server that never does — the facade then shows the requested revision
	// alone rather than a placeholder.
	RevisionResolved string `json:"revision_resolved,omitempty"`

	RequestedAt         time.Time  `json:"requested_at"`
	EngineCallStartedAt *time.Time `json:"engine_call_started_at,omitempty"`
	EngineCreatedAt     *time.Time `json:"engine_created_at,omitempty"`
	EngineStartedAt     *time.Time `json:"engine_started_at,omitempty"`
	EngineFinishedAt    *time.Time `json:"engine_finished_at,omitempty"`
	ObservedBuildingAt  *time.Time `json:"observed_building_at,omitempty"`
	ObservedTerminalAt  *time.Time `json:"observed_terminal_at,omitempty"`
	ObservedLiveAt      *time.Time `json:"observed_live_at,omitempty"`

	LogSnapshot   bool       `json:"log_snapshot,omitempty"`
	LogCapturedAt *time.Time `json:"log_captured_at,omitempty"`
	PollDeadline  time.Time  `json:"poll_deadline,omitzero"`
	LastPolledAt  time.Time  `json:"last_polled_at,omitzero"`
	NotFoundPolls int        `json:"not_found_polls,omitempty"`
}

// Deploy phases, shared with the wire enum by the hosting facade.
const (
	DeployPhaseQueued     = "QUEUED"
	DeployPhaseBuilding   = "BUILDING"
	DeployPhaseReleasing  = "RELEASING"
	DeployPhaseLive       = "LIVE"
	DeployPhaseSuperseded = "SUPERSEDED"
	DeployPhaseFailed     = "FAILED"
	DeployPhaseAbandoned  = "ABANDONED"
	DeployPhaseLost       = "LOST"
)

// Active reports whether the poller still has work to do for this deploy.
func (d DeployDoc) Active() bool {
	switch d.Phase {
	case DeployPhaseQueued, DeployPhaseBuilding, DeployPhaseReleasing:
		return true
	default:
		return false
	}
}

// DeployLog is a captured build log. Lines are stored gzipped and are capped so
// one runaway build cannot fill the store; Truncated says so honestly.
type DeployLog struct {
	Lines      []string  `json:"lines"`
	Complete   bool      `json:"complete"`
	Truncated  bool      `json:"truncated"`
	CapturedAt time.Time `json:"captured_at"`
}

type deployLogDoc struct {
	V          int       `json:"v"`
	Lines      []string  `json:"lines"`
	Complete   bool      `json:"complete"`
	Truncated  bool      `json:"truncated"`
	CapturedAt time.Time `json:"captured_at"`
}

// UploadDoc is one accepted folder upload waiting to be packed and streamed.
//
// ID has the shape UploadIDPattern describes and is also the basename of Dir:
// the janitor only ever deletes a directory under HOSTING_UPLOAD_DIR whose name
// matches that pattern, so pointing the variable at a shared directory cannot
// make it remove anything the facade did not create.
type UploadDoc struct {
	V         int       `json:"v"`
	ID        string    `json:"id"`
	ZoneID    string    `json:"zone_id"`
	Name      string    `json:"name,omitempty"`
	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
	SHA256    string    `json:"sha256,omitempty"`
	Dir       string    `json:"dir"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// MailRecordDoc is one record of the desired mail set.
type MailRecordDoc struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
	TTL   uint32 `json:"ttl"`
}

// DkimDoc is one DKIM signature the mail engine generated.
type DkimDoc struct {
	Selector  string    `json:"selector"`
	Algorithm string    `json:"algorithm,omitempty"`
	Stage     string    `json:"stage,omitempty"`
	Published bool      `json:"published"`
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// MailDomainDoc is one zone bound to the mail engine.
type MailDomainDoc struct {
	V              int             `json:"v"`
	ZoneID         string          `json:"zone_id"`
	ZoneName       string          `json:"zone_name"`
	EngineDomainID string          `json:"engine_domain_id,omitempty"`
	State          string          `json:"state"`
	Reason         string          `json:"reason,omitempty"`
	DmarcPolicy    string          `json:"dmarc_policy,omitempty"`
	ReportAddress  string          `json:"report_address,omitempty"`
	Records        []MailRecordDoc `json:"records,omitempty"`
	RecordIDs      []string        `json:"record_ids,omitempty"`
	DKIM           []DkimDoc       `json:"dkim,omitempty"`
	DkimPending    bool            `json:"dkim_pending,omitempty"`
	ZoneMissing    bool            `json:"zone_missing,omitempty"`

	// PublishClientAutoconfig extends the published set with the mail server's
	// client-autoconfiguration records. It is omitempty on purpose: a row
	// written before the field existed decodes to false, so an existing binding
	// keeps exactly the records it already had.
	PublishClientAutoconfig bool `json:"publish_client_autoconfig,omitempty"`

	// Delivery is the rolling delivered/bounced count, one entry per whole hour,
	// oldest first. It is only ever written from the mail server's own delivery
	// events; with no receiver configured it stays empty and the console says
	// the numbers are not available rather than showing a zero.
	Delivery []MailDeliveryHourDoc `json:"delivery,omitempty"`

	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ReconciledAt  time.Time  `json:"reconciled_at,omitzero"`
	BindStartedAt *time.Time `json:"bind_started_at,omitempty"`
}

// MailDeliveryHourDoc counts one whole hour of delivery outcomes for one bound
// domain. Hour is the UTC hour the events fell in, truncated.
type MailDeliveryHourDoc struct {
	Hour      time.Time `json:"hour"`
	Delivered uint32    `json:"delivered,omitempty"`
	Bounced   uint32    `json:"bounced,omitempty"`
}

// PaymentMethodDoc is the card summary Stripe reports. It holds no PAN.
type PaymentMethodDoc struct {
	Brand    string `json:"brand,omitempty"`
	Last4    string `json:"last4,omitempty"`
	ExpMonth int    `json:"exp_month,omitempty"`
	ExpYear  int    `json:"exp_year,omitempty"`
}

// CustomerDoc is the single billing customer this deployment uses.
type CustomerDoc struct {
	V             int              `json:"v"`
	CustomerID    string           `json:"customer_id"`
	Email         string           `json:"email,omitempty"`
	PaymentMethod PaymentMethodDoc `json:"payment_method,omitzero"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// SubscriptionDoc is one domain's billing state. The two monotonic guards are
// per event family so an invoice event can never mask a subscription event.
type SubscriptionDoc struct {
	V              int    `json:"v"`
	ZoneID         string `json:"zone_id"`
	ZoneName       string `json:"zone_name"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	CustomerID     string `json:"customer_id,omitempty"`
	State          string `json:"state"`

	CurrentPeriodEnd *time.Time `json:"current_period_end,omitempty"`
	CancelAt         *time.Time `json:"cancel_at,omitempty"`
	LastEventID      string     `json:"last_event_id,omitempty"`

	LastSubscriptionEventCreated int64  `json:"last_subscription_event_created,omitempty"`
	LastInvoiceEventCreated      int64  `json:"last_invoice_event_created,omitempty"`
	LastInvoiceStatus            string `json:"last_invoice_status,omitempty"`

	CheckoutSessionID string     `json:"checkout_session_id,omitempty"`
	CheckoutExpiresAt *time.Time `json:"checkout_expires_at,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at"`
	ZoneMissing       bool       `json:"zone_missing,omitempty"`
}

// CheckoutDoc is one checkout session this control plane created. Confirming a
// session that is not in this bucket is refused, so the confirm route can never
// be used to read the Stripe account's other sessions.
type CheckoutDoc struct {
	V         int       `json:"v"`
	SessionID string    `json:"session_id"`
	ZoneID    string    `json:"zone_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Completed bool      `json:"completed"`
	Attempt   int       `json:"attempt"`
}

// Webhook outcomes recorded against an event id.
const (
	WebhookOutcomeQueued  = "queued"
	WebhookOutcomeDone    = "done"
	WebhookOutcomeStale   = "stale"
	WebhookOutcomeIgnored = "ignored"
	WebhookOutcomeRetry   = "retry"
	WebhookOutcomeDead    = "dead"
)

// WebhookEventDoc is the replay guard: an event id seen once is never applied
// twice, and the row outlives the queue entry so a redelivery is a duplicate.
type WebhookEventDoc struct {
	V             int       `json:"v"`
	Type          string    `json:"type,omitempty"`
	ReceivedAt    time.Time `json:"received_at"`
	ProcessedAt   time.Time `json:"processed_at,omitzero"`
	Outcome       string    `json:"outcome"`
	Attempts      int       `json:"attempts,omitempty"`
	Error         string    `json:"error,omitempty"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
}

// QueuedWebhook is one entry of the persisted delivery queue.
type QueuedWebhook struct {
	Key           string    `json:"-"`
	V             int       `json:"v"`
	EventID       string    `json:"event_id"`
	Payload       []byte    `json:"payload"`
	Attempts      int       `json:"attempts,omitempty"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
	Dead          bool      `json:"dead,omitempty"`
}

// WebhookStats is what the Billing page and the Settings page report.
type WebhookStats struct {
	Pending        uint32
	Dead           uint32
	LastReceivedAt time.Time
}

// Gateway sources.
const (
	GatewaySourceStatic   = "static"
	GatewaySourceResolved = "resolved"
)

// GatewayDoc is the apex address set every attached site points at, plus the
// hysteresis and pending state that keep one hijacked answer from repointing
// every customer apex.
type GatewayDoc struct {
	V             int       `json:"v"`
	Addresses     []string  `json:"addresses,omitempty"`
	Source        string    `json:"source,omitempty"`
	ResolvedAt    time.Time `json:"resolved_at,omitzero"`
	Candidate     []string  `json:"candidate,omitempty"`
	CandidateSeen int       `json:"candidate_seen,omitempty"`
	Pending       []string  `json:"pending,omitempty"`
	PendingSince  time.Time `json:"pending_since,omitzero"`
	LastError     string    `json:"last_error,omitempty"`
	Failures      int       `json:"failures,omitempty"`
}

// CapabilitiesDoc records what the hosting engine's server actually supports,
// so the facade degrades against an old server instead of guessing.
type CapabilitiesDoc struct {
	V               int  `json:"v"`
	ListBuilds      bool `json:"list_builds"`
	DeleteDomain    bool `json:"delete_domain"`
	BuildTimestamps bool `json:"build_timestamps"`
	DomainReadiness bool `json:"domain_readiness"`
	TenantScoped    bool `json:"tenant_scoped"`
	// EnvVars is true once the engine has answered an environment-variable RPC
	// with anything other than Unimplemented.
	EnvVars bool `json:"env_vars,omitempty"`
	// DomainRedirect is true once the engine has actually reported a redirect
	// on a host. An engine without the field and an engine with nothing to
	// redirect look identical on the wire, so this only ever turns true on
	// evidence.
	DomainRedirect bool `json:"domain_redirect,omitempty"`
	// ResolvedCommit is true once the engine has reported a commit on a
	// release.
	ResolvedCommit bool      `json:"resolved_commit,omitempty"`
	ProbedAt       time.Time `json:"probed_at,omitzero"`
}

// Map renders the capability set for platform.v1.EngineStatus.capabilities.
func (c CapabilitiesDoc) Map() map[string]bool {
	return map[string]bool{
		"list_builds":      c.ListBuilds,
		"delete_domain":    c.DeleteDomain,
		"build_timestamps": c.BuildTimestamps,
		"domain_readiness": c.DomainReadiness,
		"tenant_scoped":    c.TenantScoped,
		"env_vars":         c.EnvVars,
		"domain_redirect":  c.DomainRedirect,
		"resolved_commit":  c.ResolvedCommit,
	}
}

type metaDoc struct {
	V         int       `json:"v"`
	CreatedAt time.Time `json:"created_at"`
}
