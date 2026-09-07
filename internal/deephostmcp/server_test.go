package deephostmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// session is an MCP client speaking to the server over two in-memory pipes.
// Nothing is written to a file and no socket is opened: the transport is the
// pipe, and the platform behind the server is the fakes.
type session struct {
	t      *testing.T
	stdin  *io.PipeWriter
	stdout *bufio.Reader
	closer func()
	served chan error
	nextID int
}

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type toolAnswer struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent map[string]any `json:"structuredContent"`
	IsError           bool           `json:"isError"`
}

func start(t *testing.T, server *Server) *session {
	t.Helper()
	clientReader, clientWriter := io.Pipe()
	serverReader, serverWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		err := server.Serve(ctx, clientReader, serverWriter)
		// Closing the writer ends the reader's blocking read, so a test that
		// asks for one answer too many fails fast instead of hanging.
		ignore(serverWriter.Close())
		served <- err
	}()
	s := &session{t: t, stdin: clientWriter, stdout: bufio.NewReader(serverReader), served: served}
	s.closer = func() {
		ignore(clientWriter.Close())
		select {
		case err := <-served:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("Serve returned %v, want a clean end of input", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after stdin closed")
		}
		cancel()
	}
	t.Cleanup(s.closer)
	return s
}

// request sends one JSON-RPC request and reads the one answer to it.
func (s *session) request(method string, params any) envelope {
	s.t.Helper()
	s.nextID++
	body := map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method}
	if params != nil {
		body["params"] = params
	}
	line, err := json.Marshal(body)
	if err != nil {
		s.t.Fatalf("encode request: %v", err)
	}
	if _, err := s.stdin.Write(append(line, '\n')); err != nil {
		s.t.Fatalf("write request: %v", err)
	}
	answer, err := s.stdout.ReadBytes('\n')
	if err != nil {
		s.t.Fatalf("read answer: %v", err)
	}
	var received envelope
	if err := json.Unmarshal(answer, &received); err != nil {
		s.t.Fatalf("decode answer %q: %v", answer, err)
	}
	if received.JSONRPC != "2.0" {
		s.t.Errorf("answer carried jsonrpc %q, want 2.0", received.JSONRPC)
	}
	if string(received.ID) != jsonNumber(s.nextID) {
		s.t.Errorf("answer carried id %s, want %d", received.ID, s.nextID)
	}
	return received
}

func (s *session) call(name string, arguments map[string]any) *toolAnswer {
	s.t.Helper()
	if arguments == nil {
		arguments = map[string]any{}
	}
	received := s.request("tools/call", map[string]any{"name": name, "arguments": arguments})
	if received.Error != nil {
		s.t.Fatalf("tools/call %s failed at the protocol level: %v", name, received.Error)
	}
	if len(received.Result) == 0 {
		s.t.Fatalf("tools/call %s returned no result", name)
	}
	answer := &toolAnswer{}
	if err := json.Unmarshal(received.Result, answer); err != nil {
		s.t.Fatalf("decode tools/call %s result: %v", name, err)
	}
	// The text content and the structured content must be the same document:
	// a client that reads only one of them must not see a different answer.
	if len(answer.Content) != 1 {
		s.t.Fatalf("tools/call %s returned %d content blocks, want 1", name, len(answer.Content))
	}
	if answer.Content[0].Type != "text" {
		s.t.Fatalf("tools/call %s returned %q content, want text", name, answer.Content[0].Type)
	}
	var fromText map[string]any
	if err := json.Unmarshal([]byte(answer.Content[0].Text), &fromText); err != nil {
		s.t.Fatalf("tools/call %s did not put JSON in its text content: %v", name, err)
	}
	if !sameDocument(fromText, answer.StructuredContent) {
		s.t.Fatalf("tools/call %s returned different text and structured content:\n%v\n%v", name, fromText, answer.StructuredContent)
	}
	return answer
}

// sameDocument compares two decoded JSON documents by re-encoding them, which
// is enough here because both came from the same marshaller.
func sameDocument(left, right map[string]any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func jsonNumber(value int) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// refusal reads the error object out of a refused call.
func refusal(t *testing.T, answer *toolAnswer) map[string]any {
	t.Helper()
	if !answer.IsError {
		t.Fatalf("the call succeeded; want a refusal: %v", answer.StructuredContent)
	}
	failure, ok := answer.StructuredContent["error"].(map[string]any)
	if !ok {
		t.Fatalf("a refusal must carry an error object, got %v", answer.StructuredContent)
	}
	return failure
}

func TestServeAnswersAReadOnlyToolOverAnInMemoryTransport(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	client := start(t, fakes.server(t, ""))

	handshake := client.request("initialize", map[string]any{})
	if handshake.Error != nil {
		t.Fatalf("initialize failed: %v", handshake.Error)
	}

	answer := client.call("deephost_domain_list", nil)
	if answer.IsError {
		t.Fatalf("deephost_domain_list was refused: %v", answer.StructuredContent)
	}
	zones, ok := answer.StructuredContent["zones"].([]any)
	if !ok || len(zones) != 1 {
		t.Fatalf("want one zone in the answer, got %v", answer.StructuredContent)
	}
	zone, ok := zones[0].(map[string]any)
	if !ok || zone["name"] != "acme.dev" {
		t.Fatalf("want the zone named acme.dev, got %v", zones[0])
	}
	// A read-only tool runs without a credential: this is the whole reason the
	// server starts at all when nobody has logged in.
	if answer.StructuredContent["status"] == nil {
		t.Error("the answer dropped the server status the control plane sent")
	}
}

func TestServeRefusesADestructiveToolWithoutConfirmation(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	client := start(t, fakes.server(t, "operator-token"))

	answer := client.call("deephost_domain_delete", map[string]any{"domain": "acme.dev"})
	failure := refusal(t, answer)
	if failure["kind"] != KindConfirmationRequired {
		t.Errorf("kind = %v, want %s", failure["kind"], KindConfirmationRequired)
	}
	message := text(t, failure, "message")
	if !strings.Contains(message, "confirm: true") {
		t.Errorf("the refusal must say what to do about it, got %q", message)
	}
	if len(fakes.dns.deletedZones) != 0 {
		t.Fatalf("a refused call reached the control plane: %v", fakes.dns.deletedZones)
	}

	// The same call with the confirmation goes through, so the refusal is a
	// gate and not a wall.
	confirmed := client.call("deephost_domain_delete", map[string]any{"domain": "acme.dev", "confirm": true})
	if confirmed.IsError {
		t.Fatalf("the confirmed call was refused: %v", confirmed.StructuredContent)
	}
	if len(fakes.dns.deletedZones) != 1 || fakes.dns.deletedZones[0] != "z-1" {
		t.Errorf("deleted zones = %v, want [z-1]", fakes.dns.deletedZones)
	}
}

func TestMutatingToolsRefuseWithoutACredential(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	client := start(t, fakes.server(t, ""))

	tests := []struct {
		name      string
		tool      string
		arguments map[string]any
	}{
		{name: "create", tool: "deephost_domain_add", arguments: map[string]any{"name": "new.dev"}},
		{name: "destructive", tool: "deephost_domain_delete", arguments: map[string]any{"domain": "acme.dev", "confirm": true}},
		{name: "record", tool: "deephost_record_add", arguments: map[string]any{"domain": "acme.dev", "name": "@", "type": "A", "value": "203.0.113.1"}},
		{name: "mailbox", tool: "deephost_mail_mailbox_add", arguments: map[string]any{"domain": "acme.dev", "local_part": "mara"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := refusal(t, client.call(test.tool, test.arguments))
			if failure["kind"] != KindCredentialRequired {
				t.Errorf("kind = %v, want %s", failure["kind"], KindCredentialRequired)
			}
			message := text(t, failure, "message")
			if !strings.Contains(message, "deephost auth login") {
				t.Errorf("the refusal must say how to get a credential, got %q", message)
			}
		})
	}
	if len(fakes.dns.deletedZones)+len(fakes.dns.created)+len(fakes.mail.created) != 0 {
		t.Error("a tool refused for want of a credential still reached the control plane")
	}
}

func TestReadOnlyToolsRunWithoutACredential(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	client := start(t, fakes.server(t, ""))

	for _, name := range []string{"deephost_status", "deephost_domain_list", "deephost_record_list", "deephost_site_show", "deephost_version"} {
		arguments := map[string]any{}
		if name == "deephost_record_list" || name == "deephost_site_show" {
			arguments["domain"] = "acme.dev"
		}
		answer := client.call(name, arguments)
		if answer.IsError {
			t.Errorf("%s was refused without a credential: %v", name, answer.StructuredContent)
		}
	}
}

func TestToolsWithoutAUsableControlPlaneRefuseRatherThanCrash(t *testing.T) {
	t.Parallel()
	// A URL that cannot be validated is the one failure that leaves the server
	// with no clients at all.
	server := New(Options{BaseURL: "not a url", Version: "test"})
	client := start(t, server)

	failure := refusal(t, client.call("deephost_domain_list", nil))
	if failure["kind"] != KindNotConfigured {
		t.Errorf("kind = %v, want %s", failure["kind"], KindNotConfigured)
	}

	// deephost_version answers from this process, so it still works: an operator
	// with a broken configuration can always find out what the server thinks.
	answer := client.call("deephost_version", nil)
	if answer.IsError {
		t.Fatalf("deephost_version was refused: %v", answer.StructuredContent)
	}
	if answer.StructuredContent["control_plane_usable"] != false {
		t.Errorf("deephost_version must admit the control plane is unusable, got %v", answer.StructuredContent)
	}
	if reason := text(t, answer.StructuredContent, "reason"); !strings.Contains(reason, "api_url") {
		t.Errorf("the reason must name the field that is wrong, got %q", reason)
	}
}

func TestUnknownToolAndMalformedInput(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	server := fakes.server(t, "operator-token")

	tests := []struct {
		name    string
		request string
		want    string
	}{
		{name: "not JSON", request: "{not json", want: `"code":-32700`},
		{name: "unknown method", request: `{"jsonrpc":"2.0","id":1,"method":"tools/summon"}`, want: `"code":-32601`},
		{name: "unknown tool", request: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deephost_launch_missiles"}}`, want: KindUnknownTool},
		{name: "no tool name", request: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, want: KindInvalidArgument},
		{name: "arguments are not an object", request: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deephost_status","arguments":[1]}}`, want: KindInvalidArgument},
		{name: "notification is not answered", request: `{"jsonrpc":"2.0","method":"notifications/initialized"}`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out strings.Builder
			if err := server.Serve(context.Background(), strings.NewReader(test.request+"\n"), &out); err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if test.want == "" {
				if out.String() != "" {
					t.Fatalf("a notification was answered with %q", out.String())
				}
				return
			}
			if !strings.Contains(out.String(), test.want) {
				t.Fatalf("answer %q does not contain %q", out.String(), test.want)
			}
		})
	}
}

func TestToolsListDescribesEveryTool(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	client := start(t, fakes.server(t, "operator-token"))

	listed := client.request("tools/list", nil)
	if listed.Error != nil {
		t.Fatalf("tools/list failed: %v", listed.Error)
	}
	var payload struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations map[string]any `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listed.Result, &payload); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(payload.Tools) != len(registry) {
		t.Fatalf("tools/list returned %d tools, want %d", len(payload.Tools), len(registry))
	}
	for _, described := range payload.Tools {
		if described.Description == "" {
			t.Errorf("%s has no description", described.Name)
		}
		if described.InputSchema["type"] != "object" {
			t.Errorf("%s has schema type %v, want object", described.Name, described.InputSchema["type"])
		}
		if _, found := described.Annotations["readOnlyHint"]; !found {
			t.Errorf("%s has no readOnlyHint", described.Name)
		}
	}
}

func TestInitializeAnnouncesTheProtocolAndTheBuild(t *testing.T) {
	t.Parallel()
	fakes := newPlatform()
	client := start(t, fakes.server(t, ""))

	handshake := client.request("initialize", map[string]any{"protocolVersion": protocolVersion})
	if handshake.Error != nil {
		t.Fatalf("initialize failed: %v", handshake.Error)
	}
	var payload struct {
		ProtocolVersion string            `json:"protocolVersion"`
		Capabilities    map[string]any    `json:"capabilities"`
		ServerInfo      map[string]string `json:"serverInfo"`
	}
	if err := json.Unmarshal(handshake.Result, &payload); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if payload.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %q, want %q", payload.ProtocolVersion, protocolVersion)
	}
	if payload.ServerInfo["name"] != serverName || payload.ServerInfo["version"] != "test" {
		t.Errorf("serverInfo = %v", payload.ServerInfo)
	}
	if _, found := payload.Capabilities["tools"]; !found {
		t.Error("the handshake must advertise the tools capability")
	}

	// ping is answered so a client can keep the session alive.
	if pong := client.request("ping", nil); pong.Error != nil {
		t.Fatalf("ping failed: %v", pong.Error)
	}
}
