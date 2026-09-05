package activity

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	activityv1 "github.com/castlemilk/dns/gen/go/activity/v1"
	"github.com/castlemilk/dns/gen/go/activity/v1/activityv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Paging bounds for ListEvents.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
	// MaxListKinds bounds the filter so one request cannot ask the store to
	// evaluate an unbounded predicate list.
	MaxListKinds = 32
)

// ErrEventNotFound is what a store returns for an unknown id. It is declared
// here so a store implementation does not have to import a heavier package.
var ErrEventNotFound = errors.New("event not found")

func (l *Log) ListEvents(ctx context.Context, request *connect.Request[activityv1.ListEventsRequest]) (*connect.Response[activityv1.ListEventsResponse], error) {
	if l.store == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the activity log is not available on this role"))
	}
	message := request.Msg
	kinds := message.GetKinds()
	if len(kinds) > MaxListKinds {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("kinds: at most 32 filters"))
	}
	for _, kind := range kinds {
		if len(kind) > 64 || strings.ContainsAny(kind, " \t\n") {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("kinds: use dotted kind names or a prefix ending in a dot"))
		}
	}
	filter := ListFilter{
		ZoneID: message.GetZoneId(),
		Kinds:  kinds,
		Limit:  clampLimit(int(message.GetLimit())),
		Cursor: message.GetCursor(),
	}
	if since := message.GetSince(); since.IsValid() {
		filter.Since = since.AsTime()
	}

	docs, next, err := l.store.ListEvents(ctx, filter)
	if err != nil {
		return nil, l.storeError(ctx, "list activity events", err)
	}
	total, err := l.store.CountEvents(ctx)
	if err != nil {
		// A missing total is not worth failing the page for; the UI shows the
		// events it did get rather than an error.
		l.logger.Warn("count activity events", "error", err)
	}

	response := &activityv1.ListEventsResponse{
		Events:        make([]*activityv1.Event, 0, len(docs)),
		NextCursor:    next,
		TotalRetained: total,
	}
	for _, doc := range docs {
		response.Events = append(response.Events, EventProto(doc))
	}
	return connect.NewResponse(response), nil
}

func (l *Log) GetEvent(ctx context.Context, request *connect.Request[activityv1.GetEventRequest]) (*connect.Response[activityv1.GetEventResponse], error) {
	if l.store == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the activity log is not available on this role"))
	}
	id := request.Msg.GetId()
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}
	doc, err := l.store.GetEvent(ctx, id)
	if err != nil {
		return nil, l.storeError(ctx, "get activity event", err)
	}
	return connect.NewResponse(&activityv1.GetEventResponse{Event: EventProto(doc)}), nil
}

func (l *Log) storeError(ctx context.Context, operation string, err error) error {
	switch {
	case errors.Is(err, ErrEventNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("event not found"))
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)
	default:
		l.logger.Error(operation, "error", err)
		return connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
}

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultListLimit
	case limit > MaxListLimit:
		return MaxListLimit
	default:
		return limit
	}
}

// EventProto converts a stored event onto the wire.
func EventProto(doc EventDoc) *activityv1.Event {
	event := &activityv1.Event{
		Id:            doc.ID,
		ZoneId:        doc.ZoneID,
		ZoneName:      doc.ZoneName,
		Actor:         doc.Actor,
		Kind:          doc.Kind,
		Severity:      severityToProto(Severity(doc.Severity)),
		Summary:       doc.Summary,
		CorrelationId: doc.CorrelationID,
	}
	if !doc.Time.IsZero() {
		event.Time = timestamppb.New(doc.Time.UTC())
	}
	if len(doc.Details) > 0 {
		event.Details = make(map[string]string, len(doc.Details))
		for key, value := range doc.Details {
			event.Details[key] = value
		}
	}
	return event
}

func severityToProto(severity Severity) activityv1.Severity {
	switch severity {
	case SeverityWarn:
		return activityv1.Severity_SEVERITY_WARN
	case SeverityError:
		return activityv1.Severity_SEVERITY_ERROR
	case SeverityInfo:
		return activityv1.Severity_SEVERITY_INFO
	default:
		return activityv1.Severity_SEVERITY_UNSPECIFIED
	}
}

var _ activityv1connect.ActivityServiceHandler = (*Log)(nil)
