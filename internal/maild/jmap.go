package maild

// jmap.go is the admin surface the control plane talks to: POST /jmap, the
// batched JMAP protocol the frozen client in internal/mail/stalwart speaks, and
// GET /api/account (account_api.go).
//
// CONTRACT.md is the specification. Three of its rules are load-bearing enough
// to restate here, because breaking one of them degrades a customer's mail
// binding silently rather than failing a test:
//
//  1. Every method-level failure is still HTTP 200, with the failure inside
//     methodResponses. Only a rejected bearer is 401; the client maps any other
//     non-200 onto "the mail engine did not answer", which is a lie about what
//     went wrong. Only a request that has no JMAP representation at all — an
//     unparseable body, an oversized one, too many calls — answers a non-200.
//
//  2. Exactly one methodResponse per call, in call order, echoing the call id.
//     The client reads results positionally and names a failing method as
//     calls[index].Name, so a response at the wrong index blames the wrong
//     method.
//
//  3. "#ids" result references are mandatory. Six of the client's calls chain a
//     query and the get that consumes its ids into one request; without the
//     reference the get receives no ids and every list comes back empty.
//
// Nothing here ever puts a credential in a response or a log line. Store errors
// are logged for the operator and answered with a fixed, detail-free
// description, so a value that reached the store cannot travel back out through
// an error string.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The two endpoints the client calls. MAIL_API_URL may carry a path prefix; the
// client appends these to it, so Options.PathPrefix serves them under the same
// prefix without an http.StripPrefix in front.
const (
	// JMAPPath is where every management call is posted.
	JMAPPath = "/jmap"
	// AccountPath is the edition and permission self-check.
	AccountPath = "/api/account"
)

// errTypeServerFail is the type reported when this server, not the request, is
// at fault. It is deliberately not one of the types errors.go reasons about:
// the facade must report it verbatim to the operator rather than mistake it for
// a duplicate address or a missing object.
const errTypeServerFail = "serverFail"

// defaultMailExchangerPriority is what SystemSettings reports when the
// deployment did not configure a list. The value is display-only — the MX
// record the facade publishes comes from its own configuration, never from
// here — but reporting an empty list would read as "this server accepts no
// mail", which is not true.
const defaultMailExchangerPriority = 10

// Options configures the admin surface.
type Options struct {
	// Store is the state file. Required.
	Store *Store
	// Token is the bearer every request must present. Required: an
	// unauthenticated management API would let anyone who can reach the port
	// create a mailbox in any hosted domain. It is compared in constant time
	// and is never logged or echoed.
	Token string
	// Hostname is what this server calls itself, reported as
	// x:SystemSettings.defaultHostname. Required, and it must equal the
	// facade's MAIL_HOSTNAME: the facade refuses to bind, publish or reconcile
	// anything while the two disagree, because the MX record it publishes names
	// MAIL_HOSTNAME and binding against a server that calls itself something
	// else would route mail to a host that does not serve these mailboxes.
	Hostname string
	// MailExchangers is reported for display. Empty means "one exchanger, this
	// host, at the default priority".
	MailExchangers []MailExchanger
	// PathPrefix is the deployment's mount point, "" for the root.
	PathPrefix string
	// RenderZoneFile renders x:Domain.dnsZoneFile, which is where the DKIM
	// records the facade publishes actually come from. It is a hook because the
	// DKIM phase owns the rendering; while it is nil the property is omitted,
	// which the facade reports honestly as a domain with no records to publish
	// rather than publishing a wrong one.
	RenderZoneFile func(ctx context.Context, domain Domain) (string, error)
	// ProvisionDkim generates a new domain's signing keys, synchronously,
	// inside x:Domain/set create. The facade's bind step waits at most 20 s for
	// one active signature per algorithm, so a key generated later leaves the
	// binding degraded until the reconciler catches up. algorithms holds the
	// DkimAlgorithm* names the request asked for.
	ProvisionDkim func(ctx context.Context, domain Domain, algorithms []string, selectorTemplate string) error
	// ListQueue reports the outbound queue for x:QueuedMessage. Nil reads the
	// store's own queue, which is what the delivery path writes to; the hook
	// exists for a deployment whose queue lives somewhere else. Reporting an
	// empty queue while messages are stuck would tell the operator "0 queued"
	// about a mailbox whose mail is going nowhere.
	ListQueue func(ctx context.Context) ([]QueuedMessage, error)
	// Now is the clock, for tests.
	Now func() time.Time
	// Logger receives the detail of a failure whose wire description is
	// deliberately generic. Defaults to slog.Default().
	Logger *slog.Logger
}

// Handler serves POST /jmap and GET /api/account.
type Handler struct {
	store       *Store
	tokenDigest [sha256.Size]byte
	settings    SystemSettings
	prefix      string
	renderZone  func(ctx context.Context, domain Domain) (string, error)
	provision   func(ctx context.Context, domain Domain, algorithms []string, selectorTemplate string) error
	listQueue   func(ctx context.Context) ([]QueuedMessage, error)
	now         func() time.Time
	logger      *slog.Logger
	// sessionState is opaque to the client, which decodes and ignores it. It is
	// fresh per process, which is the honest statement a restarted server can
	// make about its state.
	sessionState string
}

// NewHandler builds the admin surface.
func NewHandler(options Options) (*Handler, error) {
	if options.Store == nil {
		return nil, errors.New("maild: the admin API needs a store")
	}
	// The token is required rather than defaulted: the client omits the
	// Authorization header entirely when its own token is empty, so an empty
	// token here would not fail loudly, it would serve an open API.
	if strings.TrimSpace(options.Token) == "" {
		return nil, errors.New("maild: the admin API needs an API token")
	}
	hostname := NormalizeDomainName(options.Hostname)
	if hostname == "" {
		return nil, errors.New("maild: the admin API needs a hostname")
	}
	exchangers := append([]MailExchanger(nil), options.MailExchangers...)
	if len(exchangers) == 0 {
		exchangers = []MailExchanger{{Priority: defaultMailExchangerPriority}}
	}
	state := make([]byte, 4)
	if _, err := rand.Read(state); err != nil {
		return nil, fmt.Errorf("maild: generate a session state: %w", err)
	}
	handler := &Handler{
		store:       options.Store,
		tokenDigest: sha256.Sum256([]byte(strings.TrimSpace(options.Token))),
		settings: SystemSettings{
			DefaultHostname: hostname,
			MailExchangers:  exchangers,
		},
		prefix:       strings.TrimSuffix(options.PathPrefix, "/"),
		renderZone:   options.RenderZoneFile,
		provision:    options.ProvisionDkim,
		listQueue:    options.ListQueue,
		now:          options.Now,
		logger:       options.Logger,
		sessionState: hex.EncodeToString(state),
	}
	if handler.now == nil {
		handler.now = time.Now
	}
	if handler.logger == nil {
		handler.logger = slog.Default()
	}
	return handler, nil
}

// ServeHTTP routes the two endpoints.
//
// The bearer is checked before the path is looked at. Checking the method or
// the route first makes the surface enumerable without a credential: POST
// /api/account would answer 405 and POST /nope 404, which tells an anonymous
// caller exactly which paths this process serves and which verbs they take.
// Every request that fails the bearer now gets the same 401 whatever it asked
// for, so an unauthenticated caller learns only that something is listening.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		h.writeUnauthorized(w)
		return
	}
	switch r.URL.Path {
	case h.prefix + JMAPPath:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		h.serveJMAP(w, r)
	case h.prefix + AccountPath:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		h.serveAccount(w, r)
	default:
		writeProblem(w, http.StatusNotFound, "not_found")
	}
}

// authorized checks the bearer in constant time.
//
// Both sides are hashed first so the comparison is over two fixed-length
// values: subtle.ConstantTimeCompare returns early for a length mismatch, which
// would otherwise make the token's length observable. A missing or malformed
// header hashes the empty string and fails the same way a wrong token does, so
// the answer never distinguishes "no credential" from "wrong credential".
func (h *Handler) authorized(r *http.Request) bool {
	presented := ""
	if scheme, value, found := strings.Cut(r.Header.Get("Authorization"), " "); found && strings.EqualFold(scheme, "Bearer") {
		presented = strings.TrimSpace(value)
	}
	digest := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(digest[:], h.tokenDigest[:]) == 1
}

// writeUnauthorized answers a rejected credential. The body says nothing about
// the token — not whether one was sent, not whether one is configured — and 401
// is reserved for exactly this case, because the client maps it, and only it,
// onto "the mail engine rejected the API key".
func (h *Handler) writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeProblem(w, http.StatusUnauthorized, "unauthorized")
}

// serveJMAP runs one batch.
func (h *Handler) serveJMAP(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > MaxResponseBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxResponseBytes+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "unreadable_request")
		return
	}
	if len(body) > MaxResponseBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}

	// A request-level violation has no JMAP representation, so it answers a
	// non-200 and the client reports a transport error — which is the honest
	// description of a body it could not have meant to send.
	var request struct {
		// Using is parsed and ignored: this server implements exactly one
		// capability set, so refusing a request over its "using" list could
		// only ever reject a caller that is otherwise correct.
		Using       []string          `json:"using"`
		MethodCalls []json.RawMessage `json:"methodCalls"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeProblem(w, http.StatusBadRequest, "malformed_request")
		return
	}
	if len(request.MethodCalls) > MaxCallsPerRequest {
		writeProblem(w, http.StatusBadRequest, "too_many_method_calls")
		return
	}

	calls := make([]jmapCall, 0, len(request.MethodCalls))
	for _, raw := range request.MethodCalls {
		call, err := parseJMAPCall(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "malformed_method_call")
			return
		}
		calls = append(calls, call)
	}

	responses := make([]jmapResponse, 0, len(calls))
	for _, call := range calls {
		responses = append(responses, h.dispatch(r.Context(), call, responses))
	}
	writeJSON(w, http.StatusOK, jmapResponseBody{MethodResponses: responses, SessionState: h.sessionState})
}

// -----------------------------------------------------------------------------
// Wire plumbing
// -----------------------------------------------------------------------------

// jmapCall is one entry of methodCalls: [name, args, id].
type jmapCall struct {
	Name string
	Args json.RawMessage
	ID   string
}

func parseJMAPCall(raw json.RawMessage) (jmapCall, error) {
	var triple []json.RawMessage
	if err := json.Unmarshal(raw, &triple); err != nil || len(triple) != 3 {
		return jmapCall{}, errors.New("a method call was not a [name, args, id] triple")
	}
	var call jmapCall
	if err := json.Unmarshal(triple[0], &call.Name); err != nil {
		return jmapCall{}, errors.New("a method call had no name")
	}
	if err := json.Unmarshal(triple[2], &call.ID); err != nil {
		return jmapCall{}, errors.New("a method call had no call id")
	}
	call.Args = triple[1]
	if len(call.Args) == 0 {
		call.Args = json.RawMessage("{}")
	}
	return call, nil
}

// jmapResponse is one entry of methodResponses. It marshals as the triple the
// client requires; anything else is a transport error there.
type jmapResponse struct {
	Name string
	Args any
	ID   string
}

// MarshalJSON renders [name, args, id].
func (r jmapResponse) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{r.Name, r.Args, r.ID})
}

type jmapResponseBody struct {
	MethodResponses []jmapResponse `json:"methodResponses"`
	SessionState    string         `json:"sessionState"`
}

// methodErrorArgs is the args of an ["error", …] response: the whole call
// failed.
type methodErrorArgs struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// methodFailure is a method error on its way out of a handler.
type methodFailure struct {
	Type        string
	Description string
}

func (e *methodFailure) Error() string { return e.Type + ": " + e.Description }

func fail(kind, description string) error {
	return &methodFailure{Type: kind, Description: description}
}

func errorResponse(callID, kind, description string) jmapResponse {
	return jmapResponse{Name: "error", Args: methodErrorArgs{Type: kind, Description: description}, ID: callID}
}

// resultReferenceArgs is a "#ids" back-reference.
type resultReferenceArgs struct {
	ResultOf string `json:"resultOf"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

// callArgs is every argument any of the fifteen methods takes. Decoding them
// all at once is safe because no method shares an argument name with a
// different meaning, and it keeps the "#ids" resolution in one place.
type callArgs struct {
	IDs          []string                   `json:"ids"`
	IDsReference *resultReferenceArgs       `json:"#ids"`
	Properties   []string                   `json:"properties"`
	Filter       json.RawMessage            `json:"filter"`
	Limit        *int                       `json:"limit"`
	Position     *int                       `json:"position"`
	Create       map[string]json.RawMessage `json:"create"`
	Update       map[string]json.RawMessage `json:"update"`
	Destroy      []string                   `json:"destroy"`
}

// dispatch runs one call and produces exactly one response for it.
func (h *Handler) dispatch(ctx context.Context, call jmapCall, produced []jmapResponse) jmapResponse {
	var args callArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return errorResponse(call.ID, ErrTypeInvalidArguments, "the arguments were not an object of the expected shape")
	}
	if args.IDsReference != nil {
		if len(args.IDs) > 0 {
			return errorResponse(call.ID, ErrTypeInvalidResultReference, "a call carried both ids and an #ids reference")
		}
		ids, err := resolveIDsReference(produced, *args.IDsReference)
		if err != nil {
			return errorResponse(call.ID, ErrTypeInvalidResultReference, err.Error())
		}
		args.IDs = ids
	}
	if len(args.IDs) > MaxObjectsPerCall {
		return errorResponse(call.ID, ErrTypeTooLarge, fmt.Sprintf("a call may name at most %d ids", MaxObjectsPerCall))
	}

	value, err := h.invoke(ctx, call.Name, args)
	if err != nil {
		var failure *methodFailure
		if errors.As(err, &failure) {
			return errorResponse(call.ID, failure.Type, failure.Description)
		}
		// The detail goes to the operator's log, never onto the wire: a store
		// error names the object it refused, and an error string is the one
		// place a value that reached the store could travel back out.
		h.logger.Error("maild: a JMAP method failed", "method", call.Name, "error", err)
		return errorResponse(call.ID, errTypeServerFail, "the request could not be completed")
	}
	return jmapResponse{Name: call.Name, Args: value, ID: call.ID}
}

// invoke is the closed set of methods. Anything else is unknownMethod, which is
// what the client expects for a name this server does not implement — never a
// 500, which it would report as "the mail engine did not answer".
func (h *Handler) invoke(ctx context.Context, name string, args callArgs) (any, error) {
	switch name {
	// Per-domain delivery metrics are an Enterprise-only Stalwart feature. The
	// facade probes for them and treats "forbidden" as "this edition cannot
	// produce that number", degrading to showing nothing — whereas unknownMethod
	// reads as a broken engine. Answering forbidden is therefore the honest
	// reply for a server that genuinely does not offer the capability, and
	// internal/mail/integration_test.go asserts exactly that.
	case "x:Metric/query":
		return nil, fail(ErrTypeForbidden, "per-domain delivery metrics are not available in this edition")

	case MethodSystemSettingsGet:
		return h.systemSettingsGet(args)

	case MethodDomainQuery:
		return h.domainQuery(ctx, args)
	case MethodDomainGet:
		return h.domainGet(ctx, args)
	case MethodDomainSet:
		return h.domainSet(ctx, args)

	case MethodDkimQuery:
		return h.dkimQuery(ctx, args)
	case MethodDkimGet:
		return h.dkimGet(ctx, args)
	case MethodDkimSet:
		return h.dkimSet(ctx, args)

	case MethodAccountQuery:
		return h.accountQuery(ctx, args)
	case MethodAccountGet:
		return h.accountGet(ctx, args)
	case MethodAccountSet:
		return h.accountSet(ctx, args)

	case MethodListQuery:
		return h.listQuery(ctx, args)
	case MethodListGet:
		return h.listGet(ctx, args)
	case MethodListSet:
		return h.listSet(ctx, args)

	case MethodQueueQuery:
		return h.queueQuery(ctx, args)
	case MethodQueueGet:
		return h.queueGet(ctx, args)

	default:
		return nil, fail(ErrTypeUnknownMethod, "this server does not implement that method")
	}
}

// resolveIDsReference implements the one back-reference the client sends: the
// ids of an earlier query in the same request. Responses are produced in order,
// so the referenced call has already run.
//
// Only the "/ids" pointer is supported. Guessing at another pointer would risk
// binding the wrong value to ids, and silently getting the wrong objects is
// worse than an error the caller can see.
func resolveIDsReference(produced []jmapResponse, reference resultReferenceArgs) ([]string, error) {
	if reference.Path != "/ids" {
		return nil, errors.New("only the /ids result path is supported")
	}
	for _, response := range produced {
		if response.ID != reference.ResultOf {
			continue
		}
		if response.Name != reference.Name {
			// This is also how a reference to a call that failed is reported:
			// its response is named "error", which never matches.
			return nil, errors.New("the referenced call did not answer the named method")
		}
		raw, err := json.Marshal(response.Args)
		if err != nil {
			return nil, errors.New("the referenced response could not be read")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, errors.New("the referenced response was not an object")
		}
		value, ok := object["ids"]
		if !ok {
			return nil, errors.New("the referenced response has no ids")
		}
		var ids []string
		if err := json.Unmarshal(value, &ids); err != nil {
			return nil, errors.New("the referenced ids were not an array of strings")
		}
		return ids, nil
	}
	return nil, errors.New("no earlier call in this request has that id")
}

// -----------------------------------------------------------------------------
// Shared response shapes
// -----------------------------------------------------------------------------

// getResponseArgs is the args of every /get. Only "list" is read by the client;
// the rest is in the captured transcripts and is sent for fidelity.
type getResponseArgs struct {
	State    string           `json:"state"`
	List     []map[string]any `json:"list"`
	NotFound []string         `json:"notFound"`
}

// queryResponseArgs is the args of every /query.
type queryResponseArgs struct {
	IDs                 []string `json:"ids"`
	Position            int      `json:"position"`
	QueryState          string   `json:"queryState"`
	CanCalculateChanges bool     `json:"canCalculateChanges"`
}

// linkedObjectArgs names one child that keeps a parent from being destroyed.
type linkedObjectArgs struct {
	Object string `json:"object"`
	ID     string `json:"id"`
}

// setErrorArgs is one entry of notCreated/notUpdated/notDestroyed.
type setErrorArgs struct {
	Type          string             `json:"type"`
	Description   string             `json:"description,omitempty"`
	Properties    []string           `json:"properties,omitempty"`
	LinkedObjects []linkedObjectArgs `json:"linkedObjects,omitempty"`
}

type createdArgs struct {
	ID string `json:"id"`
}

// setResponseArgs is the args of every /set.
type setResponseArgs struct {
	Created      map[string]createdArgs   `json:"created,omitempty"`
	Updated      map[string]any           `json:"updated,omitempty"`
	Destroyed    []string                 `json:"destroyed,omitempty"`
	NotCreated   map[string]*setErrorArgs `json:"notCreated,omitempty"`
	NotUpdated   map[string]*setErrorArgs `json:"notUpdated,omitempty"`
	NotDestroyed map[string]*setErrorArgs `json:"notDestroyed,omitempty"`
}

func (s *setResponseArgs) created(creationID, id string) {
	if s.Created == nil {
		s.Created = map[string]createdArgs{}
	}
	s.Created[creationID] = createdArgs{ID: id}
}

func (s *setResponseArgs) notCreated(creationID string, failure *setErrorArgs) {
	if s.NotCreated == nil {
		s.NotCreated = map[string]*setErrorArgs{}
	}
	s.NotCreated[creationID] = failure
}

// updated records a successful patch. The value is null, which is what the
// client reads and discards.
func (s *setResponseArgs) updated(id string) {
	if s.Updated == nil {
		s.Updated = map[string]any{}
	}
	s.Updated[id] = nil
}

func (s *setResponseArgs) notUpdated(id string, failure *setErrorArgs) {
	if s.NotUpdated == nil {
		s.NotUpdated = map[string]*setErrorArgs{}
	}
	s.NotUpdated[id] = failure
}

func (s *setResponseArgs) destroyed(id string) {
	s.Destroyed = append(s.Destroyed, id)
}

func (s *setResponseArgs) notDestroyed(id string, failure *setErrorArgs) {
	if s.NotDestroyed == nil {
		s.NotDestroyed = map[string]*setErrorArgs{}
	}
	s.NotDestroyed[id] = failure
}

// storeSetError maps a store refusal onto the per-object error the facade
// reasons about. It returns nil for a failure that is this server's fault
// rather than the request's; the caller then reports a method-level serverFail
// and logs the detail.
//
// duplicateProperty is the property the facade blames for a taken address:
// "name" for a domain, "email" for a mailbox.
func storeSetError(err error, duplicateProperty string) *setErrorArgs {
	var linked *LinkedError
	switch {
	case errors.As(err, &linked):
		failure := &setErrorArgs{Type: ErrTypeObjectIsLinked, Description: "other objects still reference this one"}
		for _, child := range linked.Linked {
			failure.LinkedObjects = append(failure.LinkedObjects, linkedObjectArgs(child))
		}
		return failure
	case errors.Is(err, ErrExists):
		return &setErrorArgs{
			Type:        ErrTypePrimaryKeyViolation,
			Description: "that address is already in use",
			Properties:  []string{duplicateProperty},
		}
	case errors.Is(err, ErrNotFound):
		return &setErrorArgs{Type: ErrTypeNotFound, Description: "no such object"}
	case errors.Is(err, ErrInvalid):
		return &setErrorArgs{Type: ErrTypeInvalidProperties, Description: "the object was not valid"}
	default:
		return nil
	}
}

func invalidProperties(description string, properties ...string) *setErrorArgs {
	return &setErrorArgs{Type: ErrTypeInvalidProperties, Description: description, Properties: properties}
}

func invalidPatch(description string, properties ...string) *setErrorArgs {
	return &setErrorArgs{Type: ErrTypeInvalidPatch, Description: description, Properties: properties}
}

// -----------------------------------------------------------------------------
// Argument helpers
// -----------------------------------------------------------------------------

// page applies limit and position to a query result.
//
// Paging is a correctness requirement, not a nicety: ListDomains and ListLists
// loop position += 500 and stop when a page returns fewer than 500 ids, so a
// server that ignored position would loop forever once a deployment had 500
// domains.
func page(ids []string, args callArgs) ([]string, int, error) {
	position := 0
	if args.Position != nil {
		position = *args.Position
	}
	if position < 0 {
		return nil, 0, fail(ErrTypeInvalidArguments, "position must not be negative")
	}
	limit := MaxObjectsPerCall
	if args.Limit != nil {
		limit = *args.Limit
		if limit < 0 {
			return nil, 0, fail(ErrTypeInvalidArguments, "limit must not be negative")
		}
		if limit > MaxObjectsPerCall {
			limit = MaxObjectsPerCall
		}
	}
	if position >= len(ids) {
		return []string{}, position, nil
	}
	ids = ids[position:]
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, position, nil
}

func queryResult(ids []string, position int) queryResponseArgs {
	if ids == nil {
		ids = []string{}
	}
	return queryResponseArgs{IDs: ids, Position: position, QueryState: "n", CanCalculateChanges: true}
}

// parseFilter decodes a query filter and refuses a property this object cannot
// be filtered on. unsupportedFilter is the honest answer: silently ignoring a
// filter would return objects the caller asked not to see.
func parseFilter(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fail(ErrTypeUnsupportedFilter, "the filter was not an object")
	}
	for key := range fields {
		permitted := false
		for _, name := range allowed {
			if key == name {
				permitted = true
				break
			}
		}
		if !permitted {
			return nil, fail(ErrTypeUnsupportedFilter, "filtering on that property is not supported")
		}
	}
	return fields, nil
}

// filterString reads one string-valued filter property.
func filterString(fields map[string]json.RawMessage, key string) (string, bool, error) {
	raw, ok := fields[key]
	if !ok {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, fail(ErrTypeUnsupportedFilter, "that filter property must be a string")
	}
	return value, true, nil
}

// wants reports whether a property was asked for. An empty properties list
// means "everything", which is JMAP's own default.
func wants(properties []string, name string) bool {
	if len(properties) == 0 {
		return true
	}
	for _, property := range properties {
		if property == name {
			return true
		}
	}
	return false
}

func sortedKeysOf[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// stringPatch reads a patch value that is either a string or JSON null, which
// is how the client clears a description.
func stringPatch(raw json.RawMessage) (string, error) {
	if string(raw) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("the value must be a string or null")
	}
	return value, nil
}

// -----------------------------------------------------------------------------
// Address validation
//
// Every address that enters through this API goes through address.go, because
// this is the boundary where an address stops being a string the caller chose
// and starts being one this server writes into an envelope and a header. A
// recipient carrying a CR or an LF reaches delivered mail as a header the
// caller wrote; the old code lowercased and TrimSpace'd, which strips a
// trailing CR — hiding the one byte that matters — and passes an embedded one
// straight through.
// -----------------------------------------------------------------------------

// addressReason renders a ParseAddress refusal as the clause that goes on the
// wire. The refusal texts deliberately never quote the input, so handing one
// back cannot echo the untrusted string; only the sentinel's prefix is dropped,
// because "maild: that is not a valid mailbox address:" in front of every one
// of them says nothing the surrounding description has not already said.
func addressReason(err error) string {
	return strings.TrimPrefix(err.Error(), ErrInvalidAddress.Error()+": ")
}

// mailboxLocalPart validates one local part by validating the address it will
// become. A local part has no meaning on its own — "ada" is only ever
// "ada@acme.dev" — and it is the composed address that ends up in an envelope,
// so the composed address is what has to hold. A local part carrying its own @
// therefore fails as an address with two of them, which is the right answer:
// the client sends a local part here, never a full address.
//
// ASCII spaces around the value are stripped, exactly as ParseAddress strips
// them around a whole address and for the same reason: a space cannot hide
// anything, and a CR, LF, tab or NUL at either end is still a refusal.
func mailboxLocalPart(raw, domainName string) (string, error) {
	address, err := ParseAddress(strings.Trim(raw, " ") + "@" + domainName)
	if err != nil {
		return "", err
	}
	return address.Local, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		// Every value marshalled here is built from this package's own types,
		// so this cannot happen; answering 500 rather than a truncated 200 is
		// the honest failure if it ever does.
		http.Error(w, `{"error":"encoding_failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A write that fails here means the client is gone or the connection is
	// broken: there is nothing left to report to, and the status line has
	// already been sent, so there is nothing to roll back either.
	if _, err := w.Write(body); err != nil {
		return
	}
}

// writeProblem answers a request-level failure. The reason is a fixed token, so
// no request value and no credential can travel back out through it.
func writeProblem(w http.ResponseWriter, status int, reason string) {
	writeJSON(w, status, map[string]string{"error": reason})
}

// -----------------------------------------------------------------------------
// x:SystemSettings
//
// This is the first call the facade makes and the one every other operation
// depends on: Probe and requireHostname refuse to bind, publish or reconcile
// anything unless defaultHostname equals MAIL_HOSTNAME.
// -----------------------------------------------------------------------------

func (h *Handler) systemSettingsGet(args callArgs) (any, error) {
	response := newGetResponse()
	for _, id := range args.IDs {
		// A bare /get with no ids returns an empty list, which is what the
		// client's doc comment records and why it reads the singleton by id.
		if id != SingletonID {
			response.NotFound = append(response.NotFound, id)
			continue
		}
		object, err := Project(h.settings.Wire(), args.Properties)
		if err != nil {
			return nil, err
		}
		response.List = append(response.List, object)
	}
	return response, nil
}

// -----------------------------------------------------------------------------
// x:Domain
// -----------------------------------------------------------------------------

func (h *Handler) domainQuery(ctx context.Context, args callArgs) (any, error) {
	fields, err := parseFilter(args.Filter, "name")
	if err != nil {
		return nil, err
	}
	name, filtered, err := filterString(fields, "name")
	if err != nil {
		return nil, err
	}

	var ids []string
	if filtered {
		// The exact-name lookup FindDomain sends. The id derives from the name,
		// so this is a keyed read rather than a scan.
		domain, err := h.store.FindDomain(ctx, name)
		switch {
		case err == nil:
			ids = []string{domain.ID}
		case errors.Is(err, ErrNotFound):
			ids = nil
		default:
			return nil, err
		}
	} else {
		domains, err := h.store.ListDomains(ctx)
		if err != nil {
			return nil, err
		}
		for _, domain := range domains {
			ids = append(ids, domain.ID)
		}
	}

	paged, position, err := page(ids, args)
	if err != nil {
		return nil, err
	}
	return queryResult(paged, position), nil
}

func (h *Handler) domainGet(ctx context.Context, args callArgs) (any, error) {
	response := newGetResponse()
	// dnsZoneFile is the expensive property — it is the rendered BIND text the
	// facade parses its DKIM records out of — and the list query deliberately
	// omits it, so it is rendered only when it was asked for.
	renderZone := wants(args.Properties, "dnsZoneFile")
	for _, id := range args.IDs {
		domain, err := h.store.GetDomain(ctx, id)
		if errors.Is(err, ErrNotFound) {
			response.NotFound = append(response.NotFound, id)
			continue
		}
		if err != nil {
			return nil, err
		}
		zoneFile := ""
		if renderZone && h.renderZone != nil {
			zoneFile, err = h.renderZone(ctx, domain)
			if err != nil {
				return nil, fmt.Errorf("maild: render the zone file of %s: %w", domain.Name, err)
			}
			// The DKIM hook renders signing records; the client
			// autoconfiguration records the facade also parses out of this
			// text are the server's own answer about which services it
			// serves, so they are appended here rather than left to a hook a
			// deployment could forget to wire.
			zoneFile = AppendAutoconfigRecords(zoneFile, domain.Name, h.settings.DefaultHostname)
		}
		object, err := Project(domain.Wire(zoneFile), args.Properties)
		if err != nil {
			return nil, err
		}
		response.List = append(response.List, object)
	}
	return response, nil
}

// dkimManagementArgs is the DKIM policy a domain is created with.
type dkimManagementArgs struct {
	Type             string          `json:"@type"`
	Algorithms       map[string]bool `json:"algorithms"`
	SelectorTemplate string          `json:"selectorTemplate"`
}

type domainCreateArgs struct {
	Name             string  `json:"name"`
	Description      *string `json:"description"`
	IsEnabled        *bool   `json:"isEnabled"`
	ReportAddressURI string  `json:"reportAddressUri"`
	AllowRelaying    bool    `json:"allowRelaying"`
	CatchAllAddress  *string `json:"catchAllAddress"`
	SubAddressing    *struct {
		Type string `json:"@type"`
	} `json:"subAddressing"`
	DkimManagement *dkimManagementArgs `json:"dkimManagement"`
}

func (h *Handler) domainSet(ctx context.Context, args callArgs) (any, error) {
	response := &setResponseArgs{}

	for _, creationID := range sortedKeysOf(args.Create) {
		var input domainCreateArgs
		if err := json.Unmarshal(args.Create[creationID], &input); err != nil {
			response.notCreated(creationID, invalidProperties("the object was not of the expected shape"))
			continue
		}
		// Only the two repairs the store itself performs are applied before
		// the check: ASCII spaces at the ends, and one root dot, because
		// store.go keys "acme.dev." and "acme.dev" as the same domain. The
		// check then runs on that, not on NormalizeDomainName's output —
		// NormalizeDomainName is a TrimSpace, and a TrimSpace silently eats a
		// leading or trailing CR, which is the byte most in need of a refusal.
		// Everything else — an interior space, a control character, an
		// over-long label, an underscore — is refused rather than repaired: a
		// domain name is the right-hand side of every address in it.
		candidate := strings.TrimSuffix(strings.Trim(input.Name, " "), ".")
		if candidate == "" {
			response.notCreated(creationID, invalidProperties("a domain needs a name", "name"))
			continue
		}
		if _, err := ParseDomainName(candidate); err != nil {
			response.notCreated(creationID, invalidProperties("that domain name is not usable: "+addressReason(err), "name"))
			continue
		}
		name := NormalizeDomainName(candidate)
		// allowRelaying is accepted only as false. Nothing in this server reads
		// a per-domain relay policy: relaying is decided by whether the session
		// authenticated (delivery.go refuses an unhosted recipient outright
		// otherwise), which is the check that matters. Storing a true would
		// record a security-relevant setting that does nothing, and the next
		// person to read the model would reasonably believe it did. The facade
		// always sends false; anything else is refused rather than kept as a
		// lie.
		if input.AllowRelaying {
			response.notCreated(creationID, invalidProperties("this server has no per-domain relaying: relaying requires an authenticated submission session", "allowRelaying"))
			continue
		}
		reportAddress, failure := reportAddressURI(input.ReportAddressURI)
		if failure != nil {
			response.notCreated(creationID, failure)
			continue
		}
		domain := NewDomain(name, h.now())
		if input.Description != nil {
			// Byte for byte: the facade adopts a domain only when its
			// description equals "simple zone "+zoneID exactly, so a
			// description this server rewrote would orphan the domain.
			domain.Description = *input.Description
		}
		if input.IsEnabled != nil {
			domain.Enabled = *input.IsEnabled
		}
		domain.ReportAddressURI = reportAddress
		if input.SubAddressing != nil {
			domain.SubAddressing = strings.EqualFold(input.SubAddressing.Type, "Enabled")
		}
		catchAll, failure := catchAllLocalPart(input.CatchAllAddress, name)
		if failure != nil {
			response.notCreated(creationID, failure)
			continue
		}
		domain.CatchAllLocalPart = catchAll

		stored, err := h.store.CreateDomain(ctx, domain)
		if err != nil {
			if failure := storeSetError(err, "name"); failure != nil {
				response.notCreated(creationID, failure)
				continue
			}
			return nil, err
		}
		if failure := h.provisionDomainDkim(ctx, stored, input.DkimManagement); failure != nil {
			response.notCreated(creationID, failure)
			continue
		}
		response.created(creationID, stored.ID)
	}

	for _, id := range sortedKeysOf(args.Update) {
		var patch map[string]json.RawMessage
		if err := json.Unmarshal(args.Update[id], &patch); err != nil {
			response.notUpdated(id, invalidPatch("the patch was not an object"))
			continue
		}
		domain, err := h.store.GetDomain(ctx, id)
		if errors.Is(err, ErrNotFound) {
			response.notUpdated(id, &setErrorArgs{Type: ErrTypeNotFound, Description: "no such domain"})
			continue
		}
		if err != nil {
			return nil, err
		}
		if failure := applyDomainPatch(&domain, patch); failure != nil {
			response.notUpdated(id, failure)
			continue
		}
		if _, err := h.store.PutDomain(ctx, domain); err != nil {
			if failure := storeSetError(err, "name"); failure != nil {
				response.notUpdated(id, failure)
				continue
			}
			return nil, err
		}
		response.updated(id)
	}

	for _, id := range args.Destroy {
		// A domain still holding mailboxes, mailing lists or signatures is
		// refused with objectIsLinked naming them; the facade deletes the
		// signatures and retries. Destroying a domain that is not there is
		// success, because the caller is making its absence true.
		if err := h.store.DeleteDomain(ctx, id); err != nil {
			if failure := storeSetError(err, "name"); failure != nil {
				response.notDestroyed(id, failure)
				continue
			}
			return nil, err
		}
		response.destroyed(id)
	}

	return response, nil
}

// applyDomainPatch applies the three keys the client ever sends. An unknown key
// is refused rather than ignored: reporting a patch as applied when part of it
// was dropped is the silent failure this whole contract exists to avoid.
func applyDomainPatch(domain *Domain, patch map[string]json.RawMessage) *setErrorArgs {
	for _, key := range sortedKeysOf(patch) {
		raw := patch[key]
		switch key {
		case "description":
			value, err := stringPatch(raw)
			if err != nil {
				return invalidPatch(err.Error(), key)
			}
			domain.Description = value
		case "reportAddressUri":
			value, err := stringPatch(raw)
			if err != nil {
				return invalidPatch(err.Error(), key)
			}
			reportAddress, failure := reportAddressURI(value)
			if failure != nil {
				return failure
			}
			domain.ReportAddressURI = reportAddress
		case "isEnabled":
			var value bool
			if err := json.Unmarshal(raw, &value); err != nil {
				return invalidPatch("the value must be a boolean", key)
			}
			domain.Enabled = value
		default:
			return invalidPatch("that property cannot be patched", key)
		}
	}
	return nil
}

// catchAllLocalPart reads the optional catch-all address. The facade always
// sends null; an address in another domain is refused rather than silently
// dropped, because a catch-all that does not exist would send mail nowhere.
// It is written either as a bare local part or as a full address; both forms
// are validated as the address they name, because a catch-all is what an
// otherwise-undeliverable envelope is rewritten to.
func catchAllLocalPart(address *string, domainName string) (string, *setErrorArgs) {
	if address == nil {
		return "", nil
	}
	// Trim ASCII spaces only. strings.TrimSpace would eat a trailing CR, which
	// is precisely the byte this has to refuse.
	value := strings.Trim(*address, " ")
	if value == "" {
		return "", nil
	}
	if !strings.Contains(value, "@") {
		local, err := mailboxLocalPart(value, domainName)
		if err != nil {
			return "", invalidProperties("the catch-all address is not usable: "+addressReason(err), "catchAllAddress")
		}
		return local, nil
	}
	parsed, err := ParseAddress(value)
	if err != nil {
		return "", invalidProperties("the catch-all address is not usable: "+addressReason(err), "catchAllAddress")
	}
	if parsed.Domain != domainName {
		return "", invalidProperties("the catch-all address must be in this domain", "catchAllAddress")
	}
	return parsed.Local, nil
}

// reportAddressURI validates the "mailto:…" the facade sends for a domain. The
// value is stored and is where this server addresses its own DMARC and TLS
// reports, so an unvalidated one is an address this server would later put in
// an envelope. Empty is allowed — the property is optional — and the scheme is
// optional too, because only the address inside it is ever used. What comes
// back is the trimmed input, not a rewrite: nothing about this property is
// canonicalised, so nothing about it can drift.
func reportAddressURI(raw string) (string, *setErrorArgs) {
	value := strings.Trim(raw, " ")
	if value == "" {
		return "", nil
	}
	const scheme = "mailto:"
	address := value
	if len(address) >= len(scheme) && strings.EqualFold(address[:len(scheme)], scheme) {
		address = address[len(scheme):]
	}
	if _, err := ParseAddress(address); err != nil {
		return "", invalidProperties("the report address is not usable: "+addressReason(err), "reportAddressUri")
	}
	return value, nil
}

// provisionDomainDkim generates the new domain's signing keys inside the create
// call, which is what makes a binding complete: BindMailDomain waits at most
// 20 s for one active signature per algorithm before it reports the binding as
// degraded.
//
// It returns the per-object error to report, or nil on success.
func (h *Handler) provisionDomainDkim(ctx context.Context, domain Domain, management *dkimManagementArgs) *setErrorArgs {
	if h.provision == nil || management == nil || !strings.EqualFold(management.Type, "Automatic") {
		return nil
	}
	algorithms := make([]string, 0, len(RequestedDkimAlgorithms))
	for _, algorithm := range RequestedDkimAlgorithms {
		if management.Algorithms[algorithm] {
			algorithms = append(algorithms, algorithm)
		}
	}
	if len(algorithms) == 0 {
		if len(management.Algorithms) > 0 {
			// Named algorithms this server cannot generate. Creating the domain
			// with no keys would leave its mail unsigned without saying so.
			return invalidProperties("no requested DKIM algorithm is supported", "dkimManagement")
		}
		algorithms = append(algorithms, RequestedDkimAlgorithms...)
	}
	template := management.SelectorTemplate
	if strings.TrimSpace(template) == "" {
		template = DefaultSelectorTemplate
	}

	if err := h.provision(ctx, domain, algorithms, template); err != nil {
		h.logger.Error("maild: generate the signing keys of a new domain", "domain", domain.Name, "error", err)
		if rollback := h.rollbackDomain(ctx, domain.ID); rollback != nil {
			// The domain outlived the failure. Reporting it as not created
			// would strand it: the caller's retry would be answered
			// primaryKeyViolation and the operator told the name is taken. The
			// create that did happen is reported instead, and the domain's
			// missing keys surface as a degraded binding the reconciler heals.
			h.logger.Error("maild: roll back a half-provisioned domain", "domain", domain.Name, "error", rollback)
			return nil
		}
		return &setErrorArgs{Type: errTypeServerFail, Description: "the domain's signing keys could not be generated"}
	}
	return nil
}

// rollbackDomain undoes a create whose key generation failed. The keys are
// removed first because the store refuses to delete a domain anything still
// references.
func (h *Handler) rollbackDomain(ctx context.Context, domainID string) error {
	keys, err := h.store.ListDkimKeys(ctx, domainID)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := h.store.DeleteDkimKey(ctx, key.ID); err != nil {
			return err
		}
	}
	return h.store.DeleteDomain(ctx, domainID)
}

// -----------------------------------------------------------------------------
// x:DkimSignature
// -----------------------------------------------------------------------------

func (h *Handler) dkimQuery(ctx context.Context, args callArgs) (any, error) {
	// The domainId filter must be supported server-side: an unsupportedFilter
	// here breaks every bind, because ListDkim is how the facade waits for the
	// keys it is about to publish.
	fields, err := parseFilter(args.Filter, "domainId")
	if err != nil {
		return nil, err
	}
	domainID, _, err := filterString(fields, "domainId")
	if err != nil {
		return nil, err
	}
	keys, err := h.store.ListDkimKeys(ctx, domainID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		ids = append(ids, key.ID)
	}
	paged, position, err := page(ids, args)
	if err != nil {
		return nil, err
	}
	return queryResult(paged, position), nil
}

func (h *Handler) dkimGet(ctx context.Context, args callArgs) (any, error) {
	response := newGetResponse()
	for _, id := range args.IDs {
		key, err := h.store.GetDkimKey(ctx, id)
		if errors.Is(err, ErrNotFound) {
			response.NotFound = append(response.NotFound, id)
			continue
		}
		if err != nil {
			return nil, err
		}
		// Wire carries the public half only; the private key is a Secret and no
		// wire object has a field for it.
		object, err := Project(key.Wire(), args.Properties)
		if err != nil {
			return nil, err
		}
		response.List = append(response.List, object)
	}
	return response, nil
}

func (h *Handler) dkimSet(ctx context.Context, args callArgs) (any, error) {
	response := &setResponseArgs{}
	// Signatures are generated by this server when a domain is created, so a
	// create or a patch is refused rather than half-honoured.
	for _, creationID := range sortedKeysOf(args.Create) {
		response.notCreated(creationID, &setErrorArgs{
			Type:        ErrTypeInvalidArguments,
			Description: "signing keys are generated by the server",
		})
	}
	for _, id := range sortedKeysOf(args.Update) {
		response.notUpdated(id, invalidPatch("a signing key cannot be patched"))
	}
	for _, id := range args.Destroy {
		if err := h.store.DeleteDkimKey(ctx, id); err != nil {
			if failure := storeSetError(err, "selector"); failure != nil {
				response.notDestroyed(id, failure)
				continue
			}
			return nil, err
		}
		response.destroyed(id)
	}
	return response, nil
}

// -----------------------------------------------------------------------------
// x:Account — mailboxes
// -----------------------------------------------------------------------------

func (h *Handler) accountQuery(ctx context.Context, args callArgs) (any, error) {
	fields, err := parseFilter(args.Filter, "@type", "domainId")
	if err != nil {
		return nil, err
	}
	accountType, hasType, err := filterString(fields, "@type")
	if err != nil {
		return nil, err
	}
	domainID, _, err := filterString(fields, "domainId")
	if err != nil {
		return nil, err
	}
	// Every account here is a user mailbox. A filter for another type matches
	// nothing, which is a supported filter with an empty answer rather than an
	// unsupported one.
	if hasType && !strings.EqualFold(accountType, "User") {
		return queryResult(nil, 0), nil
	}

	accounts, err := h.store.ListAccounts(ctx, domainID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	paged, position, err := page(ids, args)
	if err != nil {
		return nil, err
	}
	return queryResult(paged, position), nil
}

func (h *Handler) accountGet(ctx context.Context, args callArgs) (any, error) {
	response := newGetResponse()
	names := map[string]string{}
	for _, id := range args.IDs {
		account, err := h.store.GetAccount(ctx, id)
		if errors.Is(err, ErrNotFound) {
			response.NotFound = append(response.NotFound, id)
			continue
		}
		if err != nil {
			return nil, err
		}
		domainName, err := h.domainName(ctx, names, account.DomainID)
		if err != nil {
			return nil, err
		}
		// WireAccount has no field for a credential of any kind, so a list
		// response structurally cannot carry one.
		object, err := Project(account.Wire(domainName), args.Properties)
		if err != nil {
			return nil, err
		}
		response.List = append(response.List, object)
	}
	return response, nil
}

// quotasArgs is x:Account.quotas.
type quotasArgs struct {
	MaxDiskQuota uint64 `json:"maxDiskQuota"`
}

// aliasArgs is one entry of an aliases map. Note "enabled", not "isEnabled" —
// a domain and an alias spell it differently and both spellings are the
// client's.
type aliasArgs struct {
	Name        string  `json:"name"`
	DomainID    string  `json:"domainId"`
	Description *string `json:"description"`
	Enabled     *bool   `json:"enabled"`
}

// credentialArgs is one entry of x:Account.credentials. The plaintext arrives
// here once and is hashed immediately; it is never stored, never returned and
// never logged, and the type refuses to print itself so a formatted request
// cannot leak it either.
type credentialArgs struct {
	Type   string `json:"@type"`
	Secret string `json:"secret"`
}

func (credentialArgs) String() string { return "[redacted]" }

// GoString covers %#v.
func (credentialArgs) GoString() string { return "[redacted]" }

type accountCreateArgs struct {
	Type        string                    `json:"@type"`
	Name        string                    `json:"name"`
	DomainID    string                    `json:"domainId"`
	Description *string                   `json:"description"`
	Locale      string                    `json:"locale"`
	Quotas      *quotasArgs               `json:"quotas"`
	Aliases     map[string]aliasArgs      `json:"aliases"`
	Credentials map[string]credentialArgs `json:"credentials"`
}

func (h *Handler) accountSet(ctx context.Context, args callArgs) (any, error) {
	response := &setResponseArgs{}
	// domains caches the domain names the update loop resolves, so a batch that
	// patches ten mailboxes of one domain reads it once.
	domains := map[string]string{}

	for _, creationID := range sortedKeysOf(args.Create) {
		var input accountCreateArgs
		if err := json.Unmarshal(args.Create[creationID], &input); err != nil {
			response.notCreated(creationID, invalidProperties("the object was not of the expected shape"))
			continue
		}
		if input.Type != "" && !strings.EqualFold(input.Type, "User") {
			response.notCreated(creationID, invalidProperties("this server has only user accounts", "@type"))
			continue
		}
		if strings.Trim(input.Name, " ") == "" {
			response.notCreated(creationID, invalidProperties("a mailbox needs a local part", "name"))
			continue
		}
		// The domain is read before the name is checked because the name is
		// only half an address: what has to be valid is "name@domain", which is
		// the address this mailbox will receive at and the one that goes into
		// an envelope.
		domain, err := h.store.GetDomain(ctx, input.DomainID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				response.notCreated(creationID, invalidProperties("no such domain", "domainId"))
				continue
			}
			return nil, err
		}
		localPart, err := mailboxLocalPart(input.Name, domain.Name)
		if err != nil {
			response.notCreated(creationID, invalidProperties("that mailbox address is not usable: "+addressReason(err), "name"))
			continue
		}

		account := NewAccount(input.DomainID, localPart, h.now())
		if input.Description != nil {
			account.Description = *input.Description
		}
		account.Locale = input.Locale
		if input.Quotas != nil {
			account.QuotaBytes = input.Quotas.MaxDiskQuota
		}
		aliases, failure := parseAliases(input.Aliases, input.DomainID, domain.Name)
		if failure != nil {
			response.notCreated(creationID, failure)
			continue
		}
		account.Aliases = aliases

		// A mailbox nobody can authenticate into is not a mailbox, so the
		// credential is required rather than defaulted to none.
		secret, failure := credentialSecret(input.Credentials)
		if failure != nil {
			response.notCreated(creationID, failure)
			continue
		}
		if secret == "" {
			response.notCreated(creationID, invalidProperties("a mailbox needs a credential", "credentials"))
			continue
		}
		hash, err := HashPassword(secret)
		if err != nil {
			if errors.Is(err, ErrInvalid) {
				response.notCreated(creationID, invalidProperties("the credential was not usable", "credentials"))
				continue
			}
			return nil, errors.New("maild: derive a mailbox credential")
		}
		account.Password = hash

		stored, err := h.store.CreateAccount(ctx, account)
		if err != nil {
			// The address space is shared with mailing lists and with other
			// mailboxes' aliases, so a collision with any of them is the same
			// primaryKeyViolation the facade turns into "that address is
			// already taken".
			if failure := storeSetError(err, "email"); failure != nil {
				response.notCreated(creationID, failure)
				continue
			}
			return nil, err
		}
		response.created(creationID, stored.ID)
	}

	for _, id := range sortedKeysOf(args.Update) {
		var patch map[string]json.RawMessage
		if err := json.Unmarshal(args.Update[id], &patch); err != nil {
			response.notUpdated(id, invalidPatch("the patch was not an object"))
			continue
		}
		account, err := h.store.GetAccount(ctx, id)
		if errors.Is(err, ErrNotFound) {
			response.notUpdated(id, &setErrorArgs{Type: ErrTypeNotFound, Description: "no such mailbox"})
			continue
		}
		if err != nil {
			return nil, err
		}
		// An alias in the patch is a local part, and a local part is only half
		// an address, so the mailbox's domain name is resolved before the patch
		// is applied. A mailbox whose domain is gone is a broken invariant, not
		// a bad request.
		accountDomain, err := h.domainName(ctx, domains, account.DomainID)
		if err != nil {
			return nil, err
		}
		failure, err := applyAccountPatch(&account, accountDomain, patch)
		if err != nil {
			return nil, err
		}
		if failure != nil {
			response.notUpdated(id, failure)
			continue
		}
		if _, err := h.store.PutAccount(ctx, account); err != nil {
			if failure := storeSetError(err, "email"); failure != nil {
				response.notUpdated(id, failure)
				continue
			}
			return nil, err
		}
		response.updated(id)
	}

	for _, id := range args.Destroy {
		if err := h.store.DeleteAccount(ctx, id); err != nil {
			if failure := storeSetError(err, "email"); failure != nil {
				response.notDestroyed(id, failure)
				continue
			}
			return nil, err
		}
		// The row is gone, so the mail it authorised must go with it. The
		// purge follows the delete rather than preceding it: the row is the
		// authority, and destroying a live account's mail because the row
		// delete then failed is not recoverable. A purge that fails is logged
		// and the destroy still reported — the account really is gone, and
		// answering notDestroyed would tell the facade it still exists.
		if err := PurgeMailbox(MailboxSpool(h.store), id); err != nil {
			h.logger.Error("maild: a destroyed mailbox was not purged from disk",
				"account", id, "error", err.Error())
		}
		response.destroyed(id)
	}

	return response, nil
}

// applyAccountPatch applies the four keys the client ever sends. The error
// return is this server's fault; the *setErrorArgs return is the request's.
func applyAccountPatch(account *Account, domainName string, patch map[string]json.RawMessage) (*setErrorArgs, error) {
	for _, key := range sortedKeysOf(patch) {
		raw := patch[key]
		switch key {
		case "description":
			value, err := stringPatch(raw)
			if err != nil {
				return invalidPatch(err.Error(), key), nil
			}
			account.Description = value
		case "quotas":
			var value quotasArgs
			if err := json.Unmarshal(raw, &value); err != nil {
				return invalidPatch("quotas must be an object", key), nil
			}
			account.QuotaBytes = value.MaxDiskQuota
		case "aliases":
			// The map replaces the whole alias set rather than merging into it:
			// the client read-modify-writes the set it just read, so what it
			// sends is the complete desired state and an alias missing from it
			// is meant to stop resolving.
			var value map[string]aliasArgs
			if err := json.Unmarshal(raw, &value); err != nil {
				return invalidPatch("aliases must be an object", key), nil
			}
			aliases, failure := parseAliases(value, account.DomainID, domainName)
			if failure != nil {
				return failure, nil
			}
			account.Aliases = aliases
		case "credentials":
			var value map[string]credentialArgs
			if err := json.Unmarshal(raw, &value); err != nil {
				return invalidPatch("credentials must be an object", key), nil
			}
			secret, failure := credentialSecret(value)
			if failure != nil {
				return failure, nil
			}
			if secret == "" {
				return invalidProperties("a mailbox needs a credential", key), nil
			}
			hash, err := HashPassword(secret)
			if err != nil {
				if errors.Is(err, ErrInvalid) {
					return invalidProperties("the credential was not usable", key), nil
				}
				// Deliberately not wrapped: the cause is reported to the log by
				// the caller and must not carry the value that produced it.
				return nil, errors.New("maild: derive a mailbox credential")
			}
			account.Password = hash
		default:
			return invalidPatch("that property cannot be patched", key), nil
		}
	}
	return nil, nil
}

// credentialSecret reads the one password out of a credentials map. "" with no
// failure means the map was absent or empty.
func credentialSecret(credentials map[string]credentialArgs) (string, *setErrorArgs) {
	for _, key := range sortedKeysOf(credentials) {
		credential := credentials[key]
		if credential.Type != "" && !strings.EqualFold(credential.Type, "Password") {
			return "", invalidProperties("only password credentials are supported", "credentials")
		}
		if credential.Secret == "" {
			continue
		}
		return credential.Secret, nil
	}
	return "", nil
}

// parseAliases turns the wire map into the stored set. domainName is the name
// of the domain the aliases live in: an alias is an address this server accepts
// mail at, so it is validated as "name@domainName" rather than as a bare
// string.
func parseAliases(entries map[string]aliasArgs, domainID, domainName string) ([]Alias, *setErrorArgs) {
	if len(entries) == 0 {
		return nil, nil
	}
	aliases := make([]Alias, 0, len(entries))
	for _, key := range sortedKeysOf(entries) {
		entry := entries[key]
		if strings.Trim(entry.Name, " ") == "" {
			return nil, invalidProperties("an alias needs a local part", "aliases")
		}
		localPart, err := mailboxLocalPart(entry.Name, domainName)
		if err != nil {
			return nil, invalidProperties("that alias address is not usable: "+addressReason(err), "aliases")
		}
		if entry.DomainID != "" && entry.DomainID != domainID {
			return nil, invalidProperties("an alias must be in the mailbox's own domain", "aliases")
		}
		alias := Alias{LocalPart: localPart, DomainID: domainID, Enabled: true}
		if entry.Description != nil {
			alias.Description = *entry.Description
		}
		if entry.Enabled != nil {
			alias.Enabled = *entry.Enabled
		}
		aliases = append(aliases, alias)
	}
	return aliases, nil
}

// -----------------------------------------------------------------------------
// x:MailingList — forwarders with an external or several targets
// -----------------------------------------------------------------------------

func (h *Handler) listQuery(ctx context.Context, args callArgs) (any, error) {
	// This query is always unfiltered and paged. The client matches the domain
	// itself because the real server answers unsupportedFilter for a domainId
	// filter here; answering it would make this server's behaviour differ from
	// the one the client was written against.
	if _, err := parseFilter(args.Filter); err != nil {
		return nil, err
	}
	lists, err := h.store.ListLists(ctx, "")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(lists))
	for _, list := range lists {
		ids = append(ids, list.ID)
	}
	paged, position, err := page(ids, args)
	if err != nil {
		return nil, err
	}
	return queryResult(paged, position), nil
}

func (h *Handler) listGet(ctx context.Context, args callArgs) (any, error) {
	response := newGetResponse()
	names := map[string]string{}
	for _, id := range args.IDs {
		list, err := h.store.GetList(ctx, id)
		if errors.Is(err, ErrNotFound) {
			response.NotFound = append(response.NotFound, id)
			continue
		}
		if err != nil {
			return nil, err
		}
		domainName, err := h.domainName(ctx, names, list.DomainID)
		if err != nil {
			return nil, err
		}
		object, err := Project(list.Wire(domainName), args.Properties)
		if err != nil {
			return nil, err
		}
		response.List = append(response.List, object)
	}
	return response, nil
}

type listCreateArgs struct {
	Name        string               `json:"name"`
	DomainID    string               `json:"domainId"`
	Description *string              `json:"description"`
	Recipients  map[string]bool      `json:"recipients"`
	Aliases     map[string]aliasArgs `json:"aliases"`
}

func (h *Handler) listSet(ctx context.Context, args callArgs) (any, error) {
	response := &setResponseArgs{}

	for _, creationID := range sortedKeysOf(args.Create) {
		var input listCreateArgs
		if err := json.Unmarshal(args.Create[creationID], &input); err != nil {
			response.notCreated(creationID, invalidProperties("the object was not of the expected shape"))
			continue
		}
		if strings.Trim(input.Name, " ") == "" {
			response.notCreated(creationID, invalidProperties("a mailing list needs a local part", "name"))
			continue
		}
		if len(input.Aliases) > 0 {
			// Refused rather than dropped: an alias silently ignored would be
			// an address the console shows and no mail reaches.
			response.notCreated(creationID, invalidProperties("a mailing list cannot carry aliases", "aliases"))
			continue
		}
		recipients, failure := enabledRecipients(input.Recipients)
		if failure != nil {
			response.notCreated(creationID, failure)
			continue
		}
		if len(recipients) == 0 {
			// A list with no recipients accepts mail and drops it.
			response.notCreated(creationID, invalidProperties("a mailing list needs at least one recipient", "recipients"))
			continue
		}
		// The domain is read before the name is checked for the same reason as
		// in accountSet: what has to be valid is the list's own address,
		// "name@domain", which is what a forwarded envelope is addressed from.
		domain, err := h.store.GetDomain(ctx, input.DomainID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				response.notCreated(creationID, invalidProperties("no such domain", "domainId"))
				continue
			}
			return nil, err
		}
		localPart, err := mailboxLocalPart(input.Name, domain.Name)
		if err != nil {
			response.notCreated(creationID, invalidProperties("that mailing list address is not usable: "+addressReason(err), "name"))
			continue
		}

		list := NewList(input.DomainID, localPart, h.now())
		if input.Description != nil {
			list.Description = *input.Description
		}
		list.Recipients = recipients

		stored, err := h.store.CreateList(ctx, list)
		if err != nil {
			if failure := storeSetError(err, "email"); failure != nil {
				response.notCreated(creationID, failure)
				continue
			}
			return nil, err
		}
		response.created(creationID, stored.ID)
	}

	for _, id := range sortedKeysOf(args.Update) {
		var patch map[string]json.RawMessage
		if err := json.Unmarshal(args.Update[id], &patch); err != nil {
			response.notUpdated(id, invalidPatch("the patch was not an object"))
			continue
		}
		list, err := h.store.GetList(ctx, id)
		if errors.Is(err, ErrNotFound) {
			response.notUpdated(id, &setErrorArgs{Type: ErrTypeNotFound, Description: "no such mailing list"})
			continue
		}
		if err != nil {
			return nil, err
		}
		if failure := applyListPatch(&list, patch); failure != nil {
			response.notUpdated(id, failure)
			continue
		}
		if _, err := h.store.PutList(ctx, list); err != nil {
			if failure := storeSetError(err, "email"); failure != nil {
				response.notUpdated(id, failure)
				continue
			}
			return nil, err
		}
		response.updated(id)
	}

	for _, id := range args.Destroy {
		if err := h.store.DeleteList(ctx, id); err != nil {
			if failure := storeSetError(err, "email"); failure != nil {
				response.notDestroyed(id, failure)
				continue
			}
			return nil, err
		}
		response.destroyed(id)
	}

	return response, nil
}

func applyListPatch(list *List, patch map[string]json.RawMessage) *setErrorArgs {
	for _, key := range sortedKeysOf(patch) {
		raw := patch[key]
		switch key {
		case "description":
			value, err := stringPatch(raw)
			if err != nil {
				return invalidPatch(err.Error(), key)
			}
			list.Description = value
		case "recipients":
			var value map[string]bool
			if err := json.Unmarshal(raw, &value); err != nil {
				return invalidPatch("recipients must be an object", key)
			}
			recipients, failure := enabledRecipients(value)
			if failure != nil {
				return failure
			}
			if len(recipients) == 0 {
				return invalidPatch("a mailing list needs at least one recipient", key)
			}
			list.Recipients = recipients
		default:
			// name and domainId are the list's address, and the id derives from
			// it, so a rename is a destroy and a create rather than a patch.
			return invalidPatch("that property cannot be patched", key)
		}
	}
	return nil
}

// enabledRecipients validates the true entries of the recipients map, which is
// how both the client and the server express membership.
//
// This is the header-injection gate. A list recipient is an address this server
// writes into the envelope of every forwarded copy and into the headers it adds
// to it, so an unvalidated one puts whatever the caller wrote into delivered
// mail. It used to be lowercased and TrimSpace'd, which is worse than doing
// nothing: TrimSpace strips a trailing CR, quietly removing the one byte that
// would have shown the address was hostile, and an embedded CRLF went through
// untouched. Every recipient is now parsed, and one bad entry refuses the whole
// set rather than silently shortening it — a forwarder that dropped a recipient
// without saying so is mail going nowhere.
func enabledRecipients(recipients map[string]bool) ([]string, *setErrorArgs) {
	addresses := make([]string, 0, len(recipients))
	for _, address := range sortedKeysOf(recipients) {
		if !recipients[address] {
			continue
		}
		normalized, err := NormalizeAddress(address)
		if err != nil {
			return nil, invalidProperties("a recipient is not a usable address: "+addressReason(err), "recipients")
		}
		addresses = append(addresses, normalized)
	}
	sort.Strings(addresses)
	return addresses, nil
}

// -----------------------------------------------------------------------------
// x:QueuedMessage
// -----------------------------------------------------------------------------

func (h *Handler) queueQuery(ctx context.Context, args callArgs) (any, error) {
	// The client filters by zone itself, because the real server's returnPath
	// and to filters are substring matches and a forwarder fan-out hides the
	// customer's address in orcpt.
	if _, err := parseFilter(args.Filter); err != nil {
		return nil, err
	}
	messages, err := h.queue(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	paged, position, err := page(ids, args)
	if err != nil {
		return nil, err
	}
	return queryResult(paged, position), nil
}

func (h *Handler) queueGet(ctx context.Context, args callArgs) (any, error) {
	response := newGetResponse()
	if len(args.IDs) == 0 {
		return response, nil
	}
	messages, err := h.queue(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]QueuedMessage, len(messages))
	for _, message := range messages {
		byID[message.ID] = message
	}
	for _, id := range args.IDs {
		message, ok := byID[id]
		if !ok {
			response.NotFound = append(response.NotFound, id)
			continue
		}
		object, err := Project(message.Wire(), args.Properties)
		if err != nil {
			return nil, err
		}
		response.List = append(response.List, object)
	}
	return response, nil
}

// queue reads the outbound queue: the store's own by default, because that is
// where the delivery path spools.
//
// The page is bounded at MaxObjectsPerCall because the facade reads a full page
// as "attribution is incomplete" and shows "not available" rather than a count
// that is silently a lower bound.
func (h *Handler) queue(ctx context.Context) ([]QueuedMessage, error) {
	if h.listQueue != nil {
		return h.listQueue(ctx)
	}
	return h.store.ListQueuedMessages(ctx, MaxObjectsPerCall)
}

// -----------------------------------------------------------------------------
// Shared object helpers
// -----------------------------------------------------------------------------

func newGetResponse() getResponseArgs {
	return getResponseArgs{State: "n", List: []map[string]any{}, NotFound: []string{}}
}

// domainName resolves the domain of a stored object, caching within one call.
// A mailbox or list whose domain is gone is a broken invariant — the store
// refuses to delete a domain anything still references — so it is reported as
// this server's fault rather than papered over with a half-formed address.
func (h *Handler) domainName(ctx context.Context, cache map[string]string, domainID string) (string, error) {
	if name, ok := cache[domainID]; ok {
		return name, nil
	}
	domain, err := h.store.GetDomain(ctx, domainID)
	if err != nil {
		return "", fmt.Errorf("maild: read the domain %s of a stored object: %w", domainID, err)
	}
	cache[domainID] = domain.Name
	return domain.Name, nil
}
