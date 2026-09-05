package stalwart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Transport limits, all taken from the server's own session document
// (mail-facts.md §2): 16 method calls per request, 500 objects per get/set,
// 10 MB request body. The response cap mirrors the request one so a runaway
// answer cannot be read into memory unbounded.
const (
	// MaxCallsPerRequest is the server's documented batch limit.
	MaxCallsPerRequest = 16
	// MaxObjectsPerCall is the server's documented per-call object limit; it
	// is also the page size the unfiltered list queries use.
	MaxObjectsPerCall = 500
	// MaxResponseBytes bounds one JMAP response body.
	MaxResponseBytes = 10 << 20
	// DefaultTimeout bounds one JMAP request.
	DefaultTimeout = 10 * time.Second
)

// capabilities is the "using" list every request sends.
var capabilities = []string{"urn:ietf:params:jmap:core", "urn:stalwart:jmap"}

// MethodCall is one entry of methodCalls: [name, args, id].
type MethodCall struct {
	Name string
	Args any
	ID   string
}

// MethodResponse is one entry of methodResponses, with the arguments left as
// raw JSON so a caller decodes only the shape it asked for.
type MethodResponse struct {
	Name string
	ID   string
	Args json.RawMessage
}

// Client talks to one Stalwart server as one API key.
type Client struct {
	base   *url.URL
	token  string
	client *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client. The timeout is applied per request
// through the context either way, so a caller's client needs none.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.client = client
		}
	}
}

// New builds a client for rawURL authenticating with token. The token is held
// in memory only: it is never logged, never put in an error and never returned.
func New(rawURL, token string, options ...Option) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("mail engine: MAIL_API_URL is not an absolute URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("mail engine: MAIL_API_URL must use http or https")
	}
	client := &Client{
		base:  &url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: strings.TrimSuffix(parsed.Path, "/")},
		token: strings.TrimSpace(token),
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       60 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: DefaultTimeout,
				ExpectContinueTimeout: time.Second,
			},
		},
	}
	for _, option := range options {
		option(client)
	}
	return client, nil
}

// EndpointHost is the host[:port] of the server, safe to log and to show an
// operator: no scheme, no path, no credential.
func (c *Client) EndpointHost() string { return c.base.Host }

func (c *Client) endpoint(path string) string {
	return c.base.String() + path
}

// jmapRequest is the wire body.
type jmapRequest struct {
	Using       []string  `json:"using"`
	MethodCalls [][]any   `json:"methodCalls"`
	CreatedIDs  *struct{} `json:"createdIds,omitempty"`
}

type jmapResponse struct {
	MethodResponses []json.RawMessage `json:"methodResponses"`
	SessionState    string            `json:"sessionState"`
}

// Call posts one JMAP request and returns the responses in the order the
// server sent them. A `["error",…]` response for any call is returned as a
// *MethodError, because every batch this package builds is a unit of work: a
// failed query makes the get that references its result meaningless.
func (c *Client) Call(ctx context.Context, calls []MethodCall) ([]MethodResponse, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	if len(calls) > MaxCallsPerRequest {
		return nil, fmt.Errorf("mail engine: %d method calls exceeds the server limit of %d", len(calls), MaxCallsPerRequest)
	}

	body := jmapRequest{Using: capabilities, MethodCalls: make([][]any, 0, len(calls))}
	for _, call := range calls {
		args := call.Args
		if args == nil {
			args = map[string]any{}
		}
		body.MethodCalls = append(body.MethodCalls, []any{call.Name, args, call.ID})
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("mail engine: encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/jmap"), bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("mail engine: build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	c.authorize(request)

	response, err := c.client.Do(request)
	if err != nil {
		// The URL in a transport error can carry the path but never the token
		// (it travels as a header), and the host is already operator-visible.
		return nil, fmt.Errorf("%w: %s", ErrTransport, transportReason(err))
	}
	defer func() { ignore(response.Body.Close()) }()

	if err := statusError(response.StatusCode); err != nil {
		return nil, err
	}

	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: the response could not be read", ErrTransport)
	}
	if len(raw) > MaxResponseBytes {
		return nil, fmt.Errorf("%w: the response exceeded %d bytes", ErrTransport, MaxResponseBytes)
	}

	var decoded jmapResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("%w: the response was not JMAP JSON", ErrTransport)
	}

	responses := make([]MethodResponse, 0, len(decoded.MethodResponses))
	for _, entry := range decoded.MethodResponses {
		var triple []json.RawMessage
		if err := json.Unmarshal(entry, &triple); err != nil || len(triple) != 3 {
			return nil, fmt.Errorf("%w: a method response was not a [name, args, id] triple", ErrTransport)
		}
		var name, id string
		if err := json.Unmarshal(triple[0], &name); err != nil {
			return nil, fmt.Errorf("%w: a method response had no name", ErrTransport)
		}
		if err := json.Unmarshal(triple[2], &id); err != nil {
			return nil, fmt.Errorf("%w: a method response had no call id", ErrTransport)
		}
		responses = append(responses, MethodResponse{Name: name, ID: id, Args: triple[1]})
	}

	for index, entry := range responses {
		if entry.Name != "error" {
			continue
		}
		method := entry.Name
		if index < len(calls) {
			method = calls[index].Name
		}
		var payload struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		}
		// A method error with an unreadable body is still a method error; the
		// empty type is reported rather than swallowed.
		ignore(json.Unmarshal(entry.Args, &payload))
		return responses, &MethodError{Method: method, Type: payload.Type, Description: payload.Description}
	}
	return responses, nil
}

// authorize adds the bearer credential. An empty token means an unauthenticated
// server (never valid in production, see config §9.1), and the header is then
// omitted rather than sent empty.
func (c *Client) authorize(request *http.Request) {
	if c.token == "" {
		return
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
}

// getJSON performs one authenticated GET against a non-JMAP endpoint and
// decodes the body into out.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(path), nil)
	if err != nil {
		return fmt.Errorf("mail engine: build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	c.authorize(request)

	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrTransport, transportReason(err))
	}
	defer func() { ignore(response.Body.Close()) }()

	if err := statusError(response.StatusCode); err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%w: the response could not be read", ErrTransport)
	}
	if len(raw) > MaxResponseBytes {
		return fmt.Errorf("%w: the response exceeded %d bytes", ErrTransport, MaxResponseBytes)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: the response was not the expected JSON", ErrTransport)
	}
	return nil
}

// statusError maps an HTTP status onto this package's sentinels. Only 200 is
// success: Stalwart reports every method-level failure inside a 200 body.
func statusError(status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		return fmt.Errorf("%w: the server answered HTTP %d", ErrTransport, status)
	}
}

// ignore documents a deliberately dropped error. Closing a response body and
// re-reading an error payload can both fail without changing what the caller
// already knows, and errcheck's blank-identifier rule wants the intent named.
func ignore(error) {}

// transportReason turns a transport failure into one short, credential-free
// clause. url.Error.Error() would print the request URL, which is harmless but
// noisy; the cause is what an operator needs.
func transportReason(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		switch {
		case urlErr.Timeout():
			return "the request timed out"
		case errors.Is(urlErr.Err, context.Canceled):
			return "the request was canceled"
		default:
			return urlErr.Err.Error()
		}
	}
	return err.Error()
}
