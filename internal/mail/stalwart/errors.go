package stalwart

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinels the facade maps onto Connect codes. Everything else arrives as a
// *MethodError or a *SetError, which carry the engine's own type name.
var (
	// ErrTransport is any failure to get a well-formed HTTP 200 out of the
	// server: dial, timeout, 5xx, 404, unreadable body.
	ErrTransport = errors.New("the mail engine did not answer")
	// ErrUnauthorized is HTTP 401: the API key was rejected.
	ErrUnauthorized = errors.New("the mail engine rejected the API key")
	// ErrNotFound is a /get that did not return the requested object.
	ErrNotFound = errors.New("the mail engine does not have this object")
)

// JMAP method-error and set-error types this client reasons about. Stalwart
// answers HTTP 200 for all of them; the failure is inside methodResponses.
const (
	ErrorTypeForbidden            = "forbidden"
	ErrorTypeUnsupportedFilter    = "unsupportedFilter"
	ErrorTypeInvalidPatch         = "invalidPatch"
	ErrorTypeInvalidArguments     = "invalidArguments"
	ErrorTypeInvalidProperties    = "invalidProperties"
	ErrorTypeObjectIsLinked       = "objectIsLinked"
	ErrorTypeNotFound             = "notFound"
	ErrorTypeAccountNotFound      = "accountNotFound"
	ErrorTypePrimaryKeyViolation  = "primaryKeyViolation"
	ErrorTypeAlreadyExists        = "alreadyExists"
	ErrorTypeUnknownMethod        = "unknownMethod"
	ErrorTypeStateMismatch        = "stateMismatch"
	ErrorTypeOverQuota            = "overQuota"
	ErrorTypeTooLarge             = "tooLarge"
	ErrorTypeRateLimit            = "rateLimit"
	ErrorTypeCannotCalculateChang = "cannotCalculateChanges"
)

// MethodError is a `["error",{type,description},id]` entry: the whole method
// failed.
type MethodError struct {
	Method      string
	Type        string
	Description string
}

func (e *MethodError) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("%s: %s", e.Method, e.Type)
	}
	return fmt.Sprintf("%s: %s: %s", e.Method, e.Type, e.Description)
}

// LinkedObject names one child that keeps a parent from being destroyed.
type LinkedObject struct {
	Object string
	ID     string
}

// SetError is one entry of notCreated/notUpdated/notDestroyed.
type SetError struct {
	Method        string
	Operation     string // "create" | "update" | "destroy"
	Key           string // creation id or object id
	Type          string
	Description   string
	Properties    []string
	LinkedObjects []LinkedObject
}

func (e *SetError) Error() string {
	parts := []string{e.Method, e.Type}
	if len(e.Properties) > 0 {
		parts = append(parts, strings.Join(e.Properties, ","))
	}
	if e.Description != "" {
		parts = append(parts, e.Description)
	}
	return strings.Join(parts, ": ")
}

// Property is the first property the server blamed, or "".
func (e *SetError) Property() string {
	if len(e.Properties) == 0 {
		return ""
	}
	return e.Properties[0]
}

// MethodErrorType returns the JMAP method-error type carried by err, if any.
func MethodErrorType(err error) (string, bool) {
	var methodErr *MethodError
	if errors.As(err, &methodErr) {
		return methodErr.Type, true
	}
	return "", false
}

// SetErrorOf returns the *SetError carried by err, if any.
func SetErrorOf(err error) (*SetError, bool) {
	var setErr *SetError
	if errors.As(err, &setErr) {
		return setErr, true
	}
	return nil, false
}

// IsDuplicate reports whether err says the address is already taken. Stalwart
// answers primaryKeyViolation with properties ["email"] or ["name"]; the
// documented alreadyExists is accepted too.
func IsDuplicate(err error) bool {
	if setErr, ok := SetErrorOf(err); ok {
		return setErr.Type == ErrorTypePrimaryKeyViolation || setErr.Type == ErrorTypeAlreadyExists
	}
	return false
}

// IsLinked reports whether err is the "children still exist" refusal.
func IsLinked(err error) bool {
	setErr, ok := SetErrorOf(err)
	return ok && setErr.Type == ErrorTypeObjectIsLinked
}

// IsMissing reports whether err says the object is gone. A destroy of an object
// that is already gone is success for every caller here, so this covers both
// the sentinel and the two JMAP spellings.
func IsMissing(err error) bool {
	if errors.Is(err, ErrNotFound) {
		return true
	}
	if setErr, ok := SetErrorOf(err); ok {
		return setErr.Type == ErrorTypeNotFound || setErr.Type == ErrorTypeAccountNotFound
	}
	if kind, ok := MethodErrorType(err); ok {
		return kind == ErrorTypeNotFound || kind == ErrorTypeAccountNotFound
	}
	return false
}

// IsForbidden reports whether err is Stalwart's edition or permission refusal.
func IsForbidden(err error) bool {
	kind, ok := MethodErrorType(err)
	return ok && kind == ErrorTypeForbidden
}

// IsInvalid reports whether err blames the request rather than the server.
func IsInvalid(err error) bool {
	if setErr, ok := SetErrorOf(err); ok {
		switch setErr.Type {
		case ErrorTypeInvalidPatch, ErrorTypeInvalidArguments, ErrorTypeInvalidProperties:
			return true
		}
	}
	if kind, ok := MethodErrorType(err); ok {
		switch kind {
		case ErrorTypeInvalidPatch, ErrorTypeInvalidArguments, ErrorTypeInvalidProperties:
			return true
		}
	}
	return false
}
