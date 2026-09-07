package maild

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/mail/stalwart"
)

const (
	testToken    = "maild-test-token-8f21c0"
	testHostname = "mail.local.test"
	// testSecret is the mailbox credential these tests create with. Every
	// response body is searched for it: it must never come back out.
	testSecret = "correct-horse-battery-staple-probe"
)

// newHandler builds a handler over a fresh store. The logger is discarded
// because the failure paths deliberately log their detail, and a test that
// exercises them should not print it.
func newHandler(t *testing.T, customize ...func(*Options)) (*Handler, *Store) {
	t.Helper()
	store := newStore(t)
	options := Options{
		Store:    store,
		Token:    testToken,
		Hostname: testHostname,
		Now:      func() time.Time { return testClock },
		Logger:   slog.New(slog.DiscardHandler),
	}
	for _, apply := range customize {
		apply(&options)
	}
	handler, err := NewHandler(options)
	if err != nil {
		t.Fatalf("build the handler: %v", err)
	}
	return handler, store
}

// call is one methodCalls entry.
func call(name string, args map[string]any, id string) []any {
	return []any{name, args, id}
}

// reference is a "#ids" back-reference.
func reference(resultOf, name string) map[string]any {
	return map[string]any{"resultOf": resultOf, "name": name, "path": "/ids"}
}

// post sends one request to the handler. No socket is opened: the handler is
// exercised directly through httptest.
func post(t *testing.T, handler *Handler, path, token string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	method := http.MethodPost
	if strings.HasSuffix(path, AccountPath) {
		method = http.MethodGet
	}
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// triple is one decoded methodResponses entry.
type triple struct {
	Name string
	Args map[string]any
	ID   string
}

// callJMAP posts one batch and returns the decoded responses, asserting the two
// shape rules the frozen client depends on: exactly one [name, args, id] triple
// per call, in call order, echoing the call id.
func callJMAP(t *testing.T, handler *Handler, calls ...[]any) []triple {
	t.Helper()
	body := marshal(t, map[string]any{
		"using":       []string{CapabilityCore, CapabilityStalwart},
		"methodCalls": calls,
	})
	recorder := post(t, handler, JMAPPath, testToken, strings.NewReader(string(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("a method-level answer must still be HTTP 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		MethodResponses []json.RawMessage `json:"methodResponses"`
		SessionState    string            `json:"sessionState"`
	}
	decode(t, recorder.Body.Bytes(), &envelope)
	if envelope.SessionState == "" {
		t.Error("sessionState must be present and a string")
	}
	if len(envelope.MethodResponses) != len(calls) {
		t.Fatalf("got %d method responses for %d calls; the client reads them positionally", len(envelope.MethodResponses), len(calls))
	}
	responses := make([]triple, 0, len(calls))
	for index, raw := range envelope.MethodResponses {
		var parts []json.RawMessage
		decode(t, raw, &parts)
		if len(parts) != 3 {
			t.Fatalf("response %d was not a [name, args, id] triple: %s", index, raw)
		}
		var entry triple
		decode(t, parts[0], &entry.Name)
		decode(t, parts[1], &entry.Args)
		decode(t, parts[2], &entry.ID)
		want, ok := calls[index][2].(string)
		if !ok {
			t.Fatalf("call %d was built with a non-string id", index)
		}
		if entry.ID != want {
			t.Errorf("response %d echoed call id %q, want %q", index, entry.ID, want)
		}
		responses = append(responses, entry)
	}
	return responses
}

// methodErrorOf reads a `["error", …]` response.
func methodErrorOf(t *testing.T, entry triple) (kind, description string) {
	t.Helper()
	if entry.Name != "error" {
		t.Fatalf("wanted a method error, got %s: %v", entry.Name, entry.Args)
	}
	kind, ok := entry.Args["type"].(string)
	if !ok || kind == "" {
		t.Fatalf("a method error must carry a type: %v", entry.Args)
	}
	description, hasDescription := entry.Args["description"].(string)
	if !hasDescription {
		description = ""
	}
	return kind, description
}

// listOf reads the "list" of a /get response.
func listOf(t *testing.T, entry triple) []map[string]any {
	t.Helper()
	raw, ok := entry.Args["list"]
	if !ok {
		t.Fatalf("%s answered no list: %v", entry.Name, entry.Args)
	}
	values, ok := raw.([]any)
	if !ok {
		t.Fatalf("%s answered a list that was not an array: %v", entry.Name, raw)
	}
	objects := make([]map[string]any, 0, len(values))
	for _, value := range values {
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s answered a list entry that was not an object: %v", entry.Name, value)
		}
		objects = append(objects, object)
	}
	return objects
}

// idsOf reads the "ids" of a /query response.
func idsOf(t *testing.T, entry triple) []string {
	t.Helper()
	raw, ok := entry.Args["ids"].([]any)
	if !ok {
		t.Fatalf("%s answered no ids: %v", entry.Name, entry.Args)
	}
	ids := make([]string, 0, len(raw))
	for _, value := range raw {
		id, ok := value.(string)
		if !ok {
			t.Fatalf("%s answered a non-string id: %v", entry.Name, value)
		}
		ids = append(ids, id)
	}
	return ids
}

// createdID reads created.<creationId>.id out of a /set response.
func createdIDOf(t *testing.T, entry triple, creationID string) string {
	t.Helper()
	created, ok := entry.Args["created"].(map[string]any)
	if !ok {
		t.Fatalf("%s created nothing: %v", entry.Name, entry.Args)
	}
	object, ok := created[creationID].(map[string]any)
	if !ok {
		t.Fatalf("%s did not create %q: %v", entry.Name, creationID, entry.Args)
	}
	id, ok := object["id"].(string)
	if !ok || id == "" {
		// The client errors with "reported neither a created object nor an
		// error" when this is missing.
		t.Fatalf("%s reported a created object with no id: %v", entry.Name, entry.Args)
	}
	return id
}

// setErrorOf reads the first entry of notCreated/notUpdated/notDestroyed.
func setErrorOf(t *testing.T, entry triple, bucket, key string) map[string]any {
	t.Helper()
	entries, ok := entry.Args[bucket].(map[string]any)
	if !ok {
		t.Fatalf("%s answered no %s: %v", entry.Name, bucket, entry.Args)
	}
	failure, ok := entries[key].(map[string]any)
	if !ok {
		t.Fatalf("%s answered no %s for %q: %v", entry.Name, bucket, key, entry.Args)
	}
	return failure
}

// -----------------------------------------------------------------------------
// Authentication
// -----------------------------------------------------------------------------

// TestABadTokenIs401AndSaysNothingElse covers the one status the client maps
// onto "the mail engine rejected the API key". The body must not distinguish a
// missing credential from a wrong one, and must never echo either.
func TestABadTokenIs401AndSaysNothingElse(t *testing.T) {
	handler, _ := newHandler(t)

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{name: "no header at all", header: "", want: http.StatusUnauthorized},
		{name: "a wrong token", header: "Bearer not-the-token", want: http.StatusUnauthorized},
		{name: "a prefix of the token", header: "Bearer " + testToken[:8], want: http.StatusUnauthorized},
		{name: "an empty bearer", header: "Bearer ", want: http.StatusUnauthorized},
		{name: "the token without a scheme", header: testToken, want: http.StatusUnauthorized},
		{name: "basic auth", header: "Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+testToken)), want: http.StatusUnauthorized},
		{name: "the right token", header: "Bearer " + testToken, want: http.StatusOK},
		{name: "a lowercase scheme", header: "bearer " + testToken, want: http.StatusOK},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, path := range []string{JMAPPath, AccountPath} {
				method := http.MethodPost
				var body io.Reader = strings.NewReader(`{"using":[],"methodCalls":[]}`)
				if path == AccountPath {
					method, body = http.MethodGet, nil
				}
				request := httptest.NewRequest(method, path, body)
				if test.header != "" {
					request.Header.Set("Authorization", test.header)
				}
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)

				if recorder.Code != test.want {
					t.Errorf("%s answered %d, want %d", path, recorder.Code, test.want)
				}
				answer := recorder.Body.String()
				if strings.Contains(answer, testToken) {
					t.Errorf("%s echoed the token", path)
				}
				if test.want == http.StatusUnauthorized {
					// Nothing in the body may hint at whether a token exists,
					// what it looks like, or how it was wrong.
					for _, leak := range []string{"token", "missing", "expected", "configured"} {
						if strings.Contains(strings.ToLower(answer), leak) {
							t.Errorf("the 401 body mentions %q: %s", leak, answer)
						}
					}
				}
			}
		})
	}
}

// TestAnEmptyTokenIsRefusedAtConstruction: the client omits the Authorization
// header entirely when its own token is empty, so an empty token here would not
// fail loudly — it would serve an open management API.
func TestAnEmptyTokenIsRefusedAtConstruction(t *testing.T) {
	store := newStore(t)
	tests := []struct {
		name    string
		options Options
	}{
		{name: "no store", options: Options{Token: testToken, Hostname: testHostname}},
		{name: "no token", options: Options{Store: store, Hostname: testHostname}},
		{name: "a blank token", options: Options{Store: store, Token: "   ", Hostname: testHostname}},
		{name: "no hostname", options: Options{Store: store, Token: testToken}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewHandler(test.options); err == nil {
				t.Fatal("NewHandler accepted an unsafe configuration")
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Transport
// -----------------------------------------------------------------------------

// constantReader is an endless source of one byte, for the body-limit case. It
// is deliberately not a *strings.Reader so httptest cannot infer a
// Content-Length and the read limit itself is what stops it.
type constantReader byte

func (c constantReader) Read(p []byte) (int, error) {
	for index := range p {
		p[index] = byte(c)
	}
	return len(p), nil
}

// TestRequestLevelViolationsAreNot200. A body the client could not have meant
// to send has no JMAP representation, so it answers a non-200 and the client
// reports a transport error — which is the honest description. Everything with
// a JMAP representation must stay 200; that is covered by callJMAP.
func TestRequestLevelViolationsAreNot200(t *testing.T) {
	handler, _ := newHandler(t)

	tooManyCalls := make([][]any, 0, MaxCallsPerRequest+1)
	for index := range MaxCallsPerRequest + 1 {
		tooManyCalls = append(tooManyCalls, call(MethodSystemSettingsGet, map[string]any{}, fmt.Sprint(index)))
	}

	tests := []struct {
		name    string
		method  string
		path    string
		body    io.Reader
		length  int64
		want    int
		noToken bool
	}{
		{name: "an unknown path", method: http.MethodPost, path: "/nope", body: strings.NewReader("{}"), want: http.StatusNotFound},
		{name: "GET on the JMAP endpoint", method: http.MethodGet, path: JMAPPath, want: http.StatusMethodNotAllowed},
		{name: "POST on the account endpoint", method: http.MethodPost, path: AccountPath, body: strings.NewReader("{}"), want: http.StatusMethodNotAllowed},
		{name: "a body that is not JSON", method: http.MethodPost, path: JMAPPath, body: strings.NewReader("not json"), want: http.StatusBadRequest},
		{name: "a method call that is not a triple", method: http.MethodPost, path: JMAPPath,
			body: strings.NewReader(`{"using":[],"methodCalls":[["x:Domain/get",{}]]}`), want: http.StatusBadRequest},
		{name: "a method call with a numeric id", method: http.MethodPost, path: JMAPPath,
			body: strings.NewReader(`{"using":[],"methodCalls":[["x:Domain/get",{},0]]}`), want: http.StatusBadRequest},
		{name: "more calls than the batch limit", method: http.MethodPost, path: JMAPPath,
			body: strings.NewReader(string(marshal(t, map[string]any{"using": []string{}, "methodCalls": tooManyCalls}))), want: http.StatusBadRequest},
		{name: "a declared body over the limit", method: http.MethodPost, path: JMAPPath,
			body: strings.NewReader("{}"), length: MaxResponseBytes + 1, want: http.StatusRequestEntityTooLarge},
		{name: "an undeclared body over the limit", method: http.MethodPost, path: JMAPPath,
			body: io.LimitReader(constantReader('a'), MaxResponseBytes+1), want: http.StatusRequestEntityTooLarge},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, test.body)
			if test.length != 0 {
				request.ContentLength = test.length
			}
			if !test.noToken {
				request.Header.Set("Authorization", "Bearer "+testToken)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("got %d, want %d: %s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
}

// TestTheHandlerServesUnderAPathPrefix: MAIL_API_URL may carry one, and the
// client appends /jmap and /api/account to it.
func TestTheHandlerServesUnderAPathPrefix(t *testing.T) {
	handler, _ := newHandler(t, func(options *Options) { options.PathPrefix = "/mail/" })
	for _, path := range []string{"/mail" + JMAPPath, "/mail" + AccountPath} {
		recorder := post(t, handler, path, testToken, strings.NewReader(`{"using":[],"methodCalls":[]}`))
		if recorder.Code != http.StatusOK {
			t.Errorf("%s answered %d, want 200", path, recorder.Code)
		}
	}
	if recorder := post(t, handler, JMAPPath, testToken, strings.NewReader(`{}`)); recorder.Code != http.StatusNotFound {
		t.Errorf("the unprefixed path answered %d, want 404", recorder.Code)
	}
}

// -----------------------------------------------------------------------------
// Result references
// -----------------------------------------------------------------------------

// TestQueryGetChainReturnsTheRightObjects is the mandatory "#ids" behaviour:
// six of the client's calls chain a query and the get that consumes its ids
// into one request, and without the reference every list comes back empty.
func TestQueryGetChainReturnsTheRightObjects(t *testing.T) {
	handler, store := newHandler(t)
	ctx := context.Background()
	wanted := seedDomain(t, store, "probe.test")
	other := seedDomain(t, store, "elsewhere.test")

	account := NewAccount(wanted.ID, "mara", testClock)
	account.Description = "Mara"
	account.QuotaBytes = 5 << 30
	if _, err := store.CreateAccount(ctx, account); err != nil {
		t.Fatalf("seed the mailbox: %v", err)
	}

	t.Run("a domain query feeds the get that follows it", func(t *testing.T) {
		responses := callJMAP(t, handler,
			call(MethodDomainQuery, map[string]any{"filter": map[string]any{"name": "probe.test"}}, "0"),
			call(MethodDomainGet, map[string]any{
				"#ids":       reference("0", MethodDomainQuery),
				"properties": []string{"name", "isEnabled", "description", "dnsZoneFile", "createdAt"},
			}, "1"),
		)
		if ids := idsOf(t, responses[0]); !slices.Equal(ids, []string{wanted.ID}) {
			t.Fatalf("the query answered %v, want [%s]", ids, wanted.ID)
		}
		list := listOf(t, responses[1])
		if len(list) != 1 {
			t.Fatalf("the chained get answered %d objects, want 1: %v", len(list), list)
		}
		if list[0]["name"] != "probe.test" || list[0]["id"] != wanted.ID {
			t.Errorf("the chained get answered the wrong domain: %v", list[0])
		}
		if list[0]["id"] == other.ID {
			t.Error("the chained get ignored the filter")
		}
	})

	t.Run("an account query feeds the get that follows it", func(t *testing.T) {
		responses := callJMAP(t, handler,
			call(MethodAccountQuery, map[string]any{
				"filter": map[string]any{"@type": "User", "domainId": wanted.ID},
				"limit":  MaxObjectsPerCall,
			}, "0"),
			call(MethodAccountGet, map[string]any{
				"#ids":       reference("0", MethodAccountQuery),
				"properties": []string{"emailAddress", "description", "domainId", "quotas", "usedDiskQuota", "aliases", "createdAt"},
			}, "1"),
		)
		list := listOf(t, responses[1])
		if len(list) != 1 {
			t.Fatalf("the chained get answered %d mailboxes, want 1: %v", len(list), list)
		}
		if list[0]["emailAddress"] != "mara@probe.test" {
			t.Errorf("the chained get answered %v, want mara@probe.test", list[0]["emailAddress"])
		}
	})
}

// TestResultReferenceFailures. Every one of these must be a decodable
// invalidResultReference at that call's index — never a 500, and never a
// silently empty get, which would read as "the domain does not exist".
func TestResultReferenceFailures(t *testing.T) {
	handler, store := newHandler(t)
	seedDomain(t, store, "probe.test")

	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "no call has that id", args: map[string]any{"#ids": reference("9", MethodDomainQuery)}},
		{name: "the named method does not match", args: map[string]any{"#ids": reference("0", MethodAccountQuery)}},
		{name: "a path other than /ids", args: map[string]any{
			"#ids": map[string]any{"resultOf": "0", "name": MethodDomainQuery, "path": "/list/*/id"}}},
		{name: "both ids and a reference", args: map[string]any{
			"ids": []string{"whatever"}, "#ids": reference("0", MethodDomainQuery)}},
		{name: "a reference to a call that failed", args: map[string]any{"#ids": reference("x", "x:Nope/query")}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responses := callJMAP(t, handler,
				call(MethodDomainQuery, map[string]any{}, "0"),
				call("x:Nope/query", map[string]any{}, "x"),
				call(MethodDomainGet, test.args, "1"),
			)
			kind, description := methodErrorOf(t, responses[2])
			if kind != ErrTypeInvalidResultReference {
				t.Fatalf("got %q, want %q", kind, ErrTypeInvalidResultReference)
			}
			if description == "" {
				t.Error("the error carried no description")
			}
		})
	}
}

// TestAnUnknownMethodIsADecodableError. A name this server does not implement
// must answer the JMAP error the client expects. A 500 would surface as "the
// mail engine did not answer", which blames the network for a typo.
func TestAnUnknownMethodIsADecodableError(t *testing.T) {
	handler, _ := newHandler(t)

	responses := callJMAP(t, handler,
		call("x:Principal/get", map[string]any{"ids": []string{"a"}}, "0"),
		call("Core/echo", map[string]any{}, "1"),
		call(MethodSystemSettingsGet, map[string]any{"ids": []string{SingletonID}}, "2"),
	)
	for _, index := range []int{0, 1} {
		kind, description := methodErrorOf(t, responses[index])
		if kind != ErrTypeUnknownMethod {
			t.Errorf("response %d had type %q, want %q", index, kind, ErrTypeUnknownMethod)
		}
		if description == "" {
			t.Errorf("response %d carried no description", index)
		}
	}
	// The call after the failures still answers at its own index: the client
	// reads results positionally and names calls[index] as the failing method.
	if responses[2].Name != MethodSystemSettingsGet {
		t.Errorf("response 2 was %s, want %s", responses[2].Name, MethodSystemSettingsGet)
	}
	if got := listOf(t, responses[2]); len(got) != 1 || got[0]["defaultHostname"] != testHostname {
		t.Errorf("the settings singleton answered %v", got)
	}
}

// TestTheSettingsSingletonIsReadByID. A bare /get with no ids returns an empty
// list, which is why the client reads the singleton by id; and defaultHostname
// must be what this server calls itself, because the facade refuses every mail
// operation while it disagrees with MAIL_HOSTNAME.
func TestTheSettingsSingletonIsReadByID(t *testing.T) {
	handler, _ := newHandler(t, func(options *Options) {
		options.MailExchangers = []MailExchanger{{Priority: 10}, {Hostname: "backup.local.test", Priority: 20}}
	})

	responses := callJMAP(t, handler,
		call(MethodSystemSettingsGet, map[string]any{}, "0"),
		call(MethodSystemSettingsGet, map[string]any{"ids": []string{SingletonID}}, "1"),
		call(MethodSystemSettingsGet, map[string]any{"ids": []string{"not-the-singleton"}}, "2"),
	)
	if got := listOf(t, responses[0]); len(got) != 0 {
		t.Errorf("a bare get answered %v, want an empty list", got)
	}
	settings := listOf(t, responses[1])
	if len(settings) != 1 {
		t.Fatalf("the singleton get answered %d objects", len(settings))
	}
	if settings[0]["defaultHostname"] != testHostname {
		t.Errorf("defaultHostname is %v, want %s", settings[0]["defaultHostname"], testHostname)
	}
	exchangers, ok := settings[0]["mailExchangers"].(map[string]any)
	if !ok || len(exchangers) != 2 {
		t.Fatalf("mailExchangers is %v, want a map of two", settings[0]["mailExchangers"])
	}
	first, ok := exchangers["0"].(map[string]any)
	if !ok {
		t.Fatalf("mailExchangers is not keyed by decimal index: %v", exchangers)
	}
	if first["hostname"] != nil {
		t.Errorf("an unnamed exchanger must send hostname null, got %v", first["hostname"])
	}
	if got := listOf(t, responses[2]); len(got) != 0 {
		t.Errorf("an unknown id answered %v, want an empty list", got)
	}
}

// -----------------------------------------------------------------------------
// /set
// -----------------------------------------------------------------------------

// domainCreateBody is the object the client sends on every domain create.
func domainCreateBody(name, description string) map[string]any {
	return map[string]any{
		"name":        name,
		"isEnabled":   true,
		"description": description,
		"dkimManagement": map[string]any{
			"@type":            "Automatic",
			"algorithms":       map[string]bool{DkimAlgorithmEd25519: true, DkimAlgorithmRSA: true},
			"selectorTemplate": DefaultSelectorTemplate,
			"rotateAfter":      7_776_000_000,
			"retireAfter":      604_800_000,
			"deleteAfter":      2_592_000_000,
		},
		"dnsManagement":         map[string]any{"@type": "Manual"},
		"certificateManagement": map[string]any{"@type": "Manual"},
		"subAddressing":         map[string]any{"@type": "Enabled"},
		"reportAddressUri":      "mailto:postmaster@" + name,
		"allowRelaying":         false,
		"aliases":               map[string]any{},
		"catchAllAddress":       nil,
	}
}

// accountCreateBody is the object the client sends on every mailbox create.
func accountCreateBody(domainID, localPart, description, secret string) map[string]any {
	return map[string]any{
		"@type":            "User",
		"name":             localPart,
		"domainId":         domainID,
		"description":      description,
		"credentials":      map[string]any{"0": map[string]any{"@type": "Password", "secret": secret}},
		"roles":            map[string]any{"@type": "User"},
		"permissions":      map[string]any{"@type": "Inherit"},
		"quotas":           map[string]any{"maxDiskQuota": 5 << 30},
		"aliases":          map[string]any{},
		"locale":           "en-US",
		"memberGroupIds":   map[string]any{},
		"encryptionAtRest": map[string]any{"@type": "Disabled"},
	}
}

// TestSetCreateRoundTrip is the create-then-read the client performs after
// every create, for all three creatable objects.
func TestSetCreateRoundTrip(t *testing.T) {
	handler, store := newHandler(t)
	ctx := context.Background()

	// A domain, with the description the facade uses to decide ownership: it
	// adopts a domain only when the description equals "simple zone "+zoneID
	// exactly, so it must round-trip byte for byte.
	const description = "simple zone z-abc123"
	created := callJMAP(t, handler,
		call(MethodDomainSet, map[string]any{"create": map[string]any{"d1": domainCreateBody("acme.dev", description)}}, "0"),
	)
	domainID := createdIDOf(t, created[0], "d1")

	read := callJMAP(t, handler,
		call(MethodDomainGet, map[string]any{
			"ids":        []string{domainID},
			"properties": []string{"name", "isEnabled", "description", "dnsZoneFile", "createdAt"},
		}, "0"),
	)
	domain := listOf(t, read[0])
	if len(domain) != 1 {
		t.Fatalf("the domain read back %d objects", len(domain))
	}
	if domain[0]["description"] != description {
		t.Errorf("description came back %q, want %q byte for byte", domain[0]["description"], description)
	}
	if domain[0]["isEnabled"] != true {
		t.Errorf("isEnabled came back %v; note it is not spelled enabled", domain[0]["isEnabled"])
	}
	if domain[0]["createdAt"] != testClock.Format(time.RFC3339) {
		t.Errorf("createdAt came back %v, want RFC 3339", domain[0]["createdAt"])
	}

	// A duplicate name is the primaryKeyViolation the facade turns into "that
	// name is already taken", not a transport error.
	duplicate := callJMAP(t, handler,
		call(MethodDomainSet, map[string]any{"create": map[string]any{"d1": domainCreateBody("acme.dev", description)}}, "0"),
	)
	failure := setErrorOf(t, duplicate[0], "notCreated", "d1")
	if failure["type"] != ErrTypePrimaryKeyViolation {
		t.Errorf("a duplicate domain answered %v, want %s", failure["type"], ErrTypePrimaryKeyViolation)
	}

	// A mailbox.
	createdAccount := callJMAP(t, handler,
		call(MethodAccountSet, map[string]any{
			"create": map[string]any{"u1": accountCreateBody(domainID, "mara", "Mara", testSecret)},
		}, "0"),
	)
	accountID := createdIDOf(t, createdAccount[0], "u1")

	readAccount := callJMAP(t, handler,
		call(MethodAccountGet, map[string]any{
			"ids":        []string{accountID},
			"properties": []string{"emailAddress", "description", "domainId", "quotas", "usedDiskQuota", "aliases", "createdAt"},
		}, "0"),
	)
	account := listOf(t, readAccount[0])
	if len(account) != 1 {
		t.Fatalf("the mailbox read back %d objects", len(account))
	}
	if account[0]["emailAddress"] != "mara@acme.dev" {
		t.Errorf("emailAddress is %v, want mara@acme.dev composed from the local part and the domain", account[0]["emailAddress"])
	}
	if quotas, ok := account[0]["quotas"].(map[string]any); !ok || quotas["maxDiskQuota"] != float64(5<<30) {
		t.Errorf("quotas came back %v", account[0]["quotas"])
	}

	// The credential was hashed on arrival and the plaintext dropped.
	stored, err := store.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatalf("read the stored mailbox: %v", err)
	}
	if !stored.Password.Verify(testSecret) {
		t.Error("the credential was not hashed into the stored mailbox")
	}

	// A mailing list, which the client does not re-read: created.l1.id is the
	// only thing that must come back, but the stored object must match what was
	// sent or the next ListLists disagrees with the console.
	createdList := callJMAP(t, handler,
		call(MethodListSet, map[string]any{"create": map[string]any{"l1": map[string]any{
			"name":        "team",
			"domainId":    domainID,
			"description": stalwart.ListDescription,
			"recipients":  map[string]bool{"someone@example.com": true, "mara@acme.dev": true},
			"aliases":     map[string]any{},
		}}}, "0"),
	)
	listID := createdIDOf(t, createdList[0], "l1")

	readList := callJMAP(t, handler,
		call(MethodListGet, map[string]any{
			"ids":        []string{listID},
			"properties": []string{"name", "domainId", "description", "recipients", "emailAddress"},
		}, "0"),
	)
	list := listOf(t, readList[0])
	if len(list) != 1 {
		t.Fatalf("the mailing list read back %d objects", len(list))
	}
	if list[0]["emailAddress"] != "team@acme.dev" {
		t.Errorf("emailAddress is %v, want team@acme.dev", list[0]["emailAddress"])
	}
	recipients, ok := list[0]["recipients"].(map[string]any)
	if !ok || recipients["someone@example.com"] != true || recipients["mara@acme.dev"] != true {
		t.Errorf("recipients came back %v, want a map address -> true", list[0]["recipients"])
	}

	// The address space is shared: a mailbox cannot take the list's address.
	clash := callJMAP(t, handler,
		call(MethodAccountSet, map[string]any{
			"create": map[string]any{"u1": accountCreateBody(domainID, "team", "", testSecret)},
		}, "0"),
	)
	clashError := setErrorOf(t, clash[0], "notCreated", "u1")
	if clashError["type"] != ErrTypePrimaryKeyViolation {
		t.Errorf("a clash with a mailing list answered %v, want %s", clashError["type"], ErrTypePrimaryKeyViolation)
	}
	if properties, ok := clashError["properties"].([]any); !ok || len(properties) == 0 || properties[0] != "email" {
		t.Errorf("the clash blamed %v, want [email]", clashError["properties"])
	}
}

// TestSetRejectionsAreShapedForTheFacade covers the per-object refusals the
// facade reasons about by type.
func TestSetRejectionsAreShapedForTheFacade(t *testing.T) {
	handler, store := newHandler(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "probe.test")
	account := NewAccount(domain.ID, "mara", testClock)
	if _, err := store.CreateAccount(ctx, account); err != nil {
		t.Fatalf("seed the mailbox: %v", err)
	}

	tests := []struct {
		name       string
		method     string
		args       map[string]any
		bucket     string
		key        string
		wantType   string
		wantBlames string
	}{
		{
			name:   "a domain with no name",
			method: MethodDomainSet,
			args:   map[string]any{"create": map[string]any{"d1": map[string]any{"name": ""}}},
			bucket: "notCreated", key: "d1",
			wantType: ErrTypeInvalidProperties, wantBlames: "name",
		},
		{
			name:   "a patch of a property that cannot be patched",
			method: MethodDomainSet,
			args:   map[string]any{"update": map[string]any{domain.ID: map[string]any{"name": "other.test"}}},
			bucket: "notUpdated", key: domain.ID,
			wantType: ErrTypeInvalidPatch, wantBlames: "name",
		},
		{
			name:   "a patch of a domain that is gone",
			method: MethodDomainSet,
			args:   map[string]any{"update": map[string]any{"dnope": map[string]any{"isEnabled": false}}},
			bucket: "notUpdated", key: "dnope",
			wantType: ErrTypeNotFound,
		},
		{
			name:   "a mailbox in a domain that does not exist",
			method: MethodAccountSet,
			args:   map[string]any{"create": map[string]any{"u1": accountCreateBody("dnope", "mara", "", testSecret)}},
			bucket: "notCreated", key: "u1",
			wantType: ErrTypeInvalidProperties, wantBlames: "domainId",
		},
		{
			name:   "a mailbox with no credential",
			method: MethodAccountSet,
			args: map[string]any{"create": map[string]any{"u1": map[string]any{
				"@type": "User", "name": "nobody", "domainId": domain.ID,
			}}},
			bucket: "notCreated", key: "u1",
			wantType: ErrTypeInvalidProperties, wantBlames: "credentials",
		},
		{
			name:   "a mailing list with no recipients",
			method: MethodListSet,
			args:   map[string]any{"create": map[string]any{"l1": map[string]any{"name": "team", "domainId": domain.ID, "recipients": map[string]bool{}}}},
			bucket: "notCreated", key: "l1",
			wantType: ErrTypeInvalidProperties, wantBlames: "recipients",
		},
		{
			name:   "a signature this server did not generate",
			method: MethodDkimSet,
			args:   map[string]any{"create": map[string]any{"k1": map[string]any{"selector": "v1-rsa-20260903"}}},
			bucket: "notCreated", key: "k1",
			wantType: ErrTypeInvalidArguments,
		},
		{
			name:   "an unknown credential type",
			method: MethodAccountSet,
			args:   map[string]any{"update": map[string]any{account.ID: map[string]any{"credentials": map[string]any{"0": map[string]any{"@type": "OTPAuth", "secret": testSecret}}}}},
			bucket: "notUpdated", key: account.ID,
			wantType: ErrTypeInvalidProperties, wantBlames: "credentials",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responses := callJMAP(t, handler, call(test.method, test.args, "0"))
			if responses[0].Name != test.method {
				t.Fatalf("the call failed at the method level: %v", responses[0].Args)
			}
			failure := setErrorOf(t, responses[0], test.bucket, test.key)
			if failure["type"] != test.wantType {
				t.Errorf("type is %v, want %s", failure["type"], test.wantType)
			}
			if test.wantBlames != "" {
				properties, ok := failure["properties"].([]any)
				if !ok || len(properties) == 0 || properties[0] != test.wantBlames {
					t.Errorf("blamed %v, want [%s]", failure["properties"], test.wantBlames)
				}
			}
			body := string(marshal(t, responses[0].Args))
			if strings.Contains(body, testSecret) {
				t.Errorf("a rejection echoed the credential: %s", body)
			}
		})
	}
}

// TestDestroyRefusesALinkedDomainAndForgivesAMissingOne. destroyDomain deletes
// the signatures, then the domain, and retries the pair once on objectIsLinked;
// a destroy of an id that is not there must be success or the unbind never
// completes.
func TestDestroyRefusesALinkedDomainAndForgivesAMissingOne(t *testing.T) {
	handler, store := newHandler(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "probe.test")
	if _, err := store.CreateAccount(ctx, NewAccount(domain.ID, "mara", testClock)); err != nil {
		t.Fatalf("seed the mailbox: %v", err)
	}
	key := NewDkimKey(domain.ID, "v1-rsa-20260903", DkimAlgorithmRSA, testClock)
	key.Stage = DkimStageActive
	if _, err := store.PutDkimKey(ctx, key); err != nil {
		t.Fatalf("seed the signature: %v", err)
	}

	refused := callJMAP(t, handler, call(MethodDomainSet, map[string]any{"destroy": []string{domain.ID}}, "0"))
	failure := setErrorOf(t, refused[0], "notDestroyed", domain.ID)
	if failure["type"] != ErrTypeObjectIsLinked {
		t.Fatalf("type is %v, want %s", failure["type"], ErrTypeObjectIsLinked)
	}
	linked, ok := failure["linkedObjects"].([]any)
	if !ok || len(linked) != 2 {
		t.Fatalf("linkedObjects is %v, want the mailbox and the signature", failure["linkedObjects"])
	}
	kinds := map[string]bool{}
	for _, entry := range linked {
		object, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a linked object was not an object: %v", entry)
		}
		name, ok := object["object"].(string)
		if !ok {
			t.Fatalf("a linked object carried no object name: %v", object)
		}
		kinds[name] = true
		if object["id"] == "" {
			t.Error("a linked object carried no id")
		}
	}
	if !kinds[ObjectAccount] || !kinds[ObjectDkimSignature] {
		t.Errorf("linkedObjects named %v, want Account and DkimSignature", kinds)
	}

	// Clear the children the way the facade does, then destroy for real. A
	// destroy of something already gone is success both times.
	cleared := callJMAP(t, handler,
		call(MethodAccountSet, map[string]any{"destroy": []string{AccountID(domain.ID, "mara")}}, "0"),
		call(MethodDkimSet, map[string]any{"destroy": []string{key.ID, "knope"}}, "1"),
		call(MethodDomainSet, map[string]any{"destroy": []string{domain.ID}}, "2"),
		call(MethodDomainSet, map[string]any{"destroy": []string{domain.ID}}, "3"),
	)
	for index, response := range cleared {
		if _, refusedAny := response.Args["notDestroyed"]; refusedAny {
			t.Errorf("call %d refused a destroy: %v", index, response.Args)
		}
		if _, ok := response.Args["destroyed"]; !ok {
			t.Errorf("call %d reported nothing destroyed: %v", index, response.Args)
		}
	}
	if _, err := store.GetDomain(ctx, domain.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the domain survived the destroy: %v", err)
	}
}

// TestAliasesReplaceTheWholeSet. x:Account/set update replaces the aliases map
// rather than merging into it: the client read-modify-writes the set it just
// read, so an alias missing from the patch is meant to stop resolving.
func TestAliasesReplaceTheWholeSet(t *testing.T) {
	handler, store := newHandler(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "probe.test")
	created := callJMAP(t, handler,
		call(MethodAccountSet, map[string]any{"create": map[string]any{"u1": accountCreateBody(domain.ID, "mara", "Mara", testSecret)}}, "0"),
	)
	accountID := createdIDOf(t, created[0], "u1")

	patched := callJMAP(t, handler, call(MethodAccountSet, map[string]any{"update": map[string]any{accountID: map[string]any{
		"aliases": map[string]any{
			"0": map[string]any{"name": "hello", "domainId": domain.ID, "description": nil, "enabled": true},
			"1": map[string]any{"name": "sales", "domainId": domain.ID, "description": "sales", "enabled": false},
		},
	}}}, "0"))
	if _, ok := patched[0].Args["updated"]; !ok {
		t.Fatalf("the patch was not applied: %v", patched[0].Args)
	}

	stored, err := store.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatalf("read the mailbox: %v", err)
	}
	if len(stored.Aliases) != 2 {
		t.Fatalf("stored %d aliases, want 2: %v", len(stored.Aliases), stored.Aliases)
	}
	if _, err := store.AccountByAlias(ctx, domain.ID, "hello"); err != nil {
		t.Errorf("the alias does not resolve: %v", err)
	}

	// Dropping one from the map deletes it.
	callJMAP(t, handler, call(MethodAccountSet, map[string]any{"update": map[string]any{accountID: map[string]any{
		"aliases": map[string]any{"0": map[string]any{"name": "hello", "domainId": domain.ID, "description": nil, "enabled": true}},
	}}}, "0"))
	if _, err := store.AccountByAlias(ctx, domain.ID, "sales"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a dropped alias still resolves: %v", err)
	}

	// And the wire form is a map keyed by decimal index strings, with
	// "enabled", not "isEnabled".
	read := callJMAP(t, handler, call(MethodAccountGet, map[string]any{
		"ids":        []string{accountID},
		"properties": []string{"emailAddress", "aliases"},
	}, "0"))
	aliases, ok := listOf(t, read[0])[0]["aliases"].(map[string]any)
	if !ok {
		t.Fatalf("aliases is not a map: %v", listOf(t, read[0])[0]["aliases"])
	}
	entry, ok := aliases["0"].(map[string]any)
	if !ok {
		t.Fatalf("aliases is not keyed by decimal index: %v", aliases)
	}
	if entry["name"] != "hello" || entry["enabled"] != true {
		t.Errorf("the alias came back %v", entry)
	}
	if _, wrong := entry["isEnabled"]; wrong {
		t.Error("an alias spells it enabled, not isEnabled")
	}
}

// TestPagingHonoursLimitAndPosition. ListDomains and ListLists loop
// position += 500 and stop when a page returns fewer than 500 ids, so a server
// that ignored position would loop forever.
func TestPagingHonoursLimitAndPosition(t *testing.T) {
	handler, store := newHandler(t)
	names := []string{"a.test", "b.test", "c.test", "d.test", "e.test"}
	ids := make([]string, 0, len(names))
	for _, name := range names {
		ids = append(ids, seedDomain(t, store, name).ID)
	}

	tests := []struct {
		name     string
		args     map[string]any
		want     []string
		position float64
	}{
		{name: "the first page", args: map[string]any{"limit": 2, "position": 0}, want: ids[0:2]},
		{name: "the second page", args: map[string]any{"limit": 2, "position": 2}, want: ids[2:4], position: 2},
		{name: "the last, short page", args: map[string]any{"limit": 2, "position": 4}, want: ids[4:5], position: 4},
		{name: "past the end", args: map[string]any{"limit": 2, "position": 99}, want: []string{}, position: 99},
		{name: "no limit at all", args: map[string]any{}, want: ids},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responses := callJMAP(t, handler, call(MethodDomainQuery, test.args, "0"))
			if got := idsOf(t, responses[0]); !slices.Equal(got, test.want) {
				t.Errorf("got %v, want %v", got, test.want)
			}
			if responses[0].Args["position"] != test.position {
				t.Errorf("position came back %v, want %v", responses[0].Args["position"], test.position)
			}
		})
	}

	t.Run("a negative position is refused", func(t *testing.T) {
		responses := callJMAP(t, handler, call(MethodDomainQuery, map[string]any{"position": -1}, "0"))
		if kind, _ := methodErrorOf(t, responses[0]); kind != ErrTypeInvalidArguments {
			t.Errorf("got %q, want %q", kind, ErrTypeInvalidArguments)
		}
	})
}

// TestUnsupportedFiltersAreNamedAsSuch. Silently ignoring a filter would answer
// with objects the caller asked not to see; the two filters the client does
// send must work server-side.
func TestUnsupportedFiltersAreNamedAsSuch(t *testing.T) {
	handler, store := newHandler(t)
	domain := seedDomain(t, store, "probe.test")

	tests := []struct {
		name       string
		method     string
		filter     map[string]any
		wantError  bool
		wantIDs    []string
		wantNoneOf []string
	}{
		{name: "a domain by name", method: MethodDomainQuery, filter: map[string]any{"name": "probe.test"}, wantIDs: []string{domain.ID}},
		{name: "a domain by a name that is not there", method: MethodDomainQuery, filter: map[string]any{"name": "nope.test"}, wantIDs: []string{}},
		{name: "a domain by something else", method: MethodDomainQuery, filter: map[string]any{"description": "x"}, wantError: true},
		{name: "signatures by domain", method: MethodDkimQuery, filter: map[string]any{"domainId": domain.ID}, wantIDs: []string{}},
		{name: "signatures by selector", method: MethodDkimQuery, filter: map[string]any{"selector": "v1"}, wantError: true},
		{name: "mailboxes by type and domain", method: MethodAccountQuery, filter: map[string]any{"@type": "User", "domainId": domain.ID}, wantIDs: []string{}},
		{name: "mailboxes by name", method: MethodAccountQuery, filter: map[string]any{"name": "mara"}, wantError: true},
		// The real server answers unsupportedFilter for a domainId filter on a
		// mailing list, which is why the client queries unfiltered and matches
		// the domain itself. Answering it here would make this server behave
		// differently from the one the client was written against.
		{name: "mailing lists by domain", method: MethodListQuery, filter: map[string]any{"domainId": domain.ID}, wantError: true},
		{name: "the queue by return path", method: MethodQueueQuery, filter: map[string]any{"returnPath": "x@y.test"}, wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responses := callJMAP(t, handler, call(test.method, map[string]any{"filter": test.filter}, "0"))
			if test.wantError {
				if kind, _ := methodErrorOf(t, responses[0]); kind != ErrTypeUnsupportedFilter {
					t.Fatalf("got %q, want %q", kind, ErrTypeUnsupportedFilter)
				}
				return
			}
			if got := idsOf(t, responses[0]); !slices.Equal(got, test.wantIDs) {
				t.Errorf("got %v, want %v", got, test.wantIDs)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// DKIM provisioning and the zone file
// -----------------------------------------------------------------------------

// fakeProvisioner writes the signatures a real generator would, so a test can
// exercise the create path without owning the DKIM phase's code.
func fakeProvisioner(store *Store, seen *[]string, fail error) func(context.Context, Domain, []string, string) error {
	return func(ctx context.Context, domain Domain, algorithms []string, template string) error {
		*seen = append(*seen, template)
		*seen = append(*seen, algorithms...)
		if fail != nil {
			return fail
		}
		for _, algorithm := range algorithms {
			key := NewDkimKey(domain.ID, "v1-"+DkimSelectorTag[algorithm]+"-20260903", algorithm, testClock)
			key.Stage = DkimStageActive
			key.PublicKey = base64.StdEncoding.EncodeToString([]byte("public key for " + algorithm))
			if _, err := store.PutDkimKey(ctx, key); err != nil {
				return err
			}
		}
		return nil
	}
}

// TestDomainCreateGeneratesTheSigningKeys. BindMailDomain waits at most 20 s
// for one active signature per algorithm, so the keys must exist by the time
// the create answers.
func TestDomainCreateGeneratesTheSigningKeys(t *testing.T) {
	var seen []string
	handler, store := newHandler(t, func(options *Options) {
		options.ProvisionDkim = fakeProvisioner(options.Store, &seen, nil)
	})

	created := callJMAP(t, handler,
		call(MethodDomainSet, map[string]any{"create": map[string]any{"d1": domainCreateBody("acme.dev", "simple zone z-1")}}, "0"),
	)
	domainID := createdIDOf(t, created[0], "d1")

	if want := []string{DefaultSelectorTemplate, DkimAlgorithmEd25519, DkimAlgorithmRSA}; !slices.Equal(seen, want) {
		t.Errorf("the provisioner was asked for %v, want %v", seen, want)
	}

	responses := callJMAP(t, handler,
		call(MethodDkimQuery, map[string]any{"filter": map[string]any{"domainId": domainID}}, "0"),
		call(MethodDkimGet, map[string]any{
			"#ids":       reference("0", MethodDkimQuery),
			"properties": []string{"selector", "@type", "stage", "createdAt", "publicKey"},
		}, "1"),
	)
	signatures := listOf(t, responses[1])
	if len(signatures) != 2 {
		t.Fatalf("the domain has %d signatures, want one per algorithm", len(signatures))
	}
	algorithms := map[string]bool{}
	for _, signature := range signatures {
		algorithm, ok := signature["@type"].(string)
		if !ok {
			t.Fatalf("a signature carried no @type: %v", signature)
		}
		algorithms[algorithm] = true
		if signature["stage"] != DkimStageActive {
			t.Errorf("%v is staged %v, want active", signature["selector"], signature["stage"])
		}
		if signature["publicKey"] == "" {
			t.Errorf("%v carries no public key", signature["selector"])
		}
		if _, leaked := signature["privateKey"]; leaked {
			t.Error("a signature carried a private key")
		}
	}
	if !algorithms[DkimAlgorithmEd25519] || !algorithms[DkimAlgorithmRSA] {
		t.Errorf("got algorithms %v, want both; the facade waits for one active signature of each", algorithms)
	}

	_ = store
}

// TestAFailedProvisionDoesNotStrandADomain. A create that answers notCreated
// while the domain survives would be a trap: the retry is answered
// primaryKeyViolation and the operator is told the name is taken.
func TestAFailedProvisionDoesNotStrandADomain(t *testing.T) {
	var seen []string
	handler, store := newHandler(t, func(options *Options) {
		options.ProvisionDkim = fakeProvisioner(options.Store, &seen, errors.New("no entropy"))
	})

	responses := callJMAP(t, handler,
		call(MethodDomainSet, map[string]any{"create": map[string]any{"d1": domainCreateBody("acme.dev", "simple zone z-1")}}, "0"),
	)
	failure := setErrorOf(t, responses[0], "notCreated", "d1")
	if failure["type"] == ErrTypePrimaryKeyViolation || failure["type"] == ErrTypeNotFound {
		t.Errorf("type is %v, which the facade would misread", failure["type"])
	}
	if strings.Contains(fmt.Sprint(failure["description"]), "entropy") {
		t.Errorf("the failure leaked the internal cause: %v", failure["description"])
	}
	if _, err := store.FindDomain(context.Background(), "acme.dev"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the domain outlived a failed create: %v", err)
	}
}

// TestTheZoneFileIsRenderedOnlyWhenAsked. dnsZoneFile is the expensive property
// and the list query deliberately omits it.
func TestTheZoneFileIsRenderedOnlyWhenAsked(t *testing.T) {
	renders := 0
	handler, store := newHandler(t, func(options *Options) {
		options.RenderZoneFile = func(_ context.Context, domain Domain) (string, error) {
			renders++
			return "v1-rsa-20260903._domainkey." + domain.Name + ". IN TXT \"v=DKIM1; k=rsa; h=sha256; p=AAAA\"\n", nil
		}
	})
	domain := seedDomain(t, store, "probe.test")

	list := callJMAP(t, handler, call(MethodDomainGet, map[string]any{
		"ids":        []string{domain.ID},
		"properties": []string{"name", "isEnabled", "description", "createdAt"},
	}, "0"))
	if _, present := listOf(t, list[0])[0]["dnsZoneFile"]; present {
		t.Error("the list properties do not ask for dnsZoneFile")
	}
	if renders != 0 {
		t.Errorf("the zone file was rendered %d times for a list read", renders)
	}

	full := callJMAP(t, handler, call(MethodDomainGet, map[string]any{
		"ids":        []string{domain.ID},
		"properties": []string{"name", "isEnabled", "description", "dnsZoneFile", "createdAt"},
	}, "0"))
	zone, ok := listOf(t, full[0])[0]["dnsZoneFile"].(string)
	if !ok {
		t.Fatalf("dnsZoneFile was not a string: %v", listOf(t, full[0])[0]["dnsZoneFile"])
	}
	if !strings.Contains(zone, "_domainkey.probe.test.") {
		t.Errorf("dnsZoneFile came back %q", zone)
	}
	if renders != 1 {
		t.Errorf("the zone file was rendered %d times, want once", renders)
	}
}

// -----------------------------------------------------------------------------
// The queue
// -----------------------------------------------------------------------------

func TestTheQueueChainsQueryIntoGet(t *testing.T) {
	message := QueuedMessage{
		V: DocVersion, ID: "j1", ReturnPath: "sender@example.org",
		CreatedAt: testClock, NextRetry: testClock.Add(5 * time.Minute),
		Recipients: []QueuedRecipient{{
			Address: "someone@example.com",
			// The customer's own address travels in orcpt for a forwarder
			// fan-out; without it the message is attributable to nobody.
			ORCPT:      "team@probe.test",
			Status:     QueueStatusTemporaryFailure,
			QueueName:  "remote",
			RetryCount: 2,
			RetryDue:   testClock.Add(5 * time.Minute),
		}},
	}
	handler, _ := newHandler(t, func(options *Options) {
		options.ListQueue = func(context.Context) ([]QueuedMessage, error) {
			return []QueuedMessage{message}, nil
		}
	})

	responses := callJMAP(t, handler,
		call(MethodQueueQuery, map[string]any{"limit": MaxObjectsPerCall}, "0"),
		call(MethodQueueGet, map[string]any{
			"#ids":       reference("0", MethodQueueQuery),
			"properties": []string{"returnPath", "recipients", "createdAt", "nextRetry"},
		}, "1"),
	)
	if ids := idsOf(t, responses[0]); !slices.Equal(ids, []string{"j1"}) {
		t.Fatalf("the queue query answered %v", ids)
	}
	messages := listOf(t, responses[1])
	if len(messages) != 1 {
		t.Fatalf("the chained get answered %d messages", len(messages))
	}
	recipients, ok := messages[0]["recipients"].(map[string]any)
	if !ok {
		t.Fatalf("recipients is %v, want a map keyed by address", messages[0]["recipients"])
	}
	recipient, ok := recipients["someone@example.com"].(map[string]any)
	if !ok {
		t.Fatalf("recipients is not keyed by the recipient address: %v", recipients)
	}
	if recipient["orcpt"] != "rfc822;team@probe.test" {
		t.Errorf("orcpt is %v, want the rfc822; prefix the client strips", recipient["orcpt"])
	}
	status, ok := recipient["status"].(map[string]any)
	if !ok || status["@type"] != QueueStatusTemporaryFailure {
		t.Errorf("status is %v", recipient["status"])
	}
}

// TestTheQueueDefaultsToTheStore. With no hook, the queue read is the store's
// own — the one the delivery path spools into. A handler that answered an empty
// queue instead would tell the operator "0 queued" about mail that is going
// nowhere.
func TestTheQueueDefaultsToTheStore(t *testing.T) {
	handler, store := newHandler(t)
	ctx := context.Background()

	empty := callJMAP(t, handler, call(MethodQueueQuery, map[string]any{"limit": MaxObjectsPerCall}, "0"))
	if ids := idsOf(t, empty[0]); len(ids) != 0 {
		t.Errorf("got %v, want no ids", ids)
	}

	if _, err := store.PutQueuedMessage(ctx, QueuedMessage{
		V: DocVersion, ID: "j1", ReturnPath: "sender@example.org", CreatedAt: testClock,
		Recipients: []QueuedRecipient{{Address: "someone@example.com", Status: QueueStatusScheduled}},
	}); err != nil {
		t.Fatalf("spool a message: %v", err)
	}

	responses := callJMAP(t, handler,
		call(MethodQueueQuery, map[string]any{"limit": MaxObjectsPerCall}, "0"),
		call(MethodQueueGet, map[string]any{
			"#ids":       reference("0", MethodQueueQuery),
			"properties": []string{"returnPath", "recipients", "createdAt", "nextRetry"},
		}, "1"),
	)
	if ids := idsOf(t, responses[0]); !slices.Equal(ids, []string{"j1"}) {
		t.Fatalf("the query answered %v, want the spooled message", ids)
	}
	messages := listOf(t, responses[1])
	if len(messages) != 1 || messages[0]["returnPath"] != "sender@example.org" {
		t.Errorf("the chained get answered %v", messages)
	}
	if messages[0]["nextRetry"] != nil {
		t.Errorf("an unset nextRetry must be null, got %v", messages[0]["nextRetry"])
	}
}

// -----------------------------------------------------------------------------
// Secrets
// -----------------------------------------------------------------------------

// TestNoResponseCarriesACredential sweeps every response a full lifecycle
// produces and asserts that neither the mailbox passphrase nor the admin token
// appears anywhere in the bytes.
func TestNoResponseCarriesACredential(t *testing.T) {
	var seen []string
	handler, _ := newHandler(t, func(options *Options) {
		options.ProvisionDkim = fakeProvisioner(options.Store, &seen, nil)
	})

	domain := callJMAP(t, handler,
		call(MethodDomainSet, map[string]any{"create": map[string]any{"d1": domainCreateBody("probe.test", "simple zone z-probe")}}, "0"),
	)
	domainID := createdIDOf(t, domain[0], "d1")
	account := callJMAP(t, handler,
		call(MethodAccountSet, map[string]any{"create": map[string]any{"u1": accountCreateBody(domainID, "mara", "Mara", testSecret)}}, "0"),
	)
	accountID := createdIDOf(t, account[0], "u1")

	batches := [][][]any{
		{call(MethodAccountGet, map[string]any{"ids": []string{accountID}}, "0")},
		{call(MethodAccountGet, map[string]any{"ids": []string{accountID}, "properties": []string{"emailAddress", "aliases", "quotas"}}, "0")},
		{call(MethodAccountQuery, map[string]any{"filter": map[string]any{"domainId": domainID}}, "0")},
		{call(MethodAccountSet, map[string]any{"update": map[string]any{accountID: map[string]any{
			"credentials": map[string]any{"0": map[string]any{"@type": "Password", "secret": testSecret + "-new"}},
		}}}, "0")},
		{call(MethodDkimGet, map[string]any{"ids": []string{DkimID(domainID, "v1-rsa-20260903")}}, "0")},
		{call(MethodDomainGet, map[string]any{"ids": []string{domainID}}, "0")},
	}
	for index, calls := range batches {
		body := marshal(t, map[string]any{"using": []string{}, "methodCalls": calls})
		recorder := post(t, handler, JMAPPath, testToken, strings.NewReader(string(body)))
		answer := recorder.Body.String()
		for _, secret := range []string{testSecret, testSecret + "-new", testToken} {
			if strings.Contains(answer, secret) {
				t.Errorf("batch %d leaked a secret: %s", index, answer)
			}
		}
		// The stored digest and salt travel as base64 inside the store; no
		// credential-shaped property may appear at all.
		for _, property := range []string{"credentials", "secret", "password", "privateKey"} {
			if strings.Contains(answer, property) {
				t.Errorf("batch %d answered a %s property: %s", index, property, answer)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// The frozen client, end to end
// -----------------------------------------------------------------------------

// handlerTransport drives the handler in-process. No socket is opened and no
// listener is started: the request goes straight into ServeHTTP, so the test
// exercises the real, frozen client against the real handler.
type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

func newFacadeClient(t *testing.T, handler http.Handler, token string) *stalwart.Client {
	t.Helper()
	client, err := stalwart.New("https://mail.invalid", token,
		stalwart.WithHTTPClient(&http.Client{Transport: handlerTransport{handler: handler}}))
	if err != nil {
		t.Fatalf("build the client: %v", err)
	}
	return client
}

// TestTheFrozenClientDrivesThisServer is the test that matters: every call the
// facade makes, made by the facade's own client, against this handler. A wrong
// method name, JSON tag or result reference fails here rather than silently
// degrading a customer's mail binding.
func TestTheFrozenClientDrivesThisServer(t *testing.T) {
	var seen []string
	handler, store := newHandler(t, func(options *Options) {
		options.ProvisionDkim = fakeProvisioner(options.Store, &seen, nil)
		options.RenderZoneFile = func(_ context.Context, domain Domain) (string, error) {
			return "v1-rsa-20260903._domainkey." + domain.Name + ". IN TXT \"v=DKIM1; k=rsa; h=sha256; p=AAAA\"\n", nil
		}
	})
	client := newFacadeClient(t, handler, testToken)
	ctx := context.Background()

	// The self-check the facade runs at startup.
	info, err := client.Account(ctx)
	if err != nil {
		t.Fatalf("read the account info: %v", err)
	}
	if info.Edition == "" || len(info.Permissions) == 0 {
		t.Fatalf("the account info was empty: %+v", info)
	}

	// The gate everything else depends on.
	settings, err := client.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("read the system settings: %v", err)
	}
	if settings.DefaultHostname != testHostname {
		t.Fatalf("defaultHostname is %q, want %q; the facade refuses every operation while these differ",
			settings.DefaultHostname, testHostname)
	}
	if len(settings.MailExchangers) != 1 || settings.MailExchangers[0].Hostname != testHostname {
		t.Errorf("mail exchangers came back %+v", settings.MailExchangers)
	}

	// Bind: create the domain, then read it back with its zone file.
	const description = "simple zone z-probe"
	domain, err := client.CreateDomain(ctx, stalwart.DomainInput{
		Name:             "probe.test",
		Description:      description,
		ReportAddressURI: "mailto:postmaster@probe.test",
	})
	if err != nil {
		t.Fatalf("create the domain: %v", err)
	}
	if domain.Description != description {
		t.Errorf("description came back %q, want %q; the facade adopts a domain only on an exact match", domain.Description, description)
	}
	if !domain.Enabled {
		t.Error("the domain came back disabled")
	}
	if !strings.Contains(domain.DNSZoneFile, "_domainkey.probe.test.") {
		t.Errorf("the create did not answer with a rendered zone file: %q", domain.DNSZoneFile)
	}

	found, ok, err := client.FindDomain(ctx, "PROBE.test")
	if err != nil || !ok {
		t.Fatalf("find the domain: %v (found %t)", err, ok)
	}
	if found.ID != domain.ID {
		t.Errorf("found %s, want %s", found.ID, domain.ID)
	}
	if missing, ok, err := client.FindDomain(ctx, "nope.test"); err != nil || ok {
		t.Errorf("a missing domain answered %+v %t %v", missing, ok, err)
	}

	// The readiness wait: one active signature per algorithm.
	signatures, err := client.ListDkim(ctx, domain.ID)
	if err != nil {
		t.Fatalf("list the signatures: %v", err)
	}
	algorithms := map[string]bool{}
	for _, signature := range signatures {
		if signature.Active() {
			algorithms[signature.Algorithm] = true
		}
	}
	for _, algorithm := range stalwart.RequestedDkimAlgorithms {
		if !algorithms[algorithm] {
			t.Errorf("no active %s signature; the binding would stay degraded", algorithm)
		}
	}

	// A mailbox, its read-back, and the live list.
	account, err := client.CreateAccount(ctx, stalwart.AccountInput{
		LocalPart:   "mara",
		DomainID:    domain.ID,
		Description: "Mara",
		Secret:      testSecret,
		QuotaBytes:  5 << 30,
	})
	if err != nil {
		t.Fatalf("create the mailbox: %v", err)
	}
	if account.EmailAddress != "mara@probe.test" || account.LocalPart != "mara" {
		t.Errorf("the mailbox came back %+v", account)
	}
	if account.QuotaBytes != 5<<30 {
		t.Errorf("the quota came back %d", account.QuotaBytes)
	}

	if _, err := client.CreateAccount(ctx, stalwart.AccountInput{
		LocalPart: "mara", DomainID: domain.ID, Secret: testSecret, QuotaBytes: 1 << 30,
	}); !stalwart.IsDuplicate(err) {
		t.Errorf("a duplicate mailbox answered %v, want a duplicate", err)
	}

	// A forwarder onto that mailbox is an alias: read-modify-write of the set.
	aliases := append(slices.Clone(account.Aliases), stalwart.Alias{Name: "hello", DomainID: domain.ID, Enabled: true})
	if err := client.UpdateAccount(ctx, account.ID, stalwart.AccountPatch{Aliases: &aliases}); err != nil {
		t.Fatalf("add the alias: %v", err)
	}
	accounts, err := client.ListAccounts(ctx, domain.ID)
	if err != nil {
		t.Fatalf("list the mailboxes: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("listed %d mailboxes, want 1", len(accounts))
	}
	if len(accounts[0].Aliases) != 1 || accounts[0].Aliases[0].Name != "hello" || !accounts[0].Aliases[0].Enabled {
		t.Errorf("the alias came back %+v", accounts[0].Aliases)
	}

	// The password reset, and clearing the display name.
	newSecret := testSecret + "-rotated"
	empty := ""
	if err := client.UpdateAccount(ctx, account.ID, stalwart.AccountPatch{Secret: &newSecret, Description: &empty}); err != nil {
		t.Fatalf("reset the password: %v", err)
	}
	stored, err := store.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("read the stored mailbox: %v", err)
	}
	if !stored.Password.Verify(newSecret) || stored.Password.Verify(testSecret) {
		t.Error("the credential was not replaced")
	}
	if stored.Description != "" {
		t.Errorf("a null description must clear the display name, got %q", stored.Description)
	}

	// A forwarder with an external target is a mailing list.
	list, err := client.CreateList(ctx, stalwart.ListInput{
		LocalPart:   "team",
		DomainID:    domain.ID,
		Description: stalwart.ListDescription,
		Recipients:  []string{"someone@example.com", "mara@probe.test"},
	})
	if err != nil {
		t.Fatalf("create the mailing list: %v", err)
	}
	lists, err := client.ListLists(ctx, domain.ID)
	if err != nil {
		t.Fatalf("list the mailing lists: %v", err)
	}
	if len(lists) != 1 {
		t.Fatalf("listed %d mailing lists, want 1", len(lists))
	}
	if lists[0].EmailAddress != "team@probe.test" || lists[0].Description != stalwart.ListDescription {
		t.Errorf("the mailing list came back %+v", lists[0])
	}
	if !slices.Equal(lists[0].Recipients, []string{"mara@probe.test", "someone@example.com"}) {
		t.Errorf("the recipients came back %v", lists[0].Recipients)
	}
	// The unfiltered form is what RebuildPlatformStore uses.
	if all, err := client.ListLists(ctx, ""); err != nil || len(all) != 1 {
		t.Errorf("the unfiltered list answered %v %v", all, err)
	}

	if all, err := client.ListDomains(ctx); err != nil || len(all) != 1 || all[0].ID != domain.ID {
		t.Errorf("ListDomains answered %v %v", all, err)
	}

	// Unbind. The domain is refused while its children exist, which is the
	// retry destroyDomain performs.
	if err := client.DestroyDomain(ctx, domain.ID); !stalwart.IsLinked(err) {
		t.Errorf("destroying a linked domain answered %v, want objectIsLinked", err)
	}
	if err := client.DestroyList(ctx, list.ID); err != nil {
		t.Fatalf("destroy the mailing list: %v", err)
	}
	if err := client.DestroyAccount(ctx, account.ID); err != nil {
		t.Fatalf("destroy the mailbox: %v", err)
	}
	for _, signature := range signatures {
		if err := client.DestroyDkim(ctx, signature.ID); err != nil {
			t.Fatalf("destroy a signature: %v", err)
		}
	}
	if err := client.DestroyDomain(ctx, domain.ID); err != nil {
		t.Fatalf("destroy the domain: %v", err)
	}
	// Every destroy of something already gone is success.
	if err := client.DestroyDomain(ctx, domain.ID); err != nil {
		t.Errorf("destroying a missing domain answered %v", err)
	}
	if err := client.DestroyAccount(ctx, account.ID); err != nil {
		t.Errorf("destroying a missing mailbox answered %v", err)
	}
}

// TestTheFrozenClientSeesTheQueue covers the one call whose attribution rule
// lives in the client: a forwarder fan-out is attributed by orcpt.
func TestTheFrozenClientSeesTheQueue(t *testing.T) {
	handler, _ := newHandler(t, func(options *Options) {
		options.ListQueue = func(context.Context) ([]QueuedMessage, error) {
			return []QueuedMessage{
				{
					ID: "j1", ReturnPath: "sender@example.org", CreatedAt: testClock,
					Recipients: []QueuedRecipient{{
						Address: "someone@example.com", ORCPT: "team@probe.test",
						Status: QueueStatusTemporaryFailure, QueueName: "remote", RetryCount: 2,
					}},
				},
				{
					ID: "j2", ReturnPath: "noise@elsewhere.invalid", CreatedAt: testClock,
					Recipients: []QueuedRecipient{{Address: "nobody@elsewhere.invalid", Status: QueueStatusScheduled}},
				},
			}, nil
		}
	})
	client := newFacadeClient(t, handler, testToken)

	messages, complete, err := client.QueryQueue(context.Background(), "probe.test", 10)
	if err != nil {
		t.Fatalf("query the queue: %v", err)
	}
	if !complete {
		t.Error("a short page must report the count as complete")
	}
	if len(messages) != 1 || messages[0].ID != "j1" {
		t.Fatalf("got %+v, want only the message attributable to the zone", messages)
	}
	if messages[0].Recipients[0].ORCPT != "team@probe.test" {
		t.Errorf("orcpt came back %q; without it the message belongs to nobody", messages[0].Recipients[0].ORCPT)
	}
}

// TestTheFrozenClientReportsARejectedKey. Only 401 maps to ErrUnauthorized;
// anything else would be reported as "the mail engine did not answer".
func TestTheFrozenClientReportsARejectedKey(t *testing.T) {
	handler, _ := newHandler(t)
	client := newFacadeClient(t, handler, "the-wrong-token")

	if _, err := client.Account(context.Background()); !errors.Is(err, stalwart.ErrUnauthorized) {
		t.Errorf("GET /api/account answered %v, want ErrUnauthorized", err)
	}
	if _, err := client.SystemSettings(context.Background()); !errors.Is(err, stalwart.ErrUnauthorized) {
		t.Errorf("POST /jmap answered %v, want ErrUnauthorized", err)
	}
}

// TestTheProductionHooksProduceAPublishableZoneFile wires the handler the way
// cmd/deephost-mail does — the real key generator and the real zone renderer —
// and drives it with the facade's own client.
//
// This is the output the whole server exists to produce: the DKIM records the
// facade publishes are parsed out of dnsZoneFile, not read from
// DkimSignature.publicKey, and a record the facade's parser rejects is silently
// not published, which leaves the binding permanently incomplete. The
// structural rules asserted here are validDkimValue's: v first and DKIM1, k
// present and rsa or ed25519, p non-empty base64, and the owner's first label
// equal to an active signature's selector, which is the join key.
func TestTheProductionHooksProduceAPublishableZoneFile(t *testing.T) {
	handler, store := newHandler(t, func(options *Options) {
		options.RenderZoneFile = func(ctx context.Context, domain Domain) (string, error) {
			keys, err := options.Store.ListDkimKeys(ctx, domain.ID)
			if err != nil {
				return "", err
			}
			return DkimZoneRecords(keys, domain.Name), nil
		}
		options.ProvisionDkim = func(ctx context.Context, domain Domain, algorithms []string, template string) error {
			_, err := ProvisionDkimKeys(ctx, options.Store, domain.ID, DkimOptions{
				SelectorTemplate: template,
				Algorithms:       algorithms,
				Now:              testClock,
			})
			return err
		}
	})
	client := newFacadeClient(t, handler, testToken)
	ctx := context.Background()

	domain, err := client.CreateDomain(ctx, stalwart.DomainInput{
		Name:        "probe.test",
		Description: "simple zone z-probe",
	})
	if err != nil {
		t.Fatalf("create the domain: %v", err)
	}

	signatures, err := client.ListDkim(ctx, domain.ID)
	if err != nil {
		t.Fatalf("list the signatures: %v", err)
	}
	if len(signatures) != len(stalwart.RequestedDkimAlgorithms) {
		t.Fatalf("got %d signatures, want one per algorithm", len(signatures))
	}

	// One published record per active selector, joined on the first label.
	for _, signature := range signatures {
		if !signature.Active() {
			t.Fatalf("%s is staged %q; only active signatures are published", signature.Selector, signature.Stage)
		}
		owner := signature.Selector + "._domainkey.probe.test."
		if !strings.Contains(domain.DNSZoneFile, owner) {
			t.Fatalf("the zone file has no record for %s:\n%s", owner, domain.DNSZoneFile)
		}
	}

	// And the value passes the rules a rejected record would fail. A long RSA
	// value is split into character-strings of at most 255 bytes inside
	// parentheses, and the facade concatenates them, so the record is read the
	// same way here.
	for _, record := range zoneRecords(domain.DNSZoneFile) {
		if !strings.Contains(record, "_domainkey") {
			continue
		}
		value := joinCharacterStrings(record)
		if !strings.HasPrefix(value, "v=DKIM1;") {
			t.Errorf("v must be the first tag and be DKIM1: %q", value)
		}
		if !strings.Contains(value, "k=rsa") && !strings.Contains(value, "k=ed25519") {
			t.Errorf("k must be present and be rsa or ed25519: %q", value)
		}
		_, encoded, found := strings.Cut(value, "p=")
		if !found || encoded == "" {
			t.Errorf("p must be present and non-empty: %q", value)
			continue
		}
		if _, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded)); err != nil {
			t.Errorf("p must decode as base64: %q", encoded)
		}
	}

	// The store holds the private halves and no wire object does.
	keys, err := store.ListDkimKeys(ctx, domain.ID)
	if err != nil {
		t.Fatalf("read the stored keys: %v", err)
	}
	for _, key := range keys {
		if len(key.PrivateKey) == 0 {
			t.Errorf("%s was stored without its private half", key.Selector)
		}
		if strings.Contains(string(marshal(t, key.Wire())), base64.StdEncoding.EncodeToString(key.PrivateKey.Bytes())) {
			t.Errorf("%s leaked its private key onto the wire", key.Selector)
		}
	}
}

// zoneRecords splits BIND text into logical records, folding a parenthesised
// continuation into the line that opened it — which is how a long RSA DKIM
// value is written.
func zoneRecords(zone string) []string {
	var records []string
	var current strings.Builder
	depth := 0
	for _, line := range strings.Split(zone, "\n") {
		if strings.TrimSpace(line) == "" && depth == 0 {
			continue
		}
		if current.Len() > 0 {
			current.WriteString(" ")
		}
		current.WriteString(strings.TrimSpace(line))
		depth += strings.Count(line, "(") - strings.Count(line, ")")
		if depth <= 0 {
			records = append(records, current.String())
			current.Reset()
			depth = 0
		}
	}
	if current.Len() > 0 {
		records = append(records, current.String())
	}
	return records
}

// joinCharacterStrings concatenates the quoted character-strings of a TXT
// record, which is what the facade compares by wire text.
func joinCharacterStrings(record string) string {
	var value strings.Builder
	inside := false
	for index := 0; index < len(record); index++ {
		if record[index] == '"' {
			inside = !inside
			continue
		}
		if inside {
			value.WriteByte(record[index])
		}
	}
	return value.String()
}

// -----------------------------------------------------------------------------
// The order of the bearer check, and the addresses that get past it
// -----------------------------------------------------------------------------

// TestAnUnauthenticatedCallerCannotTellWhichPathsExist. The bearer is checked
// before the path is, so every refusal an anonymous caller can reach is the
// same 401. Checking the method or the route first makes the surface
// enumerable without a credential: POST /api/account answers 405 and POST /nope
// answers 404, which together map exactly which paths this process serves and
// which verbs each of them takes.
func TestAnUnauthenticatedCallerCannotTellWhichPathsExist(t *testing.T) {
	handler, _ := newHandler(t)

	probes := []struct {
		name   string
		method string
		path   string
		// authorized is what the same request answers once it carries the
		// token. It is asserted too: if these were all the same code the test
		// would pass with the checks in either order and prove nothing.
		authorized int
	}{
		{name: "the JMAP path with the wrong verb", method: http.MethodGet, path: JMAPPath, authorized: http.StatusMethodNotAllowed},
		{name: "the JMAP path with a stranger verb", method: http.MethodDelete, path: JMAPPath, authorized: http.StatusMethodNotAllowed},
		{name: "the account path with the wrong verb", method: http.MethodPost, path: AccountPath, authorized: http.StatusMethodNotAllowed},
		{name: "a path that does not exist", method: http.MethodPost, path: "/nope", authorized: http.StatusNotFound},
		{name: "a path that looks close", method: http.MethodPost, path: JMAPPath + "/x", authorized: http.StatusNotFound},
		{name: "the root", method: http.MethodGet, path: "/", authorized: http.StatusNotFound},
	}

	// Every anonymous answer must be byte-identical, headers included: a
	// difference in any of them is the distinction this test exists to remove.
	var reference *httptest.ResponseRecorder
	var referenceName string
	for _, probe := range probes {
		t.Run(probe.name, func(t *testing.T) {
			anonymous := httptest.NewRecorder()
			handler.ServeHTTP(anonymous, httptest.NewRequest(probe.method, probe.path, nil))
			if anonymous.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s answered %d without a token, want 401", probe.method, probe.path, anonymous.Code)
			}
			// An Allow header is itself the leak: it names the verbs a path
			// takes, which only a path that exists has.
			if allow := anonymous.Header().Get("Allow"); allow != "" {
				t.Errorf("the 401 carried Allow: %q, which says the path exists", allow)
			}
			if reference == nil {
				reference, referenceName = anonymous, probe.name
			} else {
				if anonymous.Body.String() != reference.Body.String() {
					t.Errorf("the 401 body differs from %q: %q vs %q", referenceName, anonymous.Body.String(), reference.Body.String())
				}
				if fmt.Sprint(anonymous.Header()) != fmt.Sprint(reference.Header()) {
					t.Errorf("the 401 headers differ from %q: %v vs %v", referenceName, anonymous.Header(), reference.Header())
				}
			}

			// With the token the distinctions are back, which is what makes
			// the anonymous sameness a decision rather than an accident.
			authorized := httptest.NewRecorder()
			request := httptest.NewRequest(probe.method, probe.path, nil)
			request.Header.Set("Authorization", "Bearer "+testToken)
			handler.ServeHTTP(authorized, request)
			if authorized.Code != probe.authorized {
				t.Errorf("%s %s answered %d with the token, want %d", probe.method, probe.path, authorized.Code, probe.authorized)
			}
		})
	}
}

// hostileLocalParts are the values every address field must refuse. The CR and
// LF cases are the ones that matter: an address is copied into the envelope and
// the headers of delivered mail, so a control character that survives this
// boundary is a header the caller wrote. The rest are the same rule seen from
// other angles — a NUL, a tab, and a Cyrillic "а" that would otherwise let one
// mailbox impersonate another.
var hostileLocalParts = []struct {
	name  string
	value string
}{
	{name: "an embedded CRLF", value: "ada\r\nBcc: everyone@example.com"},
	{name: "a bare LF", value: "ada\nBcc: everyone@example.com"},
	{name: "a bare CR", value: "ada\rBcc: everyone@example.com"},
	// TrimSpace would silently strip this one, which is why nothing on this
	// path uses TrimSpace.
	{name: "a trailing CR", value: "ada\r"},
	{name: "a leading LF", value: "\nada"},
	{name: "a NUL", value: "ada\x00"},
	{name: "a tab", value: "ada\tvalue"},
	{name: "an interior space", value: "ada bell"},
	{name: "a non-ASCII confusable", value: "аda"},
	{name: "angle brackets", value: "<ada>"},
	{name: "a second address", value: "ada@acme.dev, evil@example.com"},
}

// TestListRecipientsCannotCarryACRLF is the header-injection case named in the
// brief. A mailing list recipient is an address this server writes into the
// envelope of every forwarded copy; the value used to be lowercased and
// TrimSpace'd and then stored, so a CRLF went in at the API and came out in
// delivered mail.
func TestListRecipientsCannotCarryACRLF(t *testing.T) {
	handler, store := newHandler(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "acme.dev")

	for _, hostile := range hostileLocalParts {
		t.Run(hostile.name, func(t *testing.T) {
			recipient := hostile.value + "@example.com"

			// Refused on create.
			responses := callJMAP(t, handler, call(MethodListSet, map[string]any{
				"create": map[string]any{"l1": map[string]any{
					"name":        "team",
					"domainId":    domain.ID,
					"description": "forwarder",
					"recipients":  map[string]bool{recipient: true, "mara@acme.dev": true},
				}},
			}, "0"))
			failure := setErrorOf(t, responses[0], "notCreated", "l1")
			if failure["type"] != ErrTypeInvalidProperties {
				t.Errorf("create answered type %v, want %s", failure["type"], ErrTypeInvalidProperties)
			}
			// The refusal must not echo the address back: the description is
			// exactly where the injected bytes would like to travel.
			if body := string(marshal(t, responses[0].Args)); strings.ContainsAny(body, "\r\n") {
				t.Errorf("the refusal echoed a control character: %q", body)
			}
			// Nothing reached the store.
			lists, err := store.ListLists(ctx, "")
			if err != nil {
				t.Fatalf("read the lists back: %v", err)
			}
			if len(lists) != 0 {
				t.Fatalf("a refused create still stored %d lists: %v", len(lists), lists)
			}

			// And refused on patch, which is the path the console drives when
			// a forwarder's targets are edited.
			good := NewList(domain.ID, "team", testClock)
			good.Recipients = []string{"mara@acme.dev"}
			stored, err := store.CreateList(ctx, good)
			if err != nil {
				t.Fatalf("seed the list: %v", err)
			}
			responses = callJMAP(t, handler, call(MethodListSet, map[string]any{
				"update": map[string]any{stored.ID: map[string]any{
					"recipients": map[string]bool{recipient: true},
				}},
			}, "0"))
			if _, isSetError := responses[0].Args["notUpdated"]; !isSetError {
				t.Errorf("the patch was accepted: %v", responses[0].Args)
			}
			after, err := store.GetList(ctx, stored.ID)
			if err != nil {
				t.Fatalf("read the list back: %v", err)
			}
			if !slices.Equal(after.Recipients, []string{"mara@acme.dev"}) {
				t.Fatalf("a refused patch changed the recipients to %v", after.Recipients)
			}
			if err := store.DeleteList(ctx, stored.ID); err != nil {
				t.Fatalf("clean up the list: %v", err)
			}
		})
	}
}

// TestEveryAddressEnteringThroughJMAPIsValidated covers the rest of the
// boundary: a mailbox local part, an alias, a mailing list's own name, a domain
// name, a catch-all and the report address. Each one becomes an address this
// server puts in an envelope, so each one is parsed rather than tidied.
func TestEveryAddressEnteringThroughJMAPIsValidated(t *testing.T) {
	handler, store := newHandler(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "acme.dev")
	account := NewAccount(domain.ID, "mara", testClock)
	stored, err := store.CreateAccount(ctx, account)
	if err != nil {
		t.Fatalf("seed the mailbox: %v", err)
	}

	fields := []struct {
		name       string
		method     string
		args       func(hostile string) map[string]any
		bucket     string
		key        string
		wantBlames string
	}{
		{
			name:   "a mailbox local part",
			method: MethodAccountSet,
			args: func(hostile string) map[string]any {
				return map[string]any{"create": map[string]any{"u1": accountCreateBody(domain.ID, hostile, "", testSecret)}}
			},
			bucket: "notCreated", key: "u1", wantBlames: "name",
		},
		{
			name:   "an alias on a mailbox create",
			method: MethodAccountSet,
			args: func(hostile string) map[string]any {
				body := accountCreateBody(domain.ID, "ada", "", testSecret)
				body["aliases"] = map[string]any{"0": map[string]any{"name": hostile, "domainId": domain.ID, "enabled": true}}
				return map[string]any{"create": map[string]any{"u1": body}}
			},
			bucket: "notCreated", key: "u1", wantBlames: "aliases",
		},
		{
			name:   "an alias on a mailbox patch",
			method: MethodAccountSet,
			args: func(hostile string) map[string]any {
				return map[string]any{"update": map[string]any{stored.ID: map[string]any{
					"aliases": map[string]any{"0": map[string]any{"name": hostile, "domainId": domain.ID, "enabled": true}},
				}}}
			},
			bucket: "notUpdated", key: stored.ID, wantBlames: "aliases",
		},
		{
			name:   "a mailing list name",
			method: MethodListSet,
			args: func(hostile string) map[string]any {
				return map[string]any{"create": map[string]any{"l1": map[string]any{
					"name":       hostile,
					"domainId":   domain.ID,
					"recipients": map[string]bool{"mara@acme.dev": true},
				}}}
			},
			bucket: "notCreated", key: "l1", wantBlames: "name",
		},
		{
			name:   "a domain name",
			method: MethodDomainSet,
			args: func(hostile string) map[string]any {
				return map[string]any{"create": map[string]any{"d1": domainCreateBody(hostile+".test", "simple zone z-1")}}
			},
			bucket: "notCreated", key: "d1", wantBlames: "name",
		},
		{
			name:   "a catch-all address",
			method: MethodDomainSet,
			args: func(hostile string) map[string]any {
				body := domainCreateBody("other.test", "simple zone z-2")
				body["catchAllAddress"] = hostile
				return map[string]any{"create": map[string]any{"d1": body}}
			},
			bucket: "notCreated", key: "d1", wantBlames: "catchAllAddress",
		},
		{
			name:   "a report address",
			method: MethodDomainSet,
			args: func(hostile string) map[string]any {
				body := domainCreateBody("other.test", "simple zone z-3")
				body["reportAddressUri"] = "mailto:" + hostile + "@other.test"
				return map[string]any{"create": map[string]any{"d1": body}}
			},
			bucket: "notCreated", key: "d1", wantBlames: "reportAddressUri",
		},
	}

	for _, field := range fields {
		for _, hostile := range hostileLocalParts {
			t.Run(field.name+"/"+hostile.name, func(t *testing.T) {
				responses := callJMAP(t, handler, call(field.method, field.args(hostile.value), "0"))
				if responses[0].Name != field.method {
					t.Fatalf("the call failed at the method level: %v", responses[0].Args)
				}
				failure := setErrorOf(t, responses[0], field.bucket, field.key)
				kind, isString := failure["type"].(string)
				if !isString || (kind != ErrTypeInvalidProperties && kind != ErrTypeInvalidPatch) {
					t.Errorf("type is %v, want a request-blaming type", failure["type"])
				}
				properties, ok := failure["properties"].([]any)
				if !ok || len(properties) == 0 || properties[0] != field.wantBlames {
					t.Errorf("blamed %v, want [%s]", failure["properties"], field.wantBlames)
				}
				if body := string(marshal(t, responses[0].Args)); strings.ContainsAny(body, "\r\n") {
					t.Errorf("the refusal echoed a control character: %q", body)
				}
			})
		}
	}

	// Nothing hostile was created along the way.
	accounts, err := store.ListAccounts(ctx, domain.ID)
	if err != nil {
		t.Fatalf("read the mailboxes back: %v", err)
	}
	if len(accounts) != 1 || accounts[0].LocalPart != "mara" {
		t.Fatalf("the refused creates left %v", accounts)
	}
	domains, err := store.ListDomains(ctx)
	if err != nil {
		t.Fatalf("read the domains back: %v", err)
	}
	if len(domains) != 1 || domains[0].Name != "acme.dev" {
		t.Fatalf("the refused creates left %v", domains)
	}
	after, err := store.GetAccount(ctx, stored.ID)
	if err != nil {
		t.Fatalf("read the mailbox back: %v", err)
	}
	if len(after.Aliases) != 0 {
		t.Fatalf("a refused alias patch stored %v", after.Aliases)
	}
}

// TestAllowRelayingIsRefusedRatherThanStoredAndIgnored. allowRelaying was
// decoded and stored and then read by nothing: relaying is decided by whether
// the session authenticated, not per domain. A security-relevant setting that
// does nothing is worse than no setting, because the next reader believes it
// works — so a true is refused and the stored flag can never become one.
//
// The false the frozen client always sends is unaffected, which is what keeps
// the wire contract intact.
func TestAllowRelayingIsRefusedRatherThanStoredAndIgnored(t *testing.T) {
	handler, store := newHandler(t)
	ctx := context.Background()

	body := domainCreateBody("relay.test", "simple zone z-relay")
	body["allowRelaying"] = true
	responses := callJMAP(t, handler, call(MethodDomainSet, map[string]any{"create": map[string]any{"d1": body}}, "0"))
	failure := setErrorOf(t, responses[0], "notCreated", "d1")
	if failure["type"] != ErrTypeInvalidProperties {
		t.Errorf("type is %v, want %s", failure["type"], ErrTypeInvalidProperties)
	}
	properties, ok := failure["properties"].([]any)
	if !ok || len(properties) == 0 || properties[0] != "allowRelaying" {
		t.Errorf("blamed %v, want [allowRelaying]", failure["properties"])
	}
	domains, err := store.ListDomains(ctx)
	if err != nil {
		t.Fatalf("read the domains back: %v", err)
	}
	if len(domains) != 0 {
		t.Fatalf("a refused create still stored %v", domains)
	}

	// The false the client sends is accepted, and no domain is ever stored
	// with the flag set.
	responses = callJMAP(t, handler, call(MethodDomainSet, map[string]any{
		"create": map[string]any{"d1": domainCreateBody("relay.test", "simple zone z-relay")},
	}, "0"))
	id := createdIDOf(t, responses[0], "d1")
	created, err := store.GetDomain(ctx, id)
	if err != nil {
		t.Fatalf("read the domain back: %v", err)
	}
	if created.AllowRelaying {
		t.Error("a domain was stored with relaying allowed")
	}
}

// TestValidAddressesStillGetThrough. The gate above must not be so tight that
// it refuses what the product actually creates: the frozen client's own
// payloads, plus the forms a customer legitimately types.
func TestValidAddressesStillGetThrough(t *testing.T) {
	handler, store := newHandler(t)
	ctx := context.Background()

	created := callJMAP(t, handler, call(MethodDomainSet, map[string]any{
		"create": map[string]any{"d1": domainCreateBody("acme-corp.dev", "simple zone z-ok")},
	}, "0"))
	domainID := createdIDOf(t, created[0], "d1")

	// Mixed case and a dot-atom local part; both survive as the canonical
	// lowercase form the store keys on.
	body := accountCreateBody(domainID, "Ada.Lovelace+notes", "Ada", testSecret)
	body["aliases"] = map[string]any{"0": map[string]any{"name": "Hello-There", "domainId": domainID, "enabled": true}}
	created = callJMAP(t, handler, call(MethodAccountSet, map[string]any{"create": map[string]any{"u1": body}}, "0"))
	accountID := createdIDOf(t, created[0], "u1")
	account, err := store.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatalf("read the mailbox back: %v", err)
	}
	if account.LocalPart != "ada.lovelace+notes" {
		t.Errorf("the local part stored as %q", account.LocalPart)
	}
	if len(account.Aliases) != 1 || account.Aliases[0].LocalPart != "hello-there" {
		t.Errorf("the alias stored as %v", account.Aliases)
	}

	created = callJMAP(t, handler, call(MethodListSet, map[string]any{
		"create": map[string]any{"l1": map[string]any{
			"name":        "Team",
			"domainId":    domainID,
			"description": stalwart.ListDescription,
			"recipients":  map[string]bool{"Someone@Example.COM": true, "ada.lovelace+notes@acme-corp.dev": true},
		}},
	}, "0"))
	listID := createdIDOf(t, created[0], "l1")
	list, err := store.GetList(ctx, listID)
	if err != nil {
		t.Fatalf("read the list back: %v", err)
	}
	if !slices.Equal(list.Recipients, []string{"ada.lovelace+notes@acme-corp.dev", "someone@example.com"}) {
		t.Errorf("the recipients stored as %v", list.Recipients)
	}
}

// TestTheBearerIsComparedInConstantTime. A timing test would be flaky and would
// prove nothing on a loaded machine, so this pins the mechanism two ways.
//
// Behaviourally: presented tokens of every length — empty, one byte, a prefix,
// the right length with the last byte wrong, and one far longer — all produce
// the same answer down to the byte, so nothing about the real token's length or
// contents is readable from the response.
//
// Structurally: the comparison itself is asserted to go through crypto/subtle.
// The obvious regression here is someone "simplifying" it to ==, which no
// black-box test can see, and which hands an attacker a byte-at-a-time oracle
// on the credential that creates and destroys mail domains.
func TestTheBearerIsComparedInConstantTime(t *testing.T) {
	handler, _ := newHandler(t)

	presented := []string{
		"",
		"x",
		testToken[:len(testToken)-1],
		testToken[:len(testToken)-1] + "!",
		testToken + strings.Repeat("x", 4096),
	}
	var reference *httptest.ResponseRecorder
	for _, token := range presented {
		request := httptest.NewRequest(http.MethodPost, JMAPPath, strings.NewReader(`{"using":[],"methodCalls":[]}`))
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("a %d-byte token answered %d, want 401", len(token), recorder.Code)
		}
		if reference == nil {
			reference = recorder
			continue
		}
		if recorder.Body.String() != reference.Body.String() || fmt.Sprint(recorder.Header()) != fmt.Sprint(reference.Header()) {
			t.Errorf("a %d-byte token got a different answer: %v %q", len(token), recorder.Header(), recorder.Body.String())
		}
	}

	source, err := os.ReadFile("jmap.go")
	if err != nil {
		t.Fatalf("read the source to check the comparison: %v", err)
	}
	const signature = "func (h *Handler) authorized("
	start := strings.Index(string(source), signature)
	if start < 0 {
		t.Fatal("authorized is gone; move this assertion to whatever compares the bearer now")
	}
	body := string(source)[start:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "subtle.ConstantTimeCompare") {
		t.Errorf("the bearer comparison no longer goes through crypto/subtle:\n%s", body)
	}
}
