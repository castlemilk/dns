package platform

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// planDoc is the unauthenticated capability summary the landing page reads. It
// carries no host, id, address or secret — only what an anonymous visitor may
// truthfully be told about this deployment's capabilities and price.
type planDoc struct {
	HostingConfigured  bool      `json:"hosting_configured"`
	UploadsEnabled     bool      `json:"uploads_enabled"`
	MailConfigured     bool      `json:"mail_configured"`
	MailboxesPerDomain uint32    `json:"mailboxes_per_domain"`
	BillingConfigured  bool      `json:"billing_configured"`
	PriceLabel         string    `json:"price_label"`
	PolicyNote         string    `json:"policy_note"`
	GeneratedAt        time.Time `json:"generated_at"`
}

type planHandler struct {
	service *StatusService
	logger  *slog.Logger
}

// PlanHandler serves GET /public/v1/plan. It answers from memory — config plus
// the prober's cache — and never calls an engine, so an anonymous request can
// neither stall on an engine nor be used to probe one.
func (s *StatusService) PlanHandler() http.Handler {
	return &planHandler{service: s, logger: s.deps.Log()}
}

func (h *planHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	service := h.service
	doc := planDoc{
		HostingConfigured: service.cfg.Hosting.Configured(),
		UploadsEnabled:    service.cfg.Hosting.Configured() && service.cfg.Hosting.UploadsEnabled,
		MailConfigured:    service.cfg.Mail.Configured(),
		BillingConfigured: service.cfg.Billing.Configured(),
		PolicyNote:        PolicyNote,
		GeneratedAt:       service.deps.Now(),
	}
	if doc.MailConfigured {
		doc.MailboxesPerDomain = uint32(max(service.cfg.Mail.MailboxesPerDomain, 0))
	}
	if doc.BillingConfigured {
		// "" when the price probe has not succeeded: an unconfirmed price is
		// better left blank than guessed at on a public page.
		doc.PriceLabel = service.PriceLabel()
	}

	payload, err := json.Marshal(doc)
	if err != nil {
		h.logger.Error("encode public plan", "error", err)
		http.Error(writer, "internal server error", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "public, max-age=300")
	if request.Method == http.MethodHead {
		writer.WriteHeader(http.StatusOK)
		return
	}
	if _, err := writer.Write(payload); err != nil {
		h.logger.Debug("write public plan", "error", err)
	}
}
