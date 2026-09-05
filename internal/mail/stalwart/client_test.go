package stalwart

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testToken is the shape of a real API key with every secret byte replaced. It
// is the only credential-looking string in this package's fixtures.
const testToken = "API_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

// recorder is a fake Stalwart: it replays the committed transcripts and records
// the request bodies so a test can assert the exact wire shape the client sent.
type recorder struct {
	t         *testing.T
	responses []string
	index     int
	requests  []map[string]any
	account   string
	status    int
}

func (r *recorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer "+testToken {
			r.t.Errorf("Authorization = %q, want a bearer credential", got)
		}
		if request.URL.Path == "/api/account" {
			w.Header().Set("Content-Type", "application/json")
			write(r.t, w, r.account)
			return
		}
		if request.URL.Path != "/jmap" {
			r.t.Errorf("path = %q, want /jmap", request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			r.t.Errorf("decode request: %v", err)
		}
		r.requests = append(r.requests, body)
		if r.status != 0 {
			w.WriteHeader(r.status)
			return
		}
		if r.index >= len(r.responses) {
			r.t.Fatalf("unexpected request %d; only %d transcripts were queued", r.index+1, len(r.responses))
		}
		payload := r.responses[r.index]
		r.index++
		w.Header().Set("Content-Type", "application/json")
		write(r.t, w, payload)
	}
}

// call returns the [name, args, id] triple of one recorded method call.
func (r *recorder) call(t *testing.T, request, index int) (string, map[string]any) {
	t.Helper()
	if request >= len(r.requests) {
		t.Fatalf("request %d was never sent (%d were)", request+1, len(r.requests))
	}
	calls, ok := r.requests[request]["methodCalls"].([]any)
	if !ok || index >= len(calls) {
		t.Fatalf("request %d has no method call %d", request, index)
	}
	triple, ok := calls[index].([]any)
	if !ok || len(triple) != 3 {
		t.Fatalf("method call %d is not a triple: %#v", index, calls[index])
	}
	name, ok := triple[0].(string)
	if !ok {
		t.Fatalf("method call %d has no name: %#v", index, triple[0])
	}
	args, ok := triple[1].(map[string]any)
	if !ok {
		t.Fatalf("method call %d has no arguments: %#v", index, triple[1])
	}
	return name, args
}

// object reads a nested JSON object out of a recorded request. A missing or
// wrongly typed key returns nil, so the assertion that follows reports what is
// actually wrong instead of the type assertion doing it.
func jsonObject(value any) map[string]any {
	nested, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return nested
}

// list reads a nested JSON array out of a recorded request.
func jsonList(value any) []any {
	nested, ok := value.([]any)
	if !ok {
		return nil
	}
	return nested
}

// write sends a recorded transcript, failing the test if it cannot.
func write(t *testing.T, w http.ResponseWriter, payload string) {
	t.Helper()
	if _, err := w.Write([]byte(payload)); err != nil {
		t.Errorf("write the response: %v", err)
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(raw)
}

func newClient(t *testing.T, responses ...string) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{t: t, responses: responses, account: fixture(t, "api_account.json")}
	server := httptest.NewServer(rec.handler())
	t.Cleanup(server.Close)
	client, err := New(server.URL, testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, rec
}

func TestAccountReportsEditionAndPermissions(t *testing.T) {
	client, _ := newClient(t)
	info, err := client.Account(t.Context())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if info.Edition != "community" {
		t.Errorf("edition = %q, want community", info.Edition)
	}
	if len(info.Permissions) == 0 {
		t.Fatal("permissions are empty")
	}
}

func TestSystemSettingsReadsTheSingletonById(t *testing.T) {
	client, rec := newClient(t, fixture(t, "system_settings_get.json"))
	settings, err := client.SystemSettings(t.Context())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if settings.DefaultHostname != "mail.local.test" {
		t.Errorf("defaultHostname = %q", settings.DefaultHostname)
	}
	// A bare /get on a singleton returns an empty list, so the id is required.
	name, args := rec.call(t, 0, 0)
	if name != "x:SystemSettings/get" {
		t.Errorf("method = %q", name)
	}
	ids := jsonList(args["ids"])
	if len(ids) != 1 || ids[0] != "singleton" {
		t.Errorf("ids = %#v, want [singleton]", args["ids"])
	}
	// mailExchangers is read for display only; a null hostname means the
	// server's own name.
	if len(settings.MailExchangers) != 1 || settings.MailExchangers[0].Hostname != "mail.local.test" {
		t.Errorf("mailExchangers = %#v", settings.MailExchangers)
	}
}

func TestFindDomainUsesAResultReference(t *testing.T) {
	client, rec := newClient(t, fixture(t, "domain_query_get.json"))
	domain, found, err := client.FindDomain(t.Context(), "probe.test")
	if err != nil || !found {
		t.Fatalf("FindDomain: %v found=%v", err, found)
	}
	if domain.ID != "c" || domain.Description != "simple zone z-probe" {
		t.Errorf("domain = %#v", domain)
	}
	if !strings.Contains(domain.DNSZoneFile, "_domainkey") {
		t.Error("the zone file was not returned")
	}
	// The query and the get travel in one request, joined by a back-reference.
	if len(rec.requests) != 1 {
		t.Fatalf("sent %d requests, want 1", len(rec.requests))
	}
	_, queryArgs := rec.call(t, 0, 0)
	filter := jsonObject(queryArgs["filter"])
	if filter["name"] != "probe.test" {
		t.Errorf("filter = %#v", queryArgs["filter"])
	}
	_, getArgs := rec.call(t, 0, 1)
	reference, ok := getArgs["#ids"].(map[string]any)
	if !ok || reference["resultOf"] != "0" || reference["name"] != "x:Domain/query" || reference["path"] != "/ids" {
		t.Errorf("#ids = %#v", getArgs["#ids"])
	}
}

func TestFindDomainReportsAbsenceWithoutAnError(t *testing.T) {
	empty := `{"methodResponses":[["x:Domain/query",{"ids":[]},"0"],["x:Domain/get",{"list":[],"notFound":[]},"1"]],"sessionState":"a"}`
	client, _ := newClient(t, empty)
	_, found, err := client.FindDomain(t.Context(), "absent.test")
	if err != nil {
		t.Fatalf("FindDomain: %v", err)
	}
	if found {
		t.Error("an absent domain was reported as found")
	}
}

func TestCreateDomainRequestsBothDkimAlgorithmsAndManualDns(t *testing.T) {
	client, rec := newClient(t, fixture(t, "domain_create.json"), fixture(t, "domain_get_zonefile.json"))
	domain, err := client.CreateDomain(t.Context(), DomainInput{
		Name:             "probe.test",
		Description:      "simple zone z-probe",
		ReportAddressURI: "mailto:postmaster@probe.test",
	})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	if domain.ID != "c" {
		t.Errorf("id = %q", domain.ID)
	}
	_, args := rec.call(t, 0, 0)
	create := jsonObject(args["create"])
	object := jsonObject(create["d1"])
	dkim := jsonObject(object["dkimManagement"])
	algorithms := jsonObject(dkim["algorithms"])
	for _, algorithm := range RequestedDkimAlgorithms {
		if algorithms[algorithm] != true {
			t.Errorf("algorithm %s was not requested: %#v", algorithm, algorithms)
		}
	}
	// This deployment publishes the records itself and issues no certificate
	// for the customer name.
	if dns := jsonObject(object["dnsManagement"]); dns["@type"] != "Manual" {
		t.Errorf("dnsManagement = %#v", object["dnsManagement"])
	}
	if tls := jsonObject(object["certificateManagement"]); tls["@type"] != "Manual" {
		t.Errorf("certificateManagement = %#v", object["certificateManagement"])
	}
	if object["allowRelaying"] != false {
		t.Errorf("allowRelaying = %#v", object["allowRelaying"])
	}
}

func TestListDkimReadsSelectorsAndStages(t *testing.T) {
	client, rec := newClient(t, fixture(t, "dkim_signature_get.json"))
	signatures, err := client.ListDkim(t.Context(), "c")
	if err != nil {
		t.Fatalf("ListDkim: %v", err)
	}
	if len(signatures) != 2 {
		t.Fatalf("got %d signatures, want 2", len(signatures))
	}
	// Sorted by selector, so ed25519 comes first.
	if signatures[0].Algorithm != DkimAlgorithmEd25519 || !signatures[0].Active() {
		t.Errorf("first = %#v", signatures[0])
	}
	if signatures[1].Algorithm != DkimAlgorithmRSA {
		t.Errorf("second = %#v", signatures[1])
	}
	if signatures[0].CreatedAt.IsZero() {
		t.Error("createdAt was not parsed")
	}
	// The domainId filter IS supported for this object, unlike mailing lists.
	_, args := rec.call(t, 0, 0)
	filter := jsonObject(args["filter"])
	if filter["domainId"] != "c" {
		t.Errorf("filter = %#v", args["filter"])
	}
}

func TestListAccountsReadsUsageAndAliases(t *testing.T) {
	client, rec := newClient(t, fixture(t, "account_query_get.json"))
	accounts, err := client.ListAccounts(t.Context(), "c")
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}
	account := accounts[0]
	if account.EmailAddress != "mara@probe.test" || account.LocalPart != "mara" {
		t.Errorf("account = %#v", account)
	}
	if account.UsedBytes != 1894 || account.QuotaBytes != 5368709120 {
		t.Errorf("usage = %d/%d", account.UsedBytes, account.QuotaBytes)
	}
	if len(account.Aliases) != 1 || account.Aliases[0].Name != "hello" {
		t.Errorf("aliases = %#v", account.Aliases)
	}
	_, args := rec.call(t, 0, 0)
	filter := jsonObject(args["filter"])
	if filter["@type"] != "User" || filter["domainId"] != "c" {
		t.Errorf("filter = %#v", args["filter"])
	}
}

func TestCreateAccountSendsTheCredentialExactlyOnce(t *testing.T) {
	client, rec := newClient(t, fixture(t, "account_create.json"), fixture(t, "account_get.json"))
	_, err := client.CreateAccount(t.Context(), AccountInput{
		LocalPart:   "mara",
		DomainID:    "c",
		Description: "Mara",
		Secret:      "corridor-lantern-basalt-tumble-quiver-47",
		QuotaBytes:  5368709120,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	_, args := rec.call(t, 0, 0)
	create := jsonObject(args["create"])
	object := jsonObject(create["u1"])
	credentials := jsonObject(object["credentials"])
	entry := jsonObject(credentials["0"])
	if entry["secret"] != "corridor-lantern-basalt-tumble-quiver-47" {
		t.Errorf("credential was not sent: %#v", credentials)
	}
	quotas := jsonObject(object["quotas"])
	if quotas["maxDiskQuota"] != float64(5368709120) {
		t.Errorf("quotas = %#v", object["quotas"])
	}
	// The second request re-reads the account; it must not carry a credential.
	if len(rec.requests) != 2 {
		t.Fatalf("sent %d requests, want 2", len(rec.requests))
	}
	second, err := json.Marshal(rec.requests[1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(second), "secret") {
		t.Errorf("the follow-up read carried a credential: %s", second)
	}
}

func TestCreateAccountMapsPrimaryKeyViolation(t *testing.T) {
	client, _ := newClient(t, fixture(t, "account_create_primary_key_violation.json"))
	_, err := client.CreateAccount(t.Context(), AccountInput{LocalPart: "mara", DomainID: "c"})
	if !IsDuplicate(err) {
		t.Fatalf("IsDuplicate(%v) = false", err)
	}
	setErr, ok := SetErrorOf(err)
	if !ok || setErr.Property() != "email" {
		t.Errorf("set error = %#v", setErr)
	}
}

func TestSetErrorsCarryTheBlamedProperty(t *testing.T) {
	client, _ := newClient(t, fixture(t, "account_create_invalid_patch.json"))
	_, err := client.CreateAccount(t.Context(), AccountInput{LocalPart: "mara", DomainID: "c"})
	if !IsInvalid(err) {
		t.Fatalf("IsInvalid(%v) = false", err)
	}
	setErr, _ := SetErrorOf(err)
	if setErr.Property() != "quotas" {
		t.Errorf("property = %q", setErr.Property())
	}
}

func TestDestroyDomainReportsLinkedChildren(t *testing.T) {
	client, _ := newClient(t, fixture(t, "domain_destroy_object_is_linked.json"))
	err := client.DestroyDomain(t.Context(), "c")
	if !IsLinked(err) {
		t.Fatalf("IsLinked(%v) = false", err)
	}
	setErr, _ := SetErrorOf(err)
	if len(setErr.LinkedObjects) != 4 {
		t.Errorf("linked objects = %#v", setErr.LinkedObjects)
	}
}

func TestDestroyTreatsAMissingObjectAsSuccess(t *testing.T) {
	missing := `{"methodResponses":[["x:Account/set",{"notDestroyed":{"d":{"type":"notFound"}}},"0"]],"sessionState":"a"}`
	client, _ := newClient(t, missing)
	if err := client.DestroyAccount(t.Context(), "d"); err != nil {
		t.Fatalf("DestroyAccount: %v", err)
	}
}

func TestForbiddenIsAMethodError(t *testing.T) {
	client, _ := newClient(t, fixture(t, "error_forbidden.json"))
	_, err := client.SystemSettings(t.Context())
	if !IsForbidden(err) {
		t.Fatalf("IsForbidden(%v) = false", err)
	}
}

func TestListListsQueriesUnfilteredAndMatchesTheDomainLocally(t *testing.T) {
	client, rec := newClient(t, fixture(t, "mailinglist_query_unfiltered.json"))
	lists, err := client.ListLists(t.Context(), "c")
	if err != nil {
		t.Fatalf("ListLists: %v", err)
	}
	if len(lists) != 1 || lists[0].ID != "b" {
		t.Fatalf("lists = %#v", lists)
	}
	if len(lists[0].Recipients) != 2 || lists[0].Recipients[0] != "mara@probe.test" {
		t.Errorf("recipients = %#v", lists[0].Recipients)
	}
	// x:MailingList/query supports only text and memberTenantId filters, so a
	// domainId filter would answer unsupportedFilter. The query must carry no
	// filter at all.
	_, args := rec.call(t, 0, 0)
	if _, present := args["filter"]; present {
		t.Errorf("the query carried a filter: %#v", args)
	}
	if args["limit"] != float64(MaxObjectsPerCall) {
		t.Errorf("limit = %#v, want %d", args["limit"], MaxObjectsPerCall)
	}
}

func TestListListsWithoutADomainReturnsEveryList(t *testing.T) {
	client, _ := newClient(t, fixture(t, "mailinglist_query_unfiltered.json"))
	lists, err := client.ListLists(t.Context(), "")
	if err != nil {
		t.Fatalf("ListLists: %v", err)
	}
	if len(lists) != 2 {
		t.Fatalf("got %d lists, want every one", len(lists))
	}
}

func TestListListsPagesWithPosition(t *testing.T) {
	// A full first page must be followed by a second query at the next
	// position; a short page ends the walk.
	first := fullListPage(t)
	second := fixture(t, "mailinglist_query_unfiltered.json")
	client, rec := newClient(t, first, second)
	lists, err := client.ListLists(t.Context(), "")
	if err != nil {
		t.Fatalf("ListLists: %v", err)
	}
	if len(lists) != MaxObjectsPerCall+2 {
		t.Fatalf("got %d lists, want %d", len(lists), MaxObjectsPerCall+2)
	}
	if len(rec.requests) != 2 {
		t.Fatalf("sent %d requests, want 2", len(rec.requests))
	}
	_, args := rec.call(t, 1, 0)
	if args["position"] != float64(MaxObjectsPerCall) {
		t.Errorf("second page position = %#v, want %d", args["position"], MaxObjectsPerCall)
	}
}

// fullListPage builds a response whose query answered a full page.
func fullListPage(t *testing.T) string {
	t.Helper()
	ids := make([]string, 0, MaxObjectsPerCall)
	entries := make([]map[string]any, 0, MaxObjectsPerCall)
	for index := range MaxObjectsPerCall {
		id := "p" + strings.Repeat("0", 3-len(itoa(index))) + itoa(index)
		ids = append(ids, id)
		entries = append(entries, map[string]any{
			"name": id, "domainId": "z", "description": "forwarder",
			"recipients": map[string]any{"a@b.test": true}, "emailAddress": id + "@other.test", "id": id,
		})
	}
	payload := map[string]any{
		"methodResponses": []any{
			[]any{"x:MailingList/query", map[string]any{"ids": ids, "position": 0}, "0"},
			[]any{"x:MailingList/get", map[string]any{"list": entries, "notFound": []any{}}, "1"},
		},
		"sessionState": "a",
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func TestQueryQueueAttributesThroughOrcpt(t *testing.T) {
	client, rec := newClient(t, fixture(t, "queued_message_get_forwarder.json"))
	messages, complete, err := client.QueryQueue(t.Context(), "probe.test", 100)
	if err != nil {
		t.Fatalf("QueryQueue: %v", err)
	}
	if !complete {
		t.Error("a short page must report complete")
	}
	// The forwarder fan-out belongs to the zone only through orcpt; the
	// unrelated message must not be attributed to it.
	if len(messages) != 1 || messages[0].ID != "j1" {
		t.Fatalf("messages = %#v", messages)
	}
	recipient := messages[0].Recipients[0]
	if recipient.Address != "someone@example.com" || recipient.ORCPT != "team@probe.test" {
		t.Errorf("recipient = %#v", recipient)
	}
	if recipient.Status != QueueStatusTemporaryFailure || recipient.QueueName != "remote" || recipient.RetryCount != 2 {
		t.Errorf("recipient state = %#v", recipient)
	}
	if recipient.RetryDue.IsZero() {
		t.Error("retryDue was not parsed")
	}
	// The server's returnPath and to filters are substring matches, so the
	// query must be unfiltered.
	_, args := rec.call(t, 0, 0)
	if _, present := args["filter"]; present {
		t.Errorf("the queue query carried a filter: %#v", args)
	}
}

func TestQueryQueueReportsAFullPageAsIncomplete(t *testing.T) {
	ids := make([]string, MaxObjectsPerCall)
	for index := range ids {
		ids[index] = "q" + itoa(index)
	}
	payload, err := json.Marshal(map[string]any{
		"methodResponses": []any{
			[]any{"x:QueuedMessage/query", map[string]any{"ids": ids}, "0"},
			[]any{"x:QueuedMessage/get", map[string]any{"list": []any{}, "notFound": []any{}}, "1"},
		},
		"sessionState": "a",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	client, _ := newClient(t, string(payload))
	_, complete, err := client.QueryQueue(t.Context(), "probe.test", 100)
	if err != nil {
		t.Fatalf("QueryQueue: %v", err)
	}
	if complete {
		t.Error("a full page must report incomplete attribution")
	}
}

func TestBatchLimitIsEnforcedBeforeTheRequestIsSent(t *testing.T) {
	client, rec := newClient(t)
	calls := make([]MethodCall, MaxCallsPerRequest+1)
	for index := range calls {
		calls[index] = MethodCall{Name: "x:Domain/get", ID: itoa(index)}
	}
	if _, err := client.Call(t.Context(), calls); err == nil {
		t.Fatal("a batch over the server limit was accepted")
	}
	if len(rec.requests) != 0 {
		t.Errorf("the oversized batch was sent anyway (%d requests)", len(rec.requests))
	}
}

func TestUnauthorizedIsItsOwnSentinel(t *testing.T) {
	rec := &recorder{t: t, status: http.StatusUnauthorized, account: "{}"}
	server := httptest.NewServer(rec.handler())
	t.Cleanup(server.Close)
	client, err := New(server.URL, testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.SystemSettings(t.Context())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestNonOKStatusIsATransportFailure(t *testing.T) {
	rec := &recorder{t: t, status: http.StatusBadGateway, account: "{}"}
	server := httptest.NewServer(rec.handler())
	t.Cleanup(server.Close)
	client, err := New(server.URL, testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.SystemSettings(t.Context())
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("err = %v, want ErrTransport", err)
	}
}

func TestNewRejectsANonAbsoluteURL(t *testing.T) {
	for _, value := range []string{"", "/jmap", "ftp://mail.local.test"} {
		if _, err := New(value, testToken); err == nil {
			t.Errorf("New(%q) was accepted", value)
		}
	}
}

func TestTheCredentialNeverAppearsInAnError(t *testing.T) {
	// A dead address: the transport fails and the message must not name the
	// credential (the token travels as a header, never in the URL).
	client, err := New("http://127.0.0.1:1/", testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.SystemSettings(t.Context())
	if err == nil {
		t.Fatal("expected a transport failure")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("the error named the credential: %v", err)
	}
}

func TestBelongsToZoneMatchesExactDomains(t *testing.T) {
	message := QueuedMessage{
		ReturnPath: "sender@example.org",
		Recipients: []QueuedRecipient{{Address: "someone@notprobe.test"}},
	}
	if BelongsToZone(message, "probe.test") {
		t.Error("a suffix match was accepted; the domain must match exactly")
	}
	message.Recipients[0].ORCPT = "team@probe.test"
	if !BelongsToZone(message, "probe.test") {
		t.Error("an orcpt match was not attributed")
	}
}

func TestNormalizeORCPTStripsTheType(t *testing.T) {
	if got := normalizeORCPT("rfc822;team@probe.test"); got != "team@probe.test" {
		t.Errorf("normalizeORCPT = %q", got)
	}
	if got := normalizeORCPT("team@probe.test"); got != "team@probe.test" {
		t.Errorf("normalizeORCPT = %q", got)
	}
}

func TestFixturesCarryNoRealCredential(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join("testdata", entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		text := string(raw)
		for _, index := range indexesOf(text, "API_") {
			token := text[index:]
			if len(token) > len(testToken) {
				token = token[:len(testToken)]
			}
			if token != testToken {
				t.Errorf("%s carries an API key that is not the placeholder: %q", entry.Name(), token)
			}
		}
	}
}

func indexesOf(text, needle string) []int {
	var found []int
	for offset := 0; ; {
		index := strings.Index(text[offset:], needle)
		if index < 0 {
			return found
		}
		found = append(found, offset+index)
		offset += index + len(needle)
	}
}

// Stalwart refuses `"description": ""` ("Invalid value for property."), and a
// display name is optional in this product, so an empty one must be absent from
// the create object rather than sent as an empty string.
func TestCreateOmitsAnEmptyDescription(t *testing.T) {
	const listCreated = `{"methodResponses":[["x:MailingList/set",{"accountId":"c","created":{"l1":{"id":"e"}}},"0"]],"sessionState":"1"}`
	tests := []struct {
		name        string
		responses   []string
		description string
		create      func(t *testing.T, client *Client, description string)
		key         string
	}{
		{
			name:      "account",
			responses: []string{fixture(t, "account_create.json"), fixture(t, "account_get.json")},
			key:       "u1",
			create: func(t *testing.T, client *Client, description string) {
				t.Helper()
				if _, err := client.CreateAccount(t.Context(), AccountInput{
					LocalPart: "iris", DomainID: "c", Description: description, QuotaBytes: 1,
				}); err != nil {
					t.Fatalf("CreateAccount: %v", err)
				}
			},
		},
		{
			name:      "domain",
			responses: []string{fixture(t, "domain_create.json"), fixture(t, "domain_get_zonefile.json")},
			key:       "d1",
			create: func(t *testing.T, client *Client, description string) {
				t.Helper()
				if _, err := client.CreateDomain(t.Context(), DomainInput{
					Name: "probe.test", Description: description,
				}); err != nil {
					t.Fatalf("CreateDomain: %v", err)
				}
			},
		},
		{
			name:      "list",
			responses: []string{listCreated},
			key:       "l1",
			create: func(t *testing.T, client *Client, description string) {
				t.Helper()
				if _, err := client.CreateList(t.Context(), ListInput{
					LocalPart: "team", DomainID: "c", Description: description,
					Recipients: []string{"someone@example.com"},
				}); err != nil {
					t.Fatalf("CreateList: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name+"/empty", func(t *testing.T) {
			client, rec := newClient(t, test.responses...)
			test.create(t, client, "")
			_, args := rec.call(t, 0, 0)
			object := jsonObject(jsonObject(args["create"])[test.key])
			if _, ok := object["description"]; ok {
				t.Errorf("description was sent: %#v", object["description"])
			}
		})
		t.Run(test.name+"/present", func(t *testing.T) {
			client, rec := newClient(t, test.responses...)
			test.create(t, client, "Iris")
			_, args := rec.call(t, 0, 0)
			object := jsonObject(jsonObject(args["create"])[test.key])
			if object["description"] != "Iris" {
				t.Errorf("description = %#v, want %q", object["description"], "Iris")
			}
		})
	}
}

// x:Account/set replaces the aliases map rather than merging into it, and both
// callers of this patch are read-modify-writes over the whole list. So an alias
// is written back exactly as it was read: adding or removing one forwarder must
// not re-enable an address an operator disabled on the mail server, nor drop the
// description they gave it.
func TestUpdateAccountWritesEveryAliasBackAsItWasRead(t *testing.T) {
	const updated = `{"methodResponses":[["x:Account/set",{"accountId":"c","updated":{"d":null}},"0"]],"sessionState":"1"}`
	client, rec := newClient(t, updated)
	aliases := []Alias{
		{Name: "sales", DomainID: "c", Description: "legacy alias", Enabled: false},
		{Name: "info", DomainID: "c", Enabled: true},
	}
	if err := client.UpdateAccount(t.Context(), "d", AccountPatch{Aliases: &aliases}); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	_, args := rec.call(t, 0, 0)
	sent := jsonObject(jsonObject(jsonObject(args["update"])["d"])["aliases"])
	if len(sent) != len(aliases) {
		t.Fatalf("sent %d aliases, want %d: %#v", len(sent), len(aliases), sent)
	}
	byName := map[string]map[string]any{}
	for _, entry := range sent {
		object := jsonObject(entry)
		byName[fmt.Sprint(object["name"])] = object
	}
	if got := byName["sales"]; got["enabled"] != false || got["description"] != "legacy alias" {
		t.Errorf("the disabled, described alias was sent as %#v", got)
	}
	if got := byName["info"]; got["enabled"] != true || got["description"] != nil {
		t.Errorf("the new alias was sent as %#v", got)
	}
}

// Clearing a display name removes the property, which JMAP expresses as null.
// An empty string is not a valid value for it.
func TestUpdateAccountClearsADescriptionWithNull(t *testing.T) {
	const updated = `{"methodResponses":[["x:Account/set",{"accountId":"c","updated":{"d":null}},"0"]],"sessionState":"1"}`
	tests := []struct {
		name        string
		description string
		want        any
	}{
		{name: "cleared", description: "", want: nil},
		{name: "set", description: "Iris", want: "Iris"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, rec := newClient(t, updated)
			description := test.description
			if err := client.UpdateAccount(t.Context(), "d", AccountPatch{Description: &description}); err != nil {
				t.Fatalf("UpdateAccount: %v", err)
			}
			_, args := rec.call(t, 0, 0)
			object := jsonObject(jsonObject(args["update"])["d"])
			value, ok := object["description"]
			if !ok {
				t.Fatalf("description was not sent: %#v", object)
			}
			if value != test.want {
				t.Errorf("description = %#v, want %#v", value, test.want)
			}
		})
	}
}
