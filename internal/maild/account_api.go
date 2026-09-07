package maild

// account_api.go serves GET /api/account, the one non-JMAP endpoint the client
// calls.
//
// It is the facade's startup self-check. Service.Probe reads it once and
// compares the reported permissions against internal/mail.RequiredPermissions,
// reporting every required name that is absent to the operator as a missing
// permission — it never assumes a permission it cannot see. So a name missing
// from the answer is not a silent no-op: it shows up on the Settings page as a
// misconfigured mail server, whether or not the operation would in fact have
// worked.
//
// maild has no permission model of its own. Every credential that gets past the
// bearer check can do everything, so the honest answer is the whole set, which
// is what GrantedPermissions holds. store_test.go asserts GrantedPermissions
// covers mail.RequiredPermissions, so a name added to the facade fails the
// build here rather than the Settings page in production.

import "net/http"

// serveAccount answers the edition and permission self-check.
//
// The bearer has already been checked by ServeHTTP: this body says what the
// server is, so it is not something an unauthenticated caller should be able to
// enumerate.
func (h *Handler) serveAccount(w http.ResponseWriter, _ *http.Request) {
	// AccountInfo copies GrantedPermissions, so a caller cannot mutate the
	// package's own slice through the value handed to the encoder.
	writeJSON(w, http.StatusOK, AccountInfo())
}
