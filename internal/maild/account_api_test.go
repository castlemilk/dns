package maild

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/mail"
	"github.com/castlemilk/dns/internal/mail/stalwart"
)

// getAccountAPI performs the self-check the way the facade does.
func getAccountAPI(t *testing.T, handler *Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, AccountPath, nil)
	request.Header.Set("Accept", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestTheAccountAPIReportsEveryPermissionTheFacadeNeeds.
//
// Service.Probe compares this list against mail.RequiredPermissions and reports
// every name it cannot see as a missing permission — it never assumes one. So a
// name absent here is not a silent no-op: the operator is told the mail server
// is misconfigured even though every operation would in fact work.
func TestTheAccountAPIReportsEveryPermissionTheFacadeNeeds(t *testing.T) {
	handler, _ := newHandler(t)

	recorder := getAccountAPI(t, handler, testToken)
	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type is %q, want application/json", got)
	}

	// The client's own payload shape: only permissions and edition are read.
	var payload struct {
		Permissions []string `json:"permissions"`
		Edition     string   `json:"edition"`
		Locale      string   `json:"locale"`
	}
	decode(t, recorder.Body.Bytes(), &payload)

	// This is missingPermissions, restated.
	var missing []string
	for _, required := range mail.RequiredPermissions {
		if !slices.Contains(payload.Permissions, required) {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the operator would be shown these as missing permissions: %v", missing)
	}
	if payload.Edition != Edition {
		t.Errorf("edition is %q, want %q", payload.Edition, Edition)
	}
	if payload.Locale == "" {
		t.Error("locale is empty; the captured transcript carries one")
	}
}

// TestTheAccountAPIIsWhatTheClientDecodes drives the frozen client, which is
// the only consumer that matters.
func TestTheAccountAPIIsWhatTheClientDecodes(t *testing.T) {
	handler, _ := newHandler(t)
	client := newFacadeClient(t, handler, testToken)

	info, err := client.Account(context.Background())
	if err != nil {
		t.Fatalf("read the account info: %v", err)
	}
	if info.Edition != Edition {
		t.Errorf("edition is %q, want %q", info.Edition, Edition)
	}
	for _, required := range mail.RequiredPermissions {
		if !slices.Contains(info.Permissions, required) {
			t.Errorf("the client did not see %s", required)
		}
	}
	// The client hands back its own AccountInfo, so a shape change would be a
	// compile error there rather than an empty Settings page.
	same := stalwart.AccountInfo{Edition: info.Edition, Permissions: info.Permissions}
	if same.Edition != info.Edition {
		t.Error("the account info did not round-trip")
	}
}

// TestTheAccountAPIRefusesEverythingElse. Only an authenticated GET answers;
// the body says what this server is, which is not something to enumerate.
func TestTheAccountAPIRefusesEverythingElse(t *testing.T) {
	handler, _ := newHandler(t)

	tests := []struct {
		name   string
		method string
		token  string
		want   int
	}{
		{name: "an authenticated GET", method: http.MethodGet, token: testToken, want: http.StatusOK},
		{name: "no credential", method: http.MethodGet, want: http.StatusUnauthorized},
		{name: "a wrong credential", method: http.MethodGet, token: "nope", want: http.StatusUnauthorized},
		{name: "a POST", method: http.MethodPost, token: testToken, want: http.StatusMethodNotAllowed},
		{name: "a DELETE", method: http.MethodDelete, token: testToken, want: http.StatusMethodNotAllowed},
		{name: "a HEAD", method: http.MethodHead, token: testToken, want: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, AccountPath, nil)
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != test.want {
				t.Fatalf("got %d, want %d", recorder.Code, test.want)
			}
			if strings.Contains(recorder.Body.String(), testToken) {
				t.Error("the answer echoed the token")
			}
			if test.want == http.StatusUnauthorized {
				if got := recorder.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Errorf("WWW-Authenticate is %q, want Bearer", got)
				}
				var body map[string]any
				decode(t, recorder.Body.Bytes(), &body)
				if _, leaked := body["permissions"]; leaked {
					t.Errorf("an unauthenticated caller was told the permissions: %v", body)
				}
			}
		})
	}
}

// TestAccountInfoHandsOutACopy: the response is built from a package-level
// slice, and a handler that returned it directly would let one request's
// encoder alias state every later request reads.
func TestAccountInfoHandsOutACopy(t *testing.T) {
	first := AccountInfo()
	if len(first.Permissions) != len(GrantedPermissions) {
		t.Fatalf("reported %d permissions, want %d", len(first.Permissions), len(GrantedPermissions))
	}
	first.Permissions[0] = "tampered"

	second := AccountInfo()
	if second.Permissions[0] == "tampered" {
		t.Error("AccountInfo aliases GrantedPermissions")
	}
	if GrantedPermissions[0] == "tampered" {
		t.Error("the package-level permission list was mutated")
	}
}

// TestTheAccountBodyIsSmallAndFixed. Nothing about the deployment — no
// hostname, no domain, no credential — belongs in this body.
func TestTheAccountBodyIsSmallAndFixed(t *testing.T) {
	handler, _ := newHandler(t)
	body := getAccountAPI(t, handler, testToken).Body.Bytes()

	var object map[string]json.RawMessage
	decode(t, body, &object)
	for key := range object {
		switch key {
		case "permissions", "edition", "locale":
		default:
			t.Errorf("the account body carries an unexpected %q", key)
		}
	}
	for _, secret := range []string{testToken, testHostname} {
		if strings.Contains(string(body), secret) {
			t.Errorf("the account body carries %q", secret)
		}
	}
}
