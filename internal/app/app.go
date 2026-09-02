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

	"github.com/castlemilk/dns/gen/go/dns/v1/dnsv1connect"
	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/control"
	"github.com/castlemilk/dns/internal/snapshot"
	"github.com/castlemilk/dns/internal/zone"
	"github.com/miekg/dns"
	"golang.org/x/sync/errgroup"
)

func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	switch cfg.Role {
	case config.RoleAll:
		return runWriter(ctx, cfg, logger, true)
	case config.RoleControl:
		return runWriter(ctx, cfg, logger, false)
	case config.RoleAuthority:
		return runAuthority(ctx, cfg, logger)
	default:
		return fmt.Errorf("unsupported DNS role %q", cfg.Role)
	}
}

func runWriter(ctx context.Context, cfg config.Config, logger *slog.Logger, serveDNS bool) error {
	store, err := zone.Open(cfg.DataPath, cfg.Nameservers, zone.WithSnapshotAdmission(func(values []zone.Zone) error {
		if err := snapshot.ValidateSize(values, cfg.SnapshotMaxBodyBytes, cfg.Production); err != nil {
			return &zone.ValidationError{Field: "zones", Message: err.Error()}
		}
		return nil
	}))
	if err != nil {
		return fmt.Errorf("open zone store: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Error("close zone store", "error", err)
		}
	}()

	startedAt := time.Now().UTC()
	var dnsHandler *authoritative.Server
	if serveDNS {
		dnsHandler = authoritative.New(logger, cfg.MaxUDPSize)
	}
	controlHandler := control.NewHandler(store, dnsHandler, logger, startedAt)
	if err := bootstrap(ctx, store, controlHandler, cfg.BootstrapZone); err != nil {
		return err
	}

	var feed http.Handler
	if cfg.Role == config.RoleControl {
		feed = snapshot.NewFeedHandler(store, logger, cfg.SnapshotMaxBodyBytes, cfg.Production)
	}
	httpHandler := newHTTPHandler(
		controlHandler,
		feed,
		logger,
		cfg.CORSOrigins,
		cfg.APIBearerToken,
		cfg.SnapshotBearerToken,
		cfg.HTTPMaxBodyBytes,
		nil,
	)
	httpServer := newHTTPServer(cfg.HTTPAddr, httpHandler)

	listenConfig := net.ListenConfig{}
	httpListener, err := listenConfig.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen for control API on %s: %w", cfg.HTTPAddr, err)
	}
	if !serveDNS {
		logger.Info("control service ready", "http", cfg.HTTPAddr, "snapshot_path", snapshot.Path)
		return serveHTTP(ctx, cfg.ShutdownTimeout, httpServer, httpListener)
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
	return serveListeners(
		ctx,
		cfg.ShutdownTimeout,
		httpServer,
		httpListener,
		udpServer,
		tcpServer,
		func() {
			logger.Info("DNS service ready", "http", cfg.HTTPAddr, "dns", cfg.DNSAddr, "nameservers", cfg.Nameservers)
		},
	)
}

func runAuthority(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	dnsHandler := authoritative.New(logger, cfg.MaxUDPSize)
	status := snapshot.NewStatus(cfg.SnapshotMaxStaleness)
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
		logger,
		nil,
		"",
		"",
		cfg.HTTPMaxBodyBytes,
		status.Ready,
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

func newHTTPHandler(
	handler *control.Handler,
	feed http.Handler,
	logger *slog.Logger,
	allowedOrigins []string,
	apiToken string,
	snapshotToken string,
	maxBodyBytes int64,
	readiness readinessCheck,
) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	protocols := make([]string, 0, 4)
	if feed == nil {
		protocols = append(protocols, "DNS over UDP/TCP")
	}
	if handler != nil {
		path, connectHandler := dnsv1connect.NewDNSServiceHandler(handler)
		mux.Handle(path, bearerAuth(apiToken, connectHandler))
		protocols = append(protocols, "Connect", "gRPC", "gRPC-Web")
	}
	if feed != nil {
		mux.Handle(snapshot.Path, bearerAuth(snapshotToken, feed))
		protocols = append(protocols, "authenticated JSON snapshots")
	}
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, logger, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if readiness != nil {
			ready, reason := readiness()
			if !ready {
				writeJSON(writer, logger, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": reason})
				return
			}
		}
		writeJSON(writer, logger, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /{$}", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, logger, http.StatusOK, map[string]any{
			"service":   "simpledns",
			"protocols": protocols,
		})
	})
	return cors(limitRequestBody(maxBodyBytes, mux), allowedOrigins)
}

func bearerAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	expected := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !validBearer(request, expected) {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func validBearer(request *http.Request, expected [sha256.Size]byte) bool {
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, credential, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || credential == "" {
		return false
	}
	provided := sha256.Sum256([]byte(credential))
	return subtle.ConstantTimeCompare(provided[:], expected[:]) == 1
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

func allowedOrigin(request *http.Request, origin string, configured []string) bool {
	if slices.Contains(configured, origin) {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Scheme != "" && strings.EqualFold(parsed.Host, request.Host)
}

func writeJSON(writer http.ResponseWriter, logger *slog.Logger, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		logger.Debug("write JSON response", "error", err)
	}
}
