// Package client builds the six Connect clients the `simple` CLI and MCP
// server talk through, and turns what comes back from them into sentences an
// operator can act on.
//
// One HTTP client serves all six services so that connections are pooled and
// exactly one timeout, one redirect policy and one credential are in force.
// The credential is attached by an interceptor rather than by a transport, so
// it lives on the Connect request that is actually being made and never ends
// up in a URL, a log line or a metric attribute.
package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/castlemilk/dns/gen/go/activity/v1/activityv1connect"
	"github.com/castlemilk/dns/gen/go/billing/v1/billingv1connect"
	"github.com/castlemilk/dns/gen/go/dns/v1/dnsv1connect"
	"github.com/castlemilk/dns/gen/go/hosting/v1/hostingv1connect"
	"github.com/castlemilk/dns/gen/go/mail/v1/mailv1connect"
	"github.com/castlemilk/dns/gen/go/platform/v1/platformv1connect"
	"github.com/castlemilk/dns/internal/simplecli/config"
	"github.com/castlemilk/dns/internal/zonefile"
)

// DefaultTimeout bounds a single RPC. Zone import and export carry whole zone
// files, so it is generous rather than snappy.
const DefaultTimeout = 30 * time.Second

// maxRPCBytes matches the bound dnsctl uses, so a zone that imports through
// dnsctl also imports through `simple`.
const maxRPCBytes = zonefile.MaxBytes + 1<<20

// Options describes one control plane and the credential to spend on it.
type Options struct {
	// BaseURL is the control plane's root, for example https://api.simple.test.
	BaseURL string

	// Token is the operator credential. It may be empty: the read-only MCP
	// tools and `simple auth login` both run before one exists, and the
	// control plane answers with Unauthenticated, which Explain translates.
	Token string

	// Timeout bounds one RPC. Zero means DefaultTimeout.
	Timeout time.Duration

	// Transport replaces the HTTP transport. Tests set it to reach an
	// httptest server without a real dial; production leaves it nil.
	Transport http.RoundTripper
}

// Clients holds one client per service. All six share a connection pool, a
// timeout and a credential.
type Clients struct {
	// BaseURL is the normalised URL the clients were built against, kept so a
	// caller can name the host in a message without re-deriving it.
	BaseURL string

	DNS      dnsv1connect.DNSServiceClient
	Platform platformv1connect.PlatformServiceClient
	Hosting  hostingv1connect.HostingServiceClient
	Mail     mailv1connect.MailServiceClient
	Billing  billingv1connect.BillingServiceClient
	Activity activityv1connect.ActivityServiceClient
}

// New validates opts and constructs the six clients. Validation failures use
// the "field: message" convention, so they read the same as the control
// plane's own.
func New(opts Options) (*Clients, error) {
	base, err := config.ValidateURL("api_url", opts.BaseURL)
	if err != nil {
		return nil, err
	}
	if opts.Token != "" {
		if err := config.ValidateToken(opts.Token); err != nil {
			return nil, err
		}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	httpClient := newHTTPClient(timeout, opts.Transport)
	options := []connect.ClientOption{
		connect.WithInterceptors(BearerInterceptor(opts.Token)),
		connect.WithReadMaxBytes(maxRPCBytes),
		connect.WithSendMaxBytes(maxRPCBytes),
	}
	return &Clients{
		BaseURL:  base,
		DNS:      dnsv1connect.NewDNSServiceClient(httpClient, base, options...),
		Platform: platformv1connect.NewPlatformServiceClient(httpClient, base, options...),
		Hosting:  hostingv1connect.NewHostingServiceClient(httpClient, base, options...),
		Mail:     mailv1connect.NewMailServiceClient(httpClient, base, options...),
		Billing:  billingv1connect.NewBillingServiceClient(httpClient, base, options...),
		Activity: activityv1connect.NewActivityServiceClient(httpClient, base, options...),
	}, nil
}

// newHTTPClient is the one HTTP client all six services share. Redirects are
// never followed: a redirect would resend the operator credential to whatever
// host answered.
func newHTTPClient(timeout time.Duration, transport http.RoundTripper) *http.Client {
	if transport == nil {
		dialer := &net.Dialer{Timeout: min(timeout, 10*time.Second), KeepAlive: 30 * time.Second}
		transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   min(timeout, 10*time.Second),
			ResponseHeaderTimeout: min(timeout, time.Minute),
		}
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// BearerInterceptor attaches the operator credential to every outbound
// request. An empty token attaches nothing, so an unauthenticated call is made
// honestly and the control plane's own rejection is what the operator sees.
//
// The header is set on the request Connect is about to send; the credential is
// never written anywhere else.
func BearerInterceptor(token string) connect.Interceptor {
	return &bearerInterceptor{token: token}
}

type bearerInterceptor struct{ token string }

func (i *bearerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
		if i.token != "" && request.Spec().IsClient {
			request.Header().Set("Authorization", "Bearer "+i.token)
		}
		return next(ctx, request)
	}
}

func (i *bearerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if i.token != "" {
			conn.RequestHeader().Set("Authorization", "Bearer "+i.token)
		}
		return conn
	}
}

func (i *bearerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	// This interceptor is only ever installed on clients; a handler passes
	// straight through so that installing it on a server can never add a
	// credential to something being served.
	return next
}

var _ connect.Interceptor = (*bearerInterceptor)(nil)

// IsUnauthenticated reports whether err is the control plane refusing the
// credential, which is the one failure a caller can fix by logging in again.
func IsUnauthenticated(err error) bool {
	return connect.CodeOf(err) == connect.CodeUnauthenticated && asConnectError(err) != nil
}

// Explain turns err into one short sentence for an operator.
//
// Validation failures are passed through verbatim, because the control plane
// already phrases them as "field: message" and rewording them would lose the
// field. Everything else becomes a sentence that names what happened and, where
// there is one, the next thing to try.
func Explain(err error) string {
	if err == nil {
		return ""
	}
	connectErr := asConnectError(err)
	if connectErr == nil {
		return explainTransport(err)
	}
	message := strings.TrimSpace(connectErr.Message())
	switch connectErr.Code() {
	case connect.CodeInvalidArgument, connect.CodeOutOfRange:
		// Already "field: message" from the control plane's own validation.
		return orElse(message, "the request was rejected as invalid")
	case connect.CodeFailedPrecondition:
		return orElse(message, "the control plane is not in a state that allows that")
	case connect.CodeNotFound:
		return orElse(message, "not found")
	case connect.CodeAlreadyExists:
		return orElse(message, "already exists")
	case connect.CodeUnauthenticated:
		return "the control plane refused the credential; run `simple auth login`, or pass --token"
	case connect.CodePermissionDenied:
		return "the credential is not allowed to do that"
	case connect.CodeUnavailable:
		return "the control plane is not reachable; check --api-url and that it is running"
	case connect.CodeDeadlineExceeded:
		return "the control plane did not answer in time; try again, or raise --timeout"
	case connect.CodeCanceled:
		return "the request was cancelled before the control plane answered"
	case connect.CodeResourceExhausted:
		return orElse(message, "the control plane is rate limiting this client; try again shortly")
	case connect.CodeUnimplemented:
		return "this control plane does not implement that operation; it may be an older build"
	case connect.CodeAborted:
		return orElse(message, "the operation was aborted; try again")
	case connect.CodeInternal, connect.CodeDataLoss:
		return "the control plane failed while handling the request; check its logs"
	case connect.CodeUnknown:
		return orElse(message, "the control plane failed for an unknown reason")
	default:
		return orElse(message, "the control plane failed for an unknown reason")
	}
}

// ExplainFor is Explain with the target named, for the failures where knowing
// which control plane was addressed is the whole diagnosis.
func ExplainFor(target string, err error) string {
	message := Explain(err)
	if message == "" {
		return ""
	}
	if target == "" {
		return message
	}
	switch {
	case strings.Contains(message, "not reachable"):
		return fmt.Sprintf("%s is not reachable; check --api-url and that it is running", target)
	case strings.Contains(message, "refused the credential"):
		return fmt.Sprintf("%s refused the credential; run `simple auth login`, or pass --token", target)
	default:
		return message
	}
}

// explainTransport handles the errors that never reached a Connect response:
// a refused dial, a DNS failure, an expired certificate, a cancelled context.
func explainTransport(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the control plane did not answer in time; try again, or raise --timeout"
	case errors.Is(err, context.Canceled):
		return "the request was cancelled before the control plane answered"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Sprintf("the control plane's host name %q could not be resolved", dnsErr.Name)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "the control plane did not answer in time; try again, or raise --timeout"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "the control plane is not reachable; check --api-url and that it is running"
	}
	// Anything left is a client-side problem worth showing as it is: a bad
	// certificate, a proxy refusal, a malformed response. None of these can
	// carry the credential, which is only ever in a request header.
	return strings.TrimSpace(err.Error())
}

func asConnectError(err error) *connect.Error {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return connectErr
	}
	return nil
}

func orElse(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
