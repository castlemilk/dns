package client_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	activityv1 "github.com/castlemilk/dns/gen/go/activity/v1"
	billingv1 "github.com/castlemilk/dns/gen/go/billing/v1"
	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/internal/deephostcli/client"
)

const theSecret = "tok_live_do_not_print_me"

// recorder is a fake control plane: it answers every Connect call with the
// status it was built with and remembers what the client sent. No real network
// leaves the loopback interface, and no generated service has to be stubbed.
type recorder struct {
	status int
	body   string

	mu      sync.Mutex
	headers []http.Header
	paths   []string
	queries []string
}

func (r *recorder) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	r.headers = append(r.headers, request.Header.Clone())
	r.paths = append(r.paths, request.URL.Path)
	r.queries = append(r.queries, request.URL.RawQuery)
	r.mu.Unlock()

	if r.status == http.StatusOK {
		// Echo the codec the client chose so an empty message decodes; an
		// empty Protobuf body is a valid empty response.
		writer.Header().Set("Content-Type", request.Header.Get("Content-Type"))
		writer.WriteHeader(r.status)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(r.status)
	if _, err := writer.Write([]byte(r.body)); err != nil {
		panic(err)
	}
}

func (r *recorder) lastHeader() http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.headers) == 0 {
		return nil
	}
	return r.headers[len(r.headers)-1]
}

func (r *recorder) seen() (paths, queries []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...), append([]string(nil), r.queries...)
}

func newRecorder(t *testing.T, status int, body string) (*recorder, string) {
	t.Helper()

	fake := &recorder{status: status, body: body}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return fake, server.URL
}

func TestNewValidatesOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    client.Options
		wantErr string
	}{
		{name: "loopback http", opts: client.Options{BaseURL: "http://127.0.0.1:8080"}},
		{name: "https with a token", opts: client.Options{BaseURL: "https://api.deephost.test", Token: theSecret}},
		{
			name:    "empty url",
			opts:    client.Options{},
			wantErr: "api_url: must be an absolute http or https URL with no userinfo, query or fragment",
		},
		{
			name:    "plain http to a public host",
			opts:    client.Options{BaseURL: "http://api.deephost.test"},
			wantErr: "api_url: must use https unless its host is localhost or a loopback address",
		},
		{
			name:    "token with a header-splitting byte",
			opts:    client.Options{BaseURL: "https://api.deephost.test", Token: "a\r\nX-Evil: 1"},
			wantErr: "token: must contain only printable ASCII",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			clients, err := client.New(test.opts)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("New error = %v, want %q", err, test.wantErr)
				}
				if clients != nil {
					t.Error("New returned clients alongside an error")
				}
				if err != nil && strings.Contains(err.Error(), theSecret) {
					t.Error("the error quoted the credential")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			for name, value := range map[string]any{
				"DNS": clients.DNS, "Platform": clients.Platform, "Hosting": clients.Hosting,
				"Mail": clients.Mail, "Billing": clients.Billing, "Activity": clients.Activity,
			} {
				if value == nil {
					t.Errorf("%s client is nil", name)
				}
			}
		})
	}
}

func TestNewNormalisesTheBaseURL(t *testing.T) {
	t.Parallel()

	clients, err := client.New(client.Options{BaseURL: "https://api.deephost.test/"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if clients.BaseURL != "https://api.deephost.test" {
		t.Errorf("BaseURL = %q, want the trailing slash removed", clients.BaseURL)
	}
}

// TestCredentialTravelsOnlyInTheHeader is the security promise: the bearer
// token is on the request header of every service, and nowhere else the fake
// control plane can observe it.
func TestCredentialTravelsOnlyInTheHeader(t *testing.T) {
	t.Parallel()

	fake, url := newRecorder(t, http.StatusOK, "")
	clients, err := client.New(client.Options{BaseURL: url, Token: theSecret, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Every one of the six services, because a missing interceptor on any one
	// of them would be an unauthenticated call in production.
	calls := []struct {
		name string
		call func(context.Context) error
	}{
		{name: "dns", call: func(ctx context.Context) error {
			_, err := clients.DNS.ListZones(ctx, connect.NewRequest(&dnsv1.ListZonesRequest{}))
			return err
		}},
		{name: "platform", call: func(ctx context.Context) error {
			_, err := clients.Platform.GetPlatformStatus(ctx, connect.NewRequest(&platformv1.GetPlatformStatusRequest{}))
			return err
		}},
		{name: "hosting", call: func(ctx context.Context) error {
			_, err := clients.Hosting.GetHostingStatus(ctx, connect.NewRequest(&hostingv1.GetHostingStatusRequest{}))
			return err
		}},
		{name: "mail", call: func(ctx context.Context) error {
			_, err := clients.Mail.GetMailStatus(ctx, connect.NewRequest(&mailv1.GetMailStatusRequest{}))
			return err
		}},
		{name: "billing", call: func(ctx context.Context) error {
			_, err := clients.Billing.GetBillingStatus(ctx, connect.NewRequest(&billingv1.GetBillingStatusRequest{}))
			return err
		}},
		{name: "activity", call: func(ctx context.Context) error {
			_, err := clients.Activity.ListEvents(ctx, connect.NewRequest(&activityv1.ListEventsRequest{}))
			return err
		}},
	}
	for _, call := range calls {
		if err := call.call(t.Context()); err != nil {
			t.Fatalf("%s call: %v", call.name, err)
		}
		header := fake.lastHeader()
		if got := header.Get("Authorization"); got != "Bearer "+theSecret {
			t.Errorf("%s: Authorization = %q, want the bearer credential", call.name, got)
		}
	}

	paths, queries := fake.seen()
	for index, path := range paths {
		if strings.Contains(path, theSecret) {
			t.Errorf("the credential reached the URL path: %q", path)
		}
		if strings.Contains(queries[index], theSecret) {
			t.Errorf("the credential reached the query string: %q", queries[index])
		}
	}
}

func TestNoTokenAttachesNoHeader(t *testing.T) {
	t.Parallel()

	fake, url := newRecorder(t, http.StatusOK, "")
	clients, err := client.New(client.Options{BaseURL: url})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := clients.DNS.ListZones(t.Context(), connect.NewRequest(&dnsv1.ListZonesRequest{})); err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	if got := fake.lastHeader().Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want it absent", got)
	}
}

func TestUnauthorizedIsExplained(t *testing.T) {
	t.Parallel()

	_, url := newRecorder(t, http.StatusUnauthorized, "unauthorized")
	clients, err := client.New(client.Options{BaseURL: url, Token: theSecret})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, callErr := clients.DNS.ListZones(t.Context(), connect.NewRequest(&dnsv1.ListZonesRequest{}))
	if callErr == nil {
		t.Fatal("a 401 did not produce an error")
	}
	if !client.IsUnauthenticated(callErr) {
		t.Errorf("IsUnauthenticated = false for %v", callErr)
	}
	explained := client.Explain(callErr)
	if !strings.Contains(explained, "refused the credential") {
		t.Errorf("Explain = %q, want it to name the credential", explained)
	}
	if strings.Contains(explained, theSecret) {
		t.Error("Explain leaked the credential")
	}
}

func TestExplain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{
			name: "validation keeps field: message verbatim",
			err:  connect.NewError(connect.CodeInvalidArgument, errors.New("records[0].ttl: must be between 30 and 86400")),
			want: "records[0].ttl: must be between 30 and 86400",
		},
		{
			name: "empty validation message still says something",
			err:  connect.NewError(connect.CodeInvalidArgument, errors.New("")),
			want: "the request was rejected as invalid",
		},
		{
			name: "not found keeps the server's words",
			err:  connect.NewError(connect.CodeNotFound, errors.New("zone or record not found")),
			want: "zone or record not found",
		},
		{
			name: "already exists keeps the server's words",
			err:  connect.NewError(connect.CodeAlreadyExists, errors.New("zone or record already exists")),
			want: "zone or record already exists",
		},
		{
			name: "failed precondition keeps the server's words",
			err:  connect.NewError(connect.CodeFailedPrecondition, errors.New("managed SOA and NS records cannot be changed")),
			want: "managed SOA and NS records cannot be changed",
		},
		{
			name: "unauthenticated points at login",
			err:  connect.NewError(connect.CodeUnauthenticated, errors.New("unauthorized")),
			want: "the control plane refused the credential; run `deephost auth login`, or pass --token",
		},
		{
			name: "permission denied",
			err:  connect.NewError(connect.CodePermissionDenied, errors.New("nope")),
			want: "the credential is not allowed to do that",
		},
		{
			name: "unavailable points at the url",
			err:  connect.NewError(connect.CodeUnavailable, errors.New("connection refused")),
			want: "the control plane is not reachable; check --api-url and that it is running",
		},
		{
			name: "deadline exceeded points at the timeout",
			err:  connect.NewError(connect.CodeDeadlineExceeded, errors.New("deadline")),
			want: "the control plane did not answer in time; try again, or raise --timeout",
		},
		{
			name: "cancelled",
			err:  connect.NewError(connect.CodeCanceled, errors.New("cancelled")),
			want: "the request was cancelled before the control plane answered",
		},
		{
			name: "unimplemented names the likely cause",
			err:  connect.NewError(connect.CodeUnimplemented, errors.New("no such method")),
			want: "this control plane does not implement that operation; it may be an older build",
		},
		{
			name: "internal never repeats the server's internals",
			err:  connect.NewError(connect.CodeInternal, errors.New("panic: nil map read at line 402")),
			want: "the control plane failed while handling the request; check its logs",
		},
		{
			name: "resource exhausted",
			err:  connect.NewError(connect.CodeResourceExhausted, errors.New("slow down")),
			want: "slow down",
		},
		{
			name: "a wrapped connect error is still recognised",
			err:  fmt.Errorf("list zones: %w", connect.NewError(connect.CodePermissionDenied, errors.New("nope"))),
			want: "the credential is not allowed to do that",
		},
		{
			name: "a context deadline that never reached connect",
			err:  fmt.Errorf("dial: %w", context.DeadlineExceeded),
			want: "the control plane did not answer in time; try again, or raise --timeout",
		},
		{
			name: "a context cancellation that never reached connect",
			err:  fmt.Errorf("dial: %w", context.Canceled),
			want: "the request was cancelled before the control plane answered",
		},
		{
			name: "an unresolvable host names the host",
			err:  fmt.Errorf("dial: %w", &net.DNSError{Err: "no such host", Name: "api.deephost.invalid", IsNotFound: true}),
			want: `the control plane's host name "api.deephost.invalid" could not be resolved`,
		},
		{
			name: "a refused dial",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			want: "the control plane is not reachable; check --api-url and that it is running",
		},
		{
			name: "anything else is shown as it is",
			err:  errors.New("x509: certificate has expired"),
			want: "x509: certificate has expired",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := client.Explain(test.err); got != test.want {
				t.Errorf("Explain = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExplainFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string
		err    error
		want   string
	}{
		{name: "nil", target: "https://api.deephost.test", err: nil, want: ""},
		{
			name:   "unreachable names the target",
			target: "https://api.deephost.test",
			err:    connect.NewError(connect.CodeUnavailable, errors.New("refused")),
			want:   "https://api.deephost.test is not reachable; check --api-url and that it is running",
		},
		{
			name:   "unauthenticated names the target",
			target: "https://api.deephost.test",
			err:    connect.NewError(connect.CodeUnauthenticated, errors.New("unauthorized")),
			want:   "https://api.deephost.test refused the credential; run `deephost auth login`, or pass --token",
		},
		{
			name:   "an empty target falls back to Explain",
			target: "",
			err:    connect.NewError(connect.CodeUnavailable, errors.New("refused")),
			want:   "the control plane is not reachable; check --api-url and that it is running",
		},
		{
			name:   "validation text is left exactly alone",
			target: "https://api.deephost.test",
			err:    connect.NewError(connect.CodeInvalidArgument, errors.New("name: is required")),
			want:   "name: is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := client.ExplainFor(test.target, test.err); got != test.want {
				t.Errorf("ExplainFor = %q, want %q", got, test.want)
			}
		})
	}
}

func TestIsUnauthenticated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "unauthenticated", err: connect.NewError(connect.CodeUnauthenticated, errors.New("no")), want: true},
		{
			name: "wrapped unauthenticated",
			err:  fmt.Errorf("call: %w", connect.NewError(connect.CodeUnauthenticated, errors.New("no"))),
			want: true,
		},
		{name: "another code", err: connect.NewError(connect.CodeNotFound, errors.New("no")), want: false},
		{name: "a plain error", err: errors.New("boom"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := client.IsUnauthenticated(test.err); got != test.want {
				t.Errorf("IsUnauthenticated = %v, want %v", got, test.want)
			}
		})
	}
}

// TestRedirectsAreNotFollowed proves the credential cannot be replayed to a
// host the control plane names in a Location header.
func TestRedirectsAreNotFollowed(t *testing.T) {
	t.Parallel()

	elsewhere, elsewhereURL := newRecorder(t, http.StatusOK, "")

	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, elsewhereURL+request.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)

	clients, err := client.New(client.Options{BaseURL: redirect.URL, Token: theSecret})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := clients.DNS.ListZones(t.Context(), connect.NewRequest(&dnsv1.ListZonesRequest{})); err == nil {
		t.Fatal("a redirect was followed and produced a successful call")
	}
	if header := elsewhere.lastHeader(); header != nil {
		t.Errorf("the credential was replayed to the redirect target: %q", header.Get("Authorization"))
	}
}
