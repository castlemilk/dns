package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/billing"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/control"
	"github.com/castlemilk/dns/internal/hosting"
	"github.com/castlemilk/dns/internal/mail"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/secretguard"
	"github.com/castlemilk/dns/internal/zone"
)

// testPlatform builds the full set of platform handlers from the WP0 stubs,
// which is exactly what a control plane with no engine configuration mounts.
func testPlatform(t *testing.T, cfg config.Config) (*platformHandlers, *platform.Store) {
	t.Helper()
	store, err := platform.Open(cfg.PlatformPath)
	if err != nil {
		t.Fatalf("platform.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close platform store: %v", err)
		}
	})
	deps := platform.Deps{Store: store, Recorder: activity.Nop()}
	hostingService, uploads := hosting.New(cfg.Hosting, deps)
	mailService := mail.New(cfg.Mail, deps)
	billingService, webhook := billing.New(cfg.Billing, deps)
	status := platform.NewStatusService(cfg, deps,
		[]platform.Probe{platform.NewDNSProbe(cfg, deps), hostingService, mailService, billingService},
		[]platform.Rebuilder{hostingService, mailService, billingService},
	)
	handlers := &platformHandlers{
		platform:       status,
		hosting:        hostingService,
		mail:           mailService,
		billing:        billingService,
		activity:       activity.NewLog(store, nil, nil),
		webhook:        webhook,
		uploads:        uploads,
		plan:           status.PlanHandler(),
		uploadMaxBytes: config.DefaultUploadMaxBytes,
	}
	if cfg.SnapshotBearerToken != "" {
		handlers.backup = status.BackupHandler()
	}
	return handlers, store
}

func testWriterConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Role:                config.RoleAll,
		Nameservers:         []string{"ns1.dns.test."},
		PlatformPath:        filepath.Join(t.TempDir(), "platform.db"),
		SnapshotBearerToken: "snapshot-secret",
		Activity:            config.Activity{MaxEvents: config.DefaultActivityMaxEvents},
	}
}

func testControlHandler(t *testing.T) *control.Handler {
	t.Helper()
	store, err := zone.Open(filepath.Join(t.TempDir(), "zones.db"), []string{"ns1.dns.test"})
	if err != nil {
		t.Fatalf("zone.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close zone store: %v", err)
		}
	})
	handler := control.NewHandler(store, authoritative.New(nil, authoritative.DefaultMaxUDPSize), nil, time.Now(), nil)
	if err := handler.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return handler
}

// generatedProcedures parses every served Connect procedure out of gen/go,
// skipping the DeepHost client, whose procedures this process calls and never
// answers.
func generatedProcedures(t *testing.T) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir("../../gen/go", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "deephost" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".connect.go") {
			return nil
		}
		fileSet := token.NewFileSet()
		file, parseErr := parser.ParseFile(fileSet, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range value.Names {
					if !strings.HasSuffix(name.Name, "Procedure") || index >= len(value.Values) {
						continue
					}
					literal, ok := value.Values[index].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					procedure, unquoteErr := strconv.Unquote(literal.Value)
					if unquoteErr != nil {
						return unquoteErr
					}
					found = append(found, procedure)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk gen/go: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no generated procedures found")
	}
	return found
}

func TestEveryGeneratedProcedureIsMounted(t *testing.T) {
	t.Parallel()
	cfg := testWriterConfig(t)
	handlers, _ := testPlatform(t, cfg)
	handler := newHTTPHandler(
		testControlHandler(t), nil, handlers, nil, nil,
		"api-secret", cfg.SnapshotBearerToken, 1<<20, nil,
	)

	for _, procedure := range generatedProcedures(t) {
		t.Run(procedure, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://control.test"+procedure, strings.NewReader("{}"))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer api-secret")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code == http.StatusNotFound {
				t.Fatalf("%s is not mounted (404)", procedure)
			}
			if response.Code == http.StatusUnauthorized {
				t.Fatalf("%s rejected the operator token", procedure)
			}
		})
	}
}

func TestEveryPlatformServiceRequiresTheOperatorToken(t *testing.T) {
	t.Parallel()
	cfg := testWriterConfig(t)
	handlers, _ := testPlatform(t, cfg)
	handler := newHTTPHandler(
		testControlHandler(t), nil, handlers, nil, nil,
		"api-secret", cfg.SnapshotBearerToken, 1<<20, nil,
	)

	services := []string{
		"/dns.v1.DNSService/ListZones",
		"/platform.v1.PlatformService/GetPlatformStatus",
		"/hosting.v1.HostingService/GetHostingStatus",
		"/mail.v1.MailService/GetMailStatus",
		"/billing.v1.BillingService/GetBillingStatus",
		"/activity.v1.ActivityService/ListEvents",
	}
	for _, procedure := range services {
		t.Run(procedure, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				token string
				want  int
			}{
				{name: "missing", want: http.StatusUnauthorized},
				{name: "snapshot token rejected", token: "snapshot-secret", want: http.StatusUnauthorized},
				{name: "operator token accepted", token: "api-secret", want: http.StatusOK},
			} {
				request := httptest.NewRequest(http.MethodPost, "http://control.test"+procedure, strings.NewReader("{}"))
				request.Header.Set("Content-Type", "application/json")
				if tc.token != "" {
					request.Header.Set("Authorization", "Bearer "+tc.token)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != tc.want {
					t.Fatalf("%s/%s: status = %d, want %d (body %q)", procedure, tc.name, response.Code, tc.want, response.Body.String())
				}
			}
		})
	}
}

func TestUnconfiguredEnginesRefuseMutationsAndNameTheirVariables(t *testing.T) {
	t.Parallel()
	cfg := testWriterConfig(t)
	handlers, _ := testPlatform(t, cfg)
	handler := newHTTPHandler(
		testControlHandler(t), nil, handlers, nil, nil,
		"api-secret", cfg.SnapshotBearerToken, 1<<20, nil,
	)

	tests := []struct {
		procedure string
		body      string
		wantEnv   []string
	}{
		{procedure: "/hosting.v1.HostingService/AttachSite", body: `{"zone_id":"z1"}`, wantEnv: []string{"HOSTING_API_URL"}},
		{procedure: "/mail.v1.MailService/BindMailDomain", body: `{"zone_id":"z1"}`, wantEnv: []string{"MAIL_API_URL", "MAIL_HOSTNAME"}},
		{procedure: "/billing.v1.BillingService/CreateCheckoutSession", body: `{"zone_id":"z1"}`, wantEnv: []string{"BILLING_PROVIDER", "STRIPE_SECRET_KEY"}},
	}
	for _, tt := range tests {
		t.Run(tt.procedure, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://control.test"+tt.procedure, strings.NewReader(tt.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer api-secret")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code == http.StatusOK {
				t.Fatalf("%s succeeded with no engine configured", tt.procedure)
			}
			body := response.Body.String()
			for _, name := range tt.wantEnv {
				if !strings.Contains(body, name) {
					t.Fatalf("%s: body %q does not name %s", tt.procedure, body, name)
				}
			}
		})
	}
}

func TestPlanRouteNeedsNoBearerAndTheBackupRouteDoes(t *testing.T) {
	t.Parallel()
	cfg := testWriterConfig(t)
	handlers, _ := testPlatform(t, cfg)
	handler := newHTTPHandler(
		testControlHandler(t), nil, handlers, nil, nil,
		"api-secret", cfg.SnapshotBearerToken, 1<<20, nil,
	)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://control.test/public/v1/plan", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("plan status = %d, want 200 without a bearer", response.Code)
	}
	var plan map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	for _, key := range []string{"hosting_configured", "mail_configured", "billing_configured", "price_label", "policy_note"} {
		if _, ok := plan[key]; !ok {
			t.Fatalf("plan is missing %q: %v", key, plan)
		}
	}
	if plan["hosting_configured"] != false || plan["billing_configured"] != false {
		t.Fatalf("plan claims a configured engine: %v", plan)
	}

	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "operator token rejected", token: "api-secret", want: http.StatusUnauthorized},
		{name: "snapshot token accepted", token: "snapshot-secret", want: http.StatusOK},
	} {
		request := httptest.NewRequest(http.MethodGet, "http://control.test/internal/v1/platform-backup", nil)
		if tc.token != "" {
			request.Header.Set("Authorization", "Bearer "+tc.token)
		}
		recorded := httptest.NewRecorder()
		handler.ServeHTTP(recorded, request)
		if recorded.Code != tc.want {
			t.Fatalf("backup/%s: status = %d, want %d", tc.name, recorded.Code, tc.want)
		}
	}
}

func TestBackupRouteIsAbsentWithoutASnapshotToken(t *testing.T) {
	t.Parallel()
	cfg := testWriterConfig(t)
	cfg.SnapshotBearerToken = ""
	handlers, _ := testPlatform(t, cfg)
	handler := newHTTPHandler(
		testControlHandler(t), nil, handlers, nil, nil,
		"api-secret", "", 1<<20, nil,
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://control.test/internal/v1/platform-backup", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no snapshot token is configured", response.Code)
	}
}

func TestAuthorityMountsNoPlatformRoutes(t *testing.T) {
	t.Parallel()
	handler := newHTTPHandler(nil, nil, nil, nil, nil, "", "", 1<<20, func() (bool, string) { return true, "ready" })

	for _, path := range []string{
		"/platform.v1.PlatformService/GetPlatformStatus",
		"/hosting.v1.HostingService/GetHostingStatus",
		"/mail.v1.MailService/GetMailStatus",
		"/billing.v1.BillingService/GetBillingStatus",
		"/activity.v1.ActivityService/ListEvents",
		"/public/v1/plan",
		"/internal/v1/platform-backup",
		"/hosting/v1/uploads",
		"/billing/v1/stripe/webhook",
	} {
		request := httptest.NewRequest(http.MethodPost, "http://authority.test"+path, strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s answered %d on the authority role, want 404", path, response.Code)
		}
	}
}

func TestWebhookRouteIsUnauthenticatedAndCappedTight(t *testing.T) {
	t.Parallel()
	cfg := testWriterConfig(t)
	handlers, _ := testPlatform(t, cfg)
	// The stub billing facade mounts no webhook handler; stand one in so the
	// mux wiring (no bearer, 256 KiB cap) is what is under test.
	handlers.webhook = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			http.Error(writer, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	handler := newHTTPHandler(
		testControlHandler(t), nil, handlers, nil, nil,
		"api-secret", cfg.SnapshotBearerToken, 1<<20, nil,
	)

	small := httptest.NewRequest(http.MethodPost, "http://control.test/billing/v1/stripe/webhook", strings.NewReader(`{"id":"evt_1"}`))
	small.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, small)
	if response.Code != http.StatusOK {
		t.Fatalf("signed-size webhook = %d, want 200 without a bearer", response.Code)
	}

	oversize := httptest.NewRequest(http.MethodPost, "http://control.test/billing/v1/stripe/webhook",
		bytes.NewReader(make([]byte, webhookBodyLimit+1)))
	oversize.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, oversize)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize webhook = %d, want 413", response.Code)
	}
}

func TestUploadRouteKeepsItsOwnCapBehindTheOperatorToken(t *testing.T) {
	t.Parallel()
	cfg := testWriterConfig(t)
	handlers, _ := testPlatform(t, cfg)
	handlers.uploadMaxBytes = 4 << 20
	handlers.uploads = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			http.Error(writer, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	// The DNS API keeps its tight 1 MiB limit; the upload route must not.
	handler := newHTTPHandler(
		testControlHandler(t), nil, handlers, nil, nil,
		"api-secret", cfg.SnapshotBearerToken, 1<<20, nil,
	)

	body := make([]byte, 2<<20)
	request := httptest.NewRequest(http.MethodPost, "http://control.test/hosting/v1/uploads", bytes.NewReader(body))
	request.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("upload without a bearer = %d, want 401", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "http://control.test/hosting/v1/uploads", bytes.NewReader(body))
	request.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	request.Header.Set("Authorization", "Bearer api-secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("2 MiB upload = %d, want 200 (the 1 MiB API limit must not apply here)", response.Code)
	}

	tooBig := make([]byte, handlers.uploadMaxBytes+uploadMultipartOverhead+1)
	request = httptest.NewRequest(http.MethodPost, "http://control.test/hosting/v1/uploads", bytes.NewReader(tooBig))
	request.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	request.Header.Set("Authorization", "Bearer api-secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload = %d, want 413", response.Code)
	}
}

// TestRequestDeadlines proves the two halves of §1.3's deadline rule against a
// real server: an upload handler that extends its own deadlines survives a body
// that takes longer than ReadTimeout to arrive, while a Connect request that
// stalls for the same time is still cut off.
func TestRequestDeadlines(t *testing.T) {
	if testing.Short() {
		t.Skip("the deadline test deliberately takes longer than the 15 s ReadTimeout")
	}
	t.Parallel()

	const slowBody = 18 * time.Second

	cfg := testWriterConfig(t)
	handlers, _ := testPlatform(t, cfg)
	handlers.uploads = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		controller := http.NewResponseController(writer)
		// This is what the real upload handler does before reading any part.
		if err := controller.SetReadDeadline(time.Now().Add(15 * time.Minute)); err != nil {
			http.Error(writer, "cannot extend the read deadline", http.StatusInternalServerError)
			return
		}
		if err := controller.SetWriteDeadline(time.Now().Add(15 * time.Minute)); err != nil {
			http.Error(writer, "cannot extend the write deadline", http.StatusInternalServerError)
			return
		}
		read, err := io.Copy(io.Discard, request.Body)
		if err != nil {
			http.Error(writer, "read failed", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprintf(writer, `{"bytes":%d}`, read); err != nil {
			t.Errorf("write upload response: %v", err)
		}
	})
	handler := newHTTPHandler(
		testControlHandler(t), nil, handlers, nil, nil,
		"api-secret", cfg.SnapshotBearerToken, 1<<20, nil,
	)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	server := newHTTPServer(listener.Addr().String(), handler)
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	address := listener.Addr().String()

	// A slow body writer: the same chunks, the same pacing, two routes.
	writeSlowly := func(t *testing.T, connection net.Conn, chunks int) {
		t.Helper()
		pause := slowBody / time.Duration(chunks)
		for range chunks {
			if _, err := connection.Write([]byte("0123456789")); err != nil {
				return
			}
			time.Sleep(pause)
		}
	}

	t.Run("upload survives a slow body", func(t *testing.T) {
		t.Parallel()
		connection, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() {
			if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("close connection: %v", err)
			}
		}()
		const chunks = 6
		header := "POST /hosting/v1/uploads HTTP/1.1\r\n" +
			"Host: control.test\r\n" +
			"Authorization: Bearer api-secret\r\n" +
			"Content-Type: multipart/form-data; boundary=x\r\n" +
			"Content-Length: " + strconv.Itoa(chunks*10) + "\r\n\r\n"
		if _, err := connection.Write([]byte(header)); err != nil {
			t.Fatalf("write header: %v", err)
		}
		writeSlowly(t, connection, chunks)

		if err := connection.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatalf("SetReadDeadline: %v", err)
		}
		status, err := bufio.NewReader(connection).ReadString('\n')
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if !strings.Contains(status, "200") {
			t.Fatalf("upload status line = %q, want 200 after a %s body", strings.TrimSpace(status), slowBody)
		}
	})

	t.Run("the DNS API keeps its read timeout", func(t *testing.T) {
		t.Parallel()
		connection, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() {
			if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("close connection: %v", err)
			}
		}()
		const chunks = 6
		header := "POST /dns.v1.DNSService/ListZones HTTP/1.1\r\n" +
			"Host: control.test\r\n" +
			"Authorization: Bearer api-secret\r\n" +
			"Content-Type: application/json\r\n" +
			"Content-Length: " + strconv.Itoa(chunks*10) + "\r\n\r\n"
		if _, err := connection.Write([]byte(header)); err != nil {
			t.Fatalf("write header: %v", err)
		}
		writeSlowly(t, connection, chunks)

		if err := connection.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatalf("SetReadDeadline: %v", err)
		}
		status, err := bufio.NewReader(connection).ReadString('\n')
		if err == nil && strings.Contains(status, "200") {
			t.Fatalf("a %s Connect body succeeded; ReadTimeout must still cut it", slowBody)
		}
	})
}

// TestRunWrapsTheLoggerInSecretguard proves §1.6's second half: the logger
// every facade receives through platform.Deps redacts credential-shaped text,
// so a wrapped engine error logged by WP1–WP3 cannot put a token in the pod's
// stdout.
func TestRunWrapsTheLoggerInSecretguard(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	deps := platform.Deps{Logger: redactingLogger(slog.New(slog.NewJSONHandler(&out, nil)))}

	deps.Log().Error("hosting engine call failed",
		"error", errors.New(`CreateGitBuild: 401 for https://x:ghp_0123456789abcdefghij0123@github.com/acme/site`),
		"stripe", "the key sk_test_51AbCdEfGhIjKlMnOp was rejected",
		"stalwart", "Bearer API_0123456789abcdefghij0123 is not authorised",
	)

	logged := out.String()
	for _, secret := range []string{
		"ghp_0123456789abcdefghij0123",
		"sk_test_51AbCdEfGhIjKlMnOp",
		"API_0123456789abcdefghij0123",
	} {
		if strings.Contains(logged, secret) {
			t.Errorf("the logger emitted %s verbatim: %s", secret, logged)
		}
	}
	if strings.Count(logged, secretguard.Placeholder) < 3 {
		t.Errorf("want one %s per credential, got: %s", secretguard.Placeholder, logged)
	}
}

// TestRunAppliesTheRedactingLogger pins the wiring the test above depends on:
// Run must wrap before it builds anything, or every logger downstream is raw.
func TestRunAppliesTheRedactingLogger(t *testing.T) {
	t.Parallel()

	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "app.go", nil, 0)
	if err != nil {
		t.Fatalf("parse app.go: %v", err)
	}
	found := false
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "Run" || function.Recv != nil {
			continue
		}
		ast.Inspect(function, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "redactingLogger" {
				found = true
			}
			return true
		})
	}
	if !found {
		t.Fatal("app.Run must pass its logger through redactingLogger before constructing anything (spec2 §1.6)")
	}
}

// TestUnconfiguredEnginesAnswerOnlyStatusAndListRPCs pins §1.1's split: an
// engine that is switched off answers Get*Status and List* so a page can render
// an honest empty state, and refuses everything else — including
// GetBillingSummary and GetSite, whose empty responses would read as "no domain
// is billed" and "this domain has no site" rather than "billing is off".
func TestUnconfiguredEnginesAnswerOnlyStatusAndListRPCs(t *testing.T) {
	t.Parallel()
	cfg := testWriterConfig(t)
	handlers, _ := testPlatform(t, cfg)
	handler := newHTTPHandler(
		testControlHandler(t), nil, handlers, nil, nil,
		"api-secret", cfg.SnapshotBearerToken, 1<<20, nil,
	)

	tests := []struct {
		procedure string
		wantOK    bool
	}{
		{procedure: "/hosting.v1.HostingService/GetHostingStatus", wantOK: true},
		{procedure: "/hosting.v1.HostingService/ListSites", wantOK: true},
		{procedure: "/hosting.v1.HostingService/GetSite"},
		{procedure: "/mail.v1.MailService/GetMailStatus", wantOK: true},
		{procedure: "/mail.v1.MailService/ListMailDomains", wantOK: true},
		{procedure: "/mail.v1.MailService/GetMailDomain"},
		{procedure: "/billing.v1.BillingService/GetBillingStatus", wantOK: true},
		{procedure: "/billing.v1.BillingService/ListInvoices", wantOK: true},
		{procedure: "/billing.v1.BillingService/GetBillingSummary"},
	}
	for _, tt := range tests {
		t.Run(tt.procedure, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://control.test"+tt.procedure, strings.NewReader(`{"zone_id":"z1"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer api-secret")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if tt.wantOK {
				if response.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (body %q)", response.Code, response.Body.String())
				}
				return
			}
			if response.Code == http.StatusOK {
				t.Fatalf("%s answered OK with no engine configured", tt.procedure)
			}
			if !strings.Contains(response.Body.String(), `"code":"failed_precondition"`) {
				t.Fatalf("status = %d, want a failed_precondition error (body %q)", response.Code, response.Body.String())
			}
		})
	}
}
