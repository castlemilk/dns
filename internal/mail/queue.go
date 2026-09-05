package mail

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/internal/mail/stalwart"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// maxQueueRows bounds what the queue card renders.
const maxQueueRows = 100

// GetMailQueue reports what is in flight for one zone right now.
//
// This is the only delivery statistic this facade produces. Community Stalwart
// has no per-domain history — x:Metric/query answers forbidden and the
// Prometheus counters carry no domain label — so a "last 24 hours delivered /
// bounced" figure could only be invented. The queue is real, so the queue is
// what is shown.
func (s *Service) GetMailQueue(
	ctx context.Context,
	request *connect.Request[mailv1.GetMailQueueRequest],
) (*connect.Response[mailv1.GetMailQueueResponse], error) {
	if err := s.requireEngine(); err != nil {
		return nil, err
	}
	doc, err := s.mailDomain(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	messages, complete, queryErr := s.queryQueue(ctx, doc.ZoneName)
	summary := s.summarize(messages, complete, queryErr)
	response := &mailv1.GetMailQueueResponse{Summary: summary}
	if !summary.GetAvailable() {
		return connect.NewResponse(response), nil
	}
	for _, message := range messages {
		response.Messages = append(response.Messages, queuedMessageProto(message, doc.ZoneName))
		if len(response.Messages) >= maxQueueRows {
			break
		}
	}
	return connect.NewResponse(response), nil
}

// queueSummary is the compact form the domain view carries.
func (s *Service) queueSummary(ctx context.Context, zoneName string) *mailv1.QueueSummary {
	if s.engine == nil {
		return &mailv1.QueueSummary{ObservedAt: timestamppb.New(s.deps.Now())}
	}
	messages, complete, err := s.queryQueue(ctx, zoneName)
	return s.summarize(messages, complete, err)
}

func (s *Service) queryQueue(ctx context.Context, zoneName string) ([]stalwart.QueuedMessage, bool, error) {
	started := time.Now()
	messages, complete, err := s.engine.QueryQueue(ctx, zoneName, maxQueueRows)
	outcome, class := engineOperationOutcome(err)
	s.deps.Meter().EngineRequest(ctx, engineName, "query_queue", outcome, class, time.Since(started))
	return messages, complete, err
}

// summarize counts the queue by the state of the recipient this zone owns. An
// incomplete page or a failed read yields available=false: a count that is
// silently a lower bound would be worse than saying nothing.
func (s *Service) summarize(messages []stalwart.QueuedMessage, complete bool, err error) *mailv1.QueueSummary {
	summary := &mailv1.QueueSummary{ObservedAt: timestamppb.New(s.deps.Now())}
	if err != nil || !complete {
		return summary
	}
	summary.Available = true
	for _, message := range messages {
		switch representative(message, "").Status {
		case stalwart.QueueStatusScheduled:
			summary.Scheduled++
		case stalwart.QueueStatusTemporaryFailure:
			summary.Failing++
		case stalwart.QueueStatusPermanentFailure:
			summary.Failed++
		}
	}
	return summary
}

// queuedMessageProto renders one queued message.
//
// A forwarder fan-out lists the EXTERNAL target as the recipient and carries the
// customer's own address only in orcpt, so the recipient list shows the zone
// address wherever one exists — that is the address the operator recognises.
func queuedMessageProto(message stalwart.QueuedMessage, zoneName string) *mailv1.QueuedMessage {
	chosen := representative(message, zoneName)
	result := &mailv1.QueuedMessage{
		Id:       message.ID,
		From:     senderFor(message.ReturnPath, zoneName),
		Status:   chosen.Status,
		Queue:    chosen.QueueName,
		Attempts: chosen.RetryCount,
	}
	for _, recipient := range message.Recipients {
		result.To = append(result.To, displayRecipient(recipient, zoneName))
	}
	due := chosen.RetryDue
	if due.IsZero() {
		due = message.NextRetry
	}
	if !due.IsZero() {
		result.DueAt = timestamppb.New(due.UTC())
	}
	return result
}

// representative is the recipient whose state this zone's row reports: the
// first one that belongs to the zone, else the first one at all.
func representative(message stalwart.QueuedMessage, zoneName string) stalwart.QueuedRecipient {
	if zoneName != "" {
		for _, recipient := range message.Recipients {
			if belongsTo(recipient.Address, zoneName) || belongsTo(recipient.ORCPT, zoneName) {
				return recipient
			}
		}
	}
	if len(message.Recipients) > 0 {
		return message.Recipients[0]
	}
	return stalwart.QueuedRecipient{}
}

// displayRecipient prefers the zone's own address when the fan-out recorded one.
func displayRecipient(recipient stalwart.QueuedRecipient, zoneName string) string {
	if recipient.ORCPT != "" && belongsTo(recipient.ORCPT, zoneName) {
		return recipient.ORCPT
	}
	return recipient.Address
}

// senderFor shows the full return path when it belongs to this zone and only
// its domain otherwise: an inbound message's sender is a third party's address.
func senderFor(returnPath, zoneName string) string {
	if returnPath == "" {
		return ""
	}
	if belongsTo(returnPath, zoneName) {
		return returnPath
	}
	_, domain, found := strings.Cut(returnPath, "@")
	if !found {
		return ""
	}
	return domain
}

func belongsTo(address, zoneName string) bool {
	if address == "" || zoneName == "" {
		return false
	}
	_, domain, found := strings.Cut(strings.ToLower(address), "@")
	return found && domain == strings.ToLower(zoneName)
}
