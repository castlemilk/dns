// Package deephostmcp serves the Deep Hosting platform over the Model Context
// Protocol's stdio transport: newline-delimited JSON-RPC on stdin and stdout,
// with one tool per operation the `deephost` CLI performs.
//
// Three rules shape everything here.
//
// Every tool answers with JSON, never prose. A caller that has to read English
// to find out what happened cannot act on it, so both the success and the
// refusal of a tool are objects: successes are the control plane's own protojson
// rendering, refusals are {"error":{"kind","message"}}.
//
// Nothing crashes. A missing credential, an unusable control-plane URL, an
// engine that is not configured and an RPC that fails are all reported as tool
// results, because a process that exits takes the explanation with it: the
// operator sees an MCP client that lost its server, not the sentence that would
// have told them what to fix.
//
// Secrets travel one way only. The credential is attached to outbound requests
// by the client package's interceptor and is never rendered here; a generated
// mailbox password is returned exactly once, by the tool that generated it,
// with a note telling the caller to give it to a human and keep no copy.
package deephostmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/castlemilk/dns/internal/deephostcli/client"
	"github.com/castlemilk/dns/internal/zonefile"
)

// protocolVersion is the MCP revision this server implements.
const protocolVersion = "2025-06-18"

// serverName is what an MCP client displays for this server.
const serverName = "deephost"

// maxRequestBytes bounds one JSON-RPC line. A zone import carries a whole zone
// file, so the bound is the control plane's own zone-file limit plus room for
// the JSON around it; anything larger is not a request this server could have
// served anyway.
const maxRequestBytes = zonefile.MaxBytes + 1<<20

// Options describes the control plane this server talks to.
type Options struct {
	// BaseURL is the control plane's root.
	BaseURL string

	// Token is the operator credential. Empty is legal and is the whole point
	// of the credential-required refusal: read-only tools still run and report
	// what the control plane says, mutating tools refuse before spending a
	// call.
	Token string

	// Version names this build, for the initialize handshake.
	Version string

	// Timeout bounds one RPC. Zero means client.DefaultTimeout.
	Timeout time.Duration

	// Transport replaces the HTTP transport, for tests that reach an httptest
	// server without a real dial.
	Transport http.RoundTripper

	// Clients replaces the six Connect clients wholesale. Tests set it to
	// fakes so no socket is opened at all; production leaves it nil.
	Clients *client.Clients

	// CredentialNote explains why there is no credential when the reason is
	// worth acting on: a settings file that could not be read, permissions
	// that had to be refused. It is shown with the refusal so an operator is
	// not told to log in again when logging in again is not the problem. It
	// must never quote the credential itself.
	CredentialNote string
}

// Server answers MCP JSON-RPC messages. It is built by New and is safe to use
// from the single goroutine that owns the transport; MCP stdio is one stream,
// so requests are handled in order.
type Server struct {
	clients *client.Clients

	// target names the control plane in messages, so an operator pointed at
	// the wrong host can see it.
	target string

	// credentialed reports whether an operator credential was configured.
	credentialed bool

	// unusable is why clients is nil: a URL or credential that could not be
	// validated. Every tool reports it instead of dialling nothing.
	unusable string

	// credentialNote is why there is no credential, when that is known.
	credentialNote string

	version string
}

// New builds a Server. It never fails: a control plane that cannot be
// addressed becomes a refusal that every tool reports, because an MCP client
// cannot read the error of a server that exited before it connected.
func New(opts Options) *Server {
	server := &Server{
		target:         strings.TrimSpace(opts.BaseURL),
		credentialed:   strings.TrimSpace(opts.Token) != "",
		credentialNote: strings.TrimSpace(opts.CredentialNote),
		version:        strings.TrimSpace(opts.Version),
	}
	if server.version == "" {
		server.version = "dev"
	}
	if opts.Clients != nil {
		server.clients = opts.Clients
		if opts.Clients.BaseURL != "" {
			server.target = opts.Clients.BaseURL
		}
		return server
	}
	clients, err := client.New(client.Options{
		BaseURL:   opts.BaseURL,
		Token:     opts.Token,
		Timeout:   opts.Timeout,
		Transport: opts.Transport,
	})
	if err != nil {
		// client.New's errors are "field: message" and never quote the
		// credential, so this is safe to hand back to a caller.
		server.unusable = err.Error()
		return server
	}
	server.clients = clients
	server.target = clients.BaseURL
	return server
}

// Target is the control plane this server addresses, for a caller that wants
// to log where it pointed. It carries no credential.
func (s *Server) Target() string { return s.target }

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// toolResult is one tools/call answer. Content carries the same JSON as
// StructuredContent, because clients that predate structured content read only
// the text.
type toolResult struct {
	Content           []content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Serve reads requests until EOF, a write failure or context cancellation.
// Logs belong on stderr: stdout is the protocol channel.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), maxRequestBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var request rpcRequest
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			// The decoder's own text can quote the offending input, which may
			// be anything the caller sent; say only that it was not JSON.
			if err := writeResponse(out, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "invalid JSON"}}); err != nil {
				return err
			}
			continue
		}
		response, respond := s.handle(ctx, request)
		if !respond {
			continue
		}
		if err := writeResponse(out, response); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("a request exceeded the %d-byte limit", maxRequestBytes)
		}
		return err
	}
	return nil
}

func writeResponse(out io.Writer, response rpcResponse) error {
	return json.NewEncoder(out).Encode(response)
}

func (s *Server) handle(ctx context.Context, request rpcRequest) (rpcResponse, bool) {
	response := rpcResponse{JSONRPC: "2.0", ID: rawID(request.ID)}
	if len(request.ID) == 0 {
		// A notification, including notifications/initialized, is never
		// answered under JSON-RPC.
		return response, false
	}
	switch request.Method {
	case "initialize":
		response.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": serverName, "version": s.version},
		}
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		response.Result = map[string]any{"tools": describeTools()}
	case "tools/call":
		response.Result = s.callTool(ctx, request.Params)
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	return response, true
}

func rawID(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var id any
	if json.Unmarshal(raw, &id) != nil {
		return nil
	}
	return id
}

// callTool runs one tool and turns whatever it produced into a result. It
// returns no error of its own: a bad call is a tool result marked isError, so
// the model sees a structured refusal rather than a protocol failure.
func (s *Server) callTool(ctx context.Context, raw json.RawMessage) *toolResult {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		return errorResult(invalid("params", "tools/call takes a tool name and an arguments object"))
	}
	name := strings.TrimSpace(call.Name)
	if name == "" {
		return errorResult(invalid("name", "required"))
	}
	definition, found := lookupTool(name)
	if !found {
		return errorResult(&toolError{Kind: KindUnknownTool, Message: fmt.Sprintf("name: %q is not a tool this server exposes", name)})
	}
	arguments := args{}
	if len(call.Arguments) > 0 && string(call.Arguments) != "null" {
		if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
			return errorResult(invalid("arguments", "must be an object"))
		}
	}
	if err := s.permit(definition, arguments); err != nil {
		return errorResult(err)
	}
	value, err := definition.Handler(s, ctx, arguments)
	if err != nil {
		return errorResult(s.translate(err))
	}
	return successResult(value)
}

// permit applies the two rules that hold before any call is made: a mutating
// tool needs a credential, and a destructive one needs to be told, in the call
// itself, that destruction is intended.
func (s *Server) permit(definition tool, arguments args) error {
	if definition.Local {
		return nil
	}
	if s.clients == nil {
		return &toolError{
			Kind:    KindNotConfigured,
			Message: fmt.Sprintf("this server has no usable control plane: %s. Set DEEPHOST_API_URL, or pass --api-url, and restart the MCP server", s.unusable),
		}
	}
	if definition.Mutates && !s.credentialed {
		message := fmt.Sprintf("%s changes %s and no operator credential is configured. Run `deephost auth login`, or set DEEPHOST_API_TOKEN in this server's environment, and restart it. Read-only tools still work.",
			definition.Name, s.describeTarget())
		if s.credentialNote != "" {
			message += " " + s.credentialNote
		}
		return &toolError{Kind: KindCredentialRequired, Message: message}
	}
	destructive := definition.Destructive
	if !destructive && definition.ConfirmIf != nil {
		asked, err := definition.ConfirmIf(arguments)
		if err != nil {
			return err
		}
		destructive = asked
	}
	if destructive {
		confirmed, err := boolArg(arguments, "confirm", false)
		if err != nil {
			return err
		}
		if !confirmed {
			return &toolError{
				Kind:    KindConfirmationRequired,
				Message: fmt.Sprintf("%s destroys data and was called without confirm: true. Ask the person you are acting for, then call it again with confirm: true.", definition.Name),
			}
		}
	}
	return nil
}

func (s *Server) describeTarget() string {
	if s.target == "" {
		return "the control plane"
	}
	return s.target
}

// translate turns a handler's error into a structured refusal. Errors this
// package raised carry their own kind; everything else came from Connect and is
// explained by the client package, which passes the control plane's own
// "field: message" validation text through unchanged.
func (s *Server) translate(err error) *toolError {
	var known *toolError
	if errors.As(err, &known) {
		return known
	}
	failure := &toolError{Kind: KindControlPlane, Message: client.ExplainFor(s.describeTarget(), err)}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		failure.Code = connectErr.Code().String()
		if connectErr.Code() == connect.CodeUnauthenticated {
			failure.Kind = KindCredentialRequired
		}
	}
	return failure
}

// Error kinds. They exist so a caller can branch without parsing English:
// a confirmation_required is a question for a human, a credential_required is
// an operator task, a control_plane failure is the platform's answer.
const (
	KindInvalidArgument      = "invalid_argument"
	KindConfirmationRequired = "confirmation_required"
	KindCredentialRequired   = "credential_required"
	KindNotConfigured        = "not_configured"
	KindNotFound             = "not_found"
	KindUnknownTool          = "unknown_tool"
	KindControlPlane         = "control_plane"
)

// toolError is a refusal a caller can act on.
type toolError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	// Code is the Connect code when the control plane is what refused.
	Code string `json:"code,omitempty"`
}

func (e *toolError) Error() string { return e.Message }

// invalid builds the repository's "field: message" validation failure, so a
// rejection from this server reads exactly like one from the control plane.
func invalid(field, message string) *toolError {
	return &toolError{Kind: KindInvalidArgument, Message: field + ": " + message}
}

func notFound(message string) *toolError {
	return &toolError{Kind: KindNotFound, Message: message}
}

func successResult(value any) *toolResult {
	data, err := json.Marshal(value)
	if err != nil {
		// Nothing a handler returns should fail to marshal; if one does, say
		// so as a refusal rather than dropping the call on the floor.
		return errorResult(&toolError{Kind: KindControlPlane, Message: "the answer could not be rendered as JSON"})
	}
	return &toolResult{Content: []content{{Type: "text", Text: string(data)}}, StructuredContent: value}
}

func errorResult(err error) *toolResult {
	failure, ok := err.(*toolError)
	if !ok {
		failure = &toolError{Kind: KindControlPlane, Message: err.Error()}
	}
	payload := map[string]any{"error": failure}
	data, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		data = []byte(`{"error":{"kind":"control_plane","message":"the refusal could not be rendered as JSON"}}`)
	}
	return &toolResult{
		Content:           []content{{Type: "text", Text: string(data)}},
		StructuredContent: payload,
		IsError:           true,
	}
}
