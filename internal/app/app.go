package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/castlemilk/dns/gen/go/activity/v1/activityv1connect"
	"github.com/castlemilk/dns/gen/go/billing/v1/billingv1connect"
	"github.com/castlemilk/dns/gen/go/dns/v1/dnsv1connect"
	"github.com/castlemilk/dns/gen/go/hosting/v1/hostingv1connect"
	"github.com/castlemilk/dns/gen/go/mail/v1/mailv1connect"
	"github.com/castlemilk/dns/gen/go/platform/v1/platformv1connect"
	activitylog "github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/control"
	"github.com/castlemilk/dns/internal/mail"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/secretguard"
	"github.com/castlemilk/dns/internal/snapshot"
	"github.com/castlemilk/dns/internal/telemetry"
	"github.com/castlemilk/dns/internal/zone"
	"github.com/miekg/dns"
	"golang.org/x/sync/errgroup"
)

func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	logger = redactingLogger(logger)
	observability, err := telemetry.Start(ctx, string(cfg.Role), logger)
	if err != nil {
		return fmt.Errorf("initialize OpenTelemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := observability.Shutdown(shutdownCtx); err != nil {
			logger.Error("flush OpenTelemetry during shutdown", "error", err)
		}
	}()
	metrics := observability.Metrics()
	switch cfg.Role {
	case config.RoleAll:
		return runWriter(ctx, cfg, logger, metrics, true)
	case config.RoleControl:
		return runWriter(ctx, cfg, logger, metrics, false)
	case config.RoleAuthority:
		return runAuthority(ctx, cfg, logger, metrics)
	default:
		return fmt.Errorf("unsupported DNS role %q", cfg.Role)
	}
}

// redactingLogger is applied once, in Run, before any handler, facade or
// worker is constructed: every logger in the process derives from this one, so
// this is where §1.6's rule that the slog handler applies secretguard.Redact is
// enforced. Engine clients wrap error text from DeepHost, Stalwart and Stripe
// into log attributes, and platform.Deps documents its Logger as
// secretguard-wrapped — without this call that promise is empty. Wrapping is
// idempotent, so a component that wraps its own component logger again is
// harmless.
func redactingLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return secretguard.NewLogger(logger)
}

func runWriter(ctx context.Context, cfg config.Config, logger *slog.Logger, metrics *telemetry.Metrics, serveDNS bool) error {
	store, err := zone.Open(cfg.DataPath, cfg.Nameservers, zone.WithSnapshotAdmission(func(values []zone.Zone) error {
		if err := snapshot.ValidateSize(values, cfg.SnapshotMaxBodyBytes, cfg.Production); err != nil {
			return &zone.ValidationError{Field: "zones", Message: err.Error()}
		}
		return nil
	}), zone.WithMetrics(metrics))
	if err != nil {
		return fmt.Errorf("open zone store: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Error("close zone store", "error", err)
		}
	}()

	platformStore, err := platform.Open(cfg.PlatformPath, platform.WithMetrics(metrics))
	if err != nil {
		return fmt.Errorf("open platform store: %w", err)
	}
	defer func() {
		if err := platformStore.Close(); err != nil {
			logger.Error("close platform store", "error", err)
		}
	}()

	startedAt := time.Now().UTC()
	var dnsHandler *authoritative.Server
	if serveDNS {
		dnsHandler = authoritative.New(logger, cfg.MaxUDPSize, metrics)
	}
	activityLog := activitylog.NewLog(platformStore, logger, metrics)
	controlHandler := control.NewHandler(
		store, dnsHandler, logger, startedAt, metrics,
		control.WithRecorder(activityLog),
		control.WithBindingChecker(platformStore),
	)
	if err := bootstrap(ctx, store, controlHandler, cfg.BootstrapZone); err != nil {
		return err
	}
	// Observers are delivered from a goroutine after the authoritative reload;
	// drain them before the stores close so a poke never lands on a closed
	// bbolt handle.
	defer controlHandler.WaitForObservers()

	engines := newEngines(ctx, cfg, controlHandler, platformStore, activityLog, logger, metrics)

	var feed http.Handler
	if cfg.Role == config.RoleControl {
		feed = snapshot.NewFeedHandler(store, logger, cfg.SnapshotMaxBodyBytes, cfg.Production, metrics)
	}
	platformRoutes := engines.handlers(cfg)
	if platformRoutes.requireBearer && cfg.APIBearerToken == "" {
		return ErrUnauthenticatedPlatformAPI
	}
	httpHandler := newHTTPHandler(
		controlHandler,
		feed,
		platformRoutes,
		logger,
		cfg.CORSOrigins,
		cfg.APIBearerToken,
		cfg.SnapshotBearerToken,
		cfg.HTTPMaxBodyBytes,
		nil,
		metrics,
	)
	httpServer := newHTTPServer(cfg.HTTPAddr, httpHandler)

	listenConfig := net.ListenConfig{}
	httpListener, err := listenConfig.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen for control API on %s: %w", cfg.HTTPAddr, err)
	}
	if !serveDNS {
		logger.Info("control service ready", "http", cfg.HTTPAddr, "snapshot_path", snapshot.Path)
		return runWithWorkers(ctx, logger, engines, func(groupCtx context.Context) error {
			return serveHTTP(groupCtx, cfg.ShutdownTimeout, httpServer, httpListener)
		})
	}
	udpConn, err := listenConfig.ListenPacket(ctx, "udp", cfg.DNSAddr)
	if err != nil {
		if closeErr := httpListener.Close(); closeErr != nil {
			return fmt.Errorf("listen for DNS UDP on %s: %v; close HTTP listener: %w", cfg.DNSAddr, err, closeErr)
		}
		return fmt.Errorf("listen for DNS UDP on %s: %w", cfg.DNSAddr, err)
	}
	tcpListener, err := listenConfig.Listen(ctx, "tcp", cfg.DNSAddr)
	if err != nil {
		closeErr := errors.Join(httpListener.Close(), udpConn.Close())
		if closeErr != nil {
			return fmt.Errorf("listen for DNS TCP on %s: %v; close listeners: %w", cfg.DNSAddr, err, closeErr)
		}
		return fmt.Errorf("listen for DNS TCP on %s: %w", cfg.DNSAddr, err)
	}

	udpServer := &dns.Server{PacketConn: udpConn, Handler: dnsHandler, UDPSize: int(cfg.MaxUDPSize)}
	tcpServer := &dns.Server{Listener: tcpListener, Handler: dnsHandler}
	return runWithWorkers(ctx, logger, engines, func(groupCtx context.Context) error {
		return serveListeners(
			groupCtx,
			cfg.ShutdownTimeout,
			httpServer,
			httpListener,
			udpServer,
			tcpServer,
			func() {
				logger.Info("DNS service ready", "http", cfg.HTTPAddr, "dns", cfg.DNSAddr, "nameservers", cfg.Nameservers)
			},
		)
	})
}

func runAuthority(ctx context.Context, cfg config.Config, logger *slog.Logger, metrics *telemetry.Metrics) error {
	dnsHandler := authoritative.New(logger, cfg.MaxUDPSize, metrics)
	status := snapshot.NewStatus(cfg.SnapshotMaxStaleness, metrics)
	consumer := snapshot.NewConsumer(
		dnsHandler,
		status,
		newSnapshotHTTPClient(cfg.SnapshotHTTPTimeout),
		cfg.SnapshotURL,
		cfg.SnapshotBearerToken,
		cfg.SnapshotCachePath,
		cfg.SnapshotMaxBodyBytes,
		cfg.Production,
		logger,
		metrics,
	)
	if err := consumer.LoadCache(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Info("no authoritative snapshot cache found", "path", cfg.SnapshotCachePath)
		} else {
			logger.Warn("ignore invalid authoritative snapshot cache", "path", cfg.SnapshotCachePath, "error", err)
		}
	} else {
		logger.Info("loaded authoritative snapshot cache", "path", cfg.SnapshotCachePath, "generated_at", status.GeneratedAt())
	}

	httpHandler := newHTTPHandler(
		nil,
		nil,
		nil,
		logger,
		nil,
		"",
		"",
		cfg.HTTPMaxBodyBytes,
		status.Ready,
		metrics,
	)
	httpServer := newHTTPServer(cfg.HTTPAddr, httpHandler)
	listenConfig := net.ListenConfig{}
	httpListener, err := listenConfig.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen for authority health on %s: %w", cfg.HTTPAddr, err)
	}
	udpConn, err := listenConfig.ListenPacket(ctx, "udp", cfg.DNSAddr)
	if err != nil {
		if closeErr := httpListener.Close(); closeErr != nil {
			return fmt.Errorf("listen for DNS UDP on %s: %v; close HTTP listener: %w", cfg.DNSAddr, err, closeErr)
		}
		return fmt.Errorf("listen for DNS UDP on %s: %w", cfg.DNSAddr, err)
	}
	tcpListener, err := listenConfig.Listen(ctx, "tcp", cfg.DNSAddr)
	if err != nil {
		closeErr := errors.Join(httpListener.Close(), udpConn.Close())
		if closeErr != nil {
			return fmt.Errorf("listen for DNS TCP on %s: %v; close listeners: %w", cfg.DNSAddr, err, closeErr)
		}
		return fmt.Errorf("listen for DNS TCP on %s: %w", cfg.DNSAddr, err)
	}

	udpServer := &dns.Server{PacketConn: udpConn, Handler: dnsHandler, UDPSize: int(cfg.MaxUDPSize)}
	tcpServer := &dns.Server{Listener: tcpListener, Handler: dnsHandler}
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		consumer.Run(groupCtx, cfg.SnapshotPollInterval)
		return nil
	})
	group.Go(func() error {
		return serveListeners(
			groupCtx,
			cfg.ShutdownTimeout,
			httpServer,
			httpListener,
			udpServer,
			tcpServer,
			func() {
				logger.Info("authority listeners ready", "http", cfg.HTTPAddr, "dns", cfg.DNSAddr)
			},
		)
	})
	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("run authority: %w", err)
	}
	return nil
}

func newSnapshotHTTPClient(timeout time.Duration) *http.Client {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic("http.DefaultTransport is not an *http.Transport")
	}
	transport := defaultTransport.Clone()
	transport.ResponseHeaderTimeout = timeout
	transport.MaxResponseHeaderBytes = 32 << 10
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
}

func serveHTTP(ctx context.Context, shutdownTimeout time.Duration, server *http.Server, listener net.Listener) error {
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && groupCtx.Err() == nil {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		return nil
	})
	group.Go(func() error {
		<-groupCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	})
	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("run HTTP service: %w", err)
	}
	return nil
}

func serveListeners(
	ctx context.Context,
	shutdownTimeout time.Duration,
	httpServer *http.Server,
	httpListener net.Listener,
	udpServer *dns.Server,
	tcpServer *dns.Server,
	onReady func(),
) error {
	udpStarted := make(chan struct{})
	tcpStarted := make(chan struct{})
	udpDone := make(chan struct{})
	tcpDone := make(chan struct{})
	udpServer.NotifyStartedFunc = func() { close(udpStarted) }
	tcpServer.NotifyStartedFunc = func() { close(tcpStarted) }

	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		if err := httpServer.Serve(httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) && groupCtx.Err() == nil {
			return fmt.Errorf("serve control API: %w", err)
		}
		return nil
	})
	group.Go(func() error {
		defer close(udpDone)
		if err := udpServer.ActivateAndServe(); err != nil && groupCtx.Err() == nil {
			return fmt.Errorf("serve DNS over UDP: %w", err)
		}
		return nil
	})
	group.Go(func() error {
		defer close(tcpDone)
		if err := tcpServer.ActivateAndServe(); err != nil && groupCtx.Err() == nil {
			return fmt.Errorf("serve DNS over TCP: %w", err)
		}
		return nil
	})

	// miekg/dns returns "server not started" when ShutdownContext races ahead
	// of ActivateAndServe. Wait until each server has either announced startup
	// or returned, so cancellation cannot leave a server starting after the one
	// and only shutdown attempt.
	udpRunning := dnsServerStarted(udpStarted, udpDone)
	tcpRunning := dnsServerStarted(tcpStarted, tcpDone)
	if udpRunning && tcpRunning && groupCtx.Err() == nil && onReady != nil {
		onReady()
	}

	group.Go(func() error {
		<-groupCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		var udpErr, tcpErr error
		if udpRunning {
			udpErr = udpServer.ShutdownContext(shutdownCtx)
		}
		if tcpRunning {
			tcpErr = tcpServer.ShutdownContext(shutdownCtx)
		}
		return errors.Join(udpErr, tcpErr, httpServer.Shutdown(shutdownCtx))
	})

	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("run DNS service: %w", err)
	}
	return nil
}

func dnsServerStarted(started, done <-chan struct{}) bool {
	select {
	case <-started:
		return true
	case <-done:
		return false
	}
}

func bootstrap(ctx context.Context, store *zone.Store, handler *control.Handler, bootstrapZone string) error {
	values, err := store.List(ctx)
	if err != nil {
		return fmt.Errorf("inspect zone store: %w", err)
	}
	if len(values) == 0 && strings.TrimSpace(bootstrapZone) != "" {
		if _, err := store.Create(ctx, bootstrapZone); err != nil {
			return fmt.Errorf("create bootstrap zone: %w", err)
		}
	}
	if err := handler.Reload(ctx); err != nil {
		return fmt.Errorf("load authoritative data: %w", err)
	}
	return nil
}

type readinessCheck func() (bool, string)

// platformHandlers is everything the engine facades contribute to the mux. It
// is nil for the authority role, which holds no engine credential and mounts
// none of these routes.
type platformHandlers struct {
	platform platformv1connect.PlatformServiceHandler
	hosting  hostingv1connect.HostingServiceHandler
	mail     mailv1connect.MailServiceHandler
	billing  billingv1connect.BillingServiceHandler
	activity activityv1connect.ActivityServiceHandler

	webhook http.Handler // nil unless billing is configured
	// mailWebhook is nil unless MAIL_WEBHOOK_SECRET is set. It is the mail
	// server's delivery-event receiver; it authenticates itself, so it is
	// mounted outside the operator bearer like the billing webhook.
	mailWebhook http.Handler
	uploads     http.Handler // nil unless HOSTING_UPLOADS_ENABLED
	plan        http.Handler // GET /public/v1/plan; always set on writer roles
	backup      http.Handler // nil when DNS_SNAPSHOT_BEARER_TOKEN is empty

	uploadMaxBytes int64

	// requireBearer is set when at least one engine is real, and these routes
	// therefore may not be served without DNS_API_BEARER_TOKEN. config.Load
	// already refuses that combination; newHTTPHandler refuses to mount it as
	// well, so a future change to the config rules cannot silently reopen an
	// unauthenticated DeployApp, DetachSite or DeleteZone.
	requireBearer bool
}

// ErrUnauthenticatedPlatformAPI is returned instead of a mux that would serve
// the engine facades to anyone who can reach the port.
var ErrUnauthenticatedPlatformAPI = errors.New(
	"refusing to serve the hosting, mail and billing APIs without DNS_API_BEARER_TOKEN: a real engine is configured")

// webhookBodyLimit bounds the unauthenticated Stripe webhook body. One HMAC
// over 256 KiB is cheap; anything larger is not a Stripe event.
const webhookBodyLimit = 256 << 10

// uploadMultipartOverhead is the slack added to HOSTING_UPLOAD_MAX_BYTES for
// multipart part headers and boundaries.
const uploadMultipartOverhead = 1 << 20

func newHTTPHandler(
	handler *control.Handler,
	feed http.Handler,
	platform *platformHandlers,
	logger *slog.Logger,
	allowedOrigins []string,
	apiToken string,
	snapshotToken string,
	maxBodyBytes int64,
	readiness readinessCheck,
	metricSets ...*telemetry.Metrics,
) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	metrics := telemetry.Select(metricSets...)
	mux := http.NewServeMux()
	protocols := make([]string, 0, 5)
	if feed == nil {
		protocols = append(protocols, "DNS over UDP/TCP")
	}
	if handler != nil {
		path, connectHandler := dnsv1connect.NewDNSServiceHandler(
			handler,
			connect.WithInterceptors(metrics.ConnectInterceptor()),
		)
		mux.Handle(path, bearerAuthFor(apiToken, "api", metrics, connectHandler))
		protocols = append(protocols, "Connect", "gRPC", "gRPC-Web")
	}
	if platform != nil {
		// Every platform service is mounted exactly like the DNS one: same
		// interceptor, same bearer audience. An unconfigured engine still
		// answers, with FailedPrecondition naming its variables, so the console
		// can tell "not configured" from "not deployed".
		mountConnect(mux, apiToken, metrics, platformv1connect.NewPlatformServiceHandler, platform.platform)
		mountConnect(mux, apiToken, metrics, hostingv1connect.NewHostingServiceHandler, platform.hosting)
		mountConnect(mux, apiToken, metrics, mailv1connect.NewMailServiceHandler, platform.mail)
		mountConnect(mux, apiToken, metrics, billingv1connect.NewBillingServiceHandler, platform.billing)
		mountConnect(mux, apiToken, metrics, activityv1connect.NewActivityServiceHandler, platform.activity)
		protocols = append(protocols, "platform")
	}
	if feed != nil {
		mux.Handle(snapshot.Path, bearerAuthFor(snapshotToken, "snapshot", metrics, feed))
		protocols = append(protocols, "authenticated JSON snapshots")
	}
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, logger, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, request *http.Request) {
		if readiness != nil {
			ready, reason := readiness()
			metrics.Readiness(request.Context(), ready, reason)
			if !ready {
				writeJSON(writer, logger, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": reason})
				return
			}
		}
		if readiness == nil {
			metrics.Readiness(request.Context(), true, "ready")
		}
		writeJSON(writer, logger, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /{$}", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, logger, http.StatusOK, map[string]any{
			"service":   "simpledns",
			"protocols": protocols,
		})
	})

	// The raw routes need their own body limits: the webhook is unauthenticated
	// and must stay small, a folder upload is two orders of magnitude larger
	// than any Connect request, and the plan route has no body at all. They are
	// registered on an outer mux so the tight DNS API limit still applies to
	// everything else.
	outer := http.NewServeMux()
	outer.Handle("/", limitRequestBody(maxBodyBytes, mux))
	if platform != nil {
		if platform.webhook != nil {
			outer.Handle("POST /billing/v1/stripe/webhook", limitRequestBody(webhookBodyLimit, platform.webhook))
		}
		if platform.mailWebhook != nil {
			outer.Handle("POST /mail/v1/delivery-events",
				limitRequestBody(mail.WebhookBodyLimit, platform.mailWebhook))
		}
		if platform.uploads != nil {
			uploadLimit := platform.uploadMaxBytes + uploadMultipartOverhead
			outer.Handle("POST /hosting/v1/uploads",
				bearerAuthFor(apiToken, "api", metrics, limitRequestBody(uploadLimit, platform.uploads)))
		}
		if platform.plan != nil {
			outer.Handle("GET /public/v1/plan", platform.plan)
		}
		if platform.backup != nil {
			outer.Handle("GET /internal/v1/platform-backup",
				bearerAuthFor(snapshotToken, "snapshot", metrics, platform.backup))
		}
	}
	return metrics.HTTPMiddleware(cors(outer, allowedOrigins))
}

// mountConnect registers one generated Connect service behind the operator
// bearer, or does nothing when the service was not built.
func mountConnect[H any](
	mux *http.ServeMux,
	apiToken string,
	metrics *telemetry.Metrics,
	construct func(H, ...connect.HandlerOption) (string, http.Handler),
	service H,
) {
	if any(service) == nil {
		return
	}
	path, handler := construct(service, connect.WithInterceptors(metrics.ConnectInterceptor()))
	mux.Handle(path, bearerAuthFor(apiToken, "api", metrics, handler))
}

func bearerAuth(token string, next http.Handler) http.Handler {
	return bearerAuthFor(token, "other", telemetry.Disabled(), next)
}

// bearerAuthFor guards a route with the shared-secret bearer. An empty token
// serves the route unauthenticated, which is only ever allowed for a control
// plane that can reach nothing real: config.Load refuses an empty
// DNS_API_BEARER_TOKEN whenever config.RequiresOperatorToken is true, and run
// refuses to mount the platform routes in that state
// (ErrUnauthenticatedPlatformAPI). Do not add a call site that relies on this
// returning next without checking one of those two.
func bearerAuthFor(token, audience string, metrics *telemetry.Metrics, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	expected := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if reason := bearerFailureReason(request, expected); reason != "" {
			metrics.AuthFailure(request.Context(), audience, reason)
			writer.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func validBearer(request *http.Request, expected [sha256.Size]byte) bool {
	return bearerFailureReason(request, expected) == ""
}

func bearerFailureReason(request *http.Request, expected [sha256.Size]byte) string {
	values := request.Header.Values("Authorization")
	if len(values) == 0 {
		return "missing"
	}
	if len(values) != 1 {
		return "multiple"
	}
	scheme, credential, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || credential == "" {
		return "malformed"
	}
	provided := sha256.Sum256([]byte(credential))
	if subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
		return "invalid"
	}
	return ""
}

func limitRequestBody(maxBytes int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if maxBytes <= 0 {
			http.Error(writer, "request body limit is not configured", http.StatusInternalServerError)
			return
		}
		if request.ContentLength > maxBytes {
			http.Error(writer, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if request.Body != nil {
			request.Body = http.MaxBytesReader(writer, request.Body, maxBytes)
		}
		next.ServeHTTP(writer, request)
	})
}

func cors(next http.Handler, allowedOrigins []string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Add("Vary", "Origin")
		origin := request.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(writer, request)
			return
		}
		if !allowedOrigin(request, origin, allowedOrigins) {
			http.Error(writer, "origin is not allowed", http.StatusForbidden)
			return
		}
		writer.Header().Set("Access-Control-Allow-Origin", origin)
		writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		writer.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Grpc-Timeout, X-User-Agent")
		writer.Header().Set("Access-Control-Expose-Headers", "Connect-Content-Encoding, Connect-Accept-Encoding, Grpc-Status, Grpc-Message")
		if request.Method == http.MethodOptions {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// allowedOrigin decides whether a browser origin may read this API. An origin
// listed in DNS_CORS_ORIGINS always may. Otherwise the same-origin fallback
// applies, and it compares the scheme as well as the host: on a deployment
// served over HTTPS, `http://<host>` is a different origin, and treating it as
// the same one hands the whole control API to any plaintext surface answering
// on that name — a gateway listener replying before the HTTP→HTTPS redirect,
// or an on-path attacker.
func allowedOrigin(request *http.Request, origin string, configured []string) bool {
	if slices.Contains(configured, origin) {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Host, request.Host) {
		return false
	}
	return strings.EqualFold(parsed.Scheme, requestScheme(request))
}

// requestScheme is the scheme the client used to reach this listener. The TLS
// state of the connection is authoritative when the process terminates TLS
// itself; behind a gateway that terminates it, the gateway's
// X-Forwarded-Proto is the only witness. A client cannot forge that header
// from a browser — it is not CORS-safelisted, so a cross-origin request
// carrying it is preflighted and the preflight does not allow it — and a
// client that can set headers freely is not subject to CORS at all.
func requestScheme(request *http.Request) string {
	if request.TLS != nil {
		return "https"
	}
	if forwarded := request.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme, _, _ := strings.Cut(forwarded, ",")
		if trimmed := strings.TrimSpace(scheme); trimmed != "" {
			return trimmed
		}
	}
	return "http"
}

func writeJSON(writer http.ResponseWriter, logger *slog.Logger, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		logger.Debug("write JSON response", "error", err)
	}
}
