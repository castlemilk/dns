// Package secretguard removes credential-shaped substrings from text that is
// about to be logged, persisted or returned. It is a leaf package: it imports
// nothing from this module so every other package may depend on it.
package secretguard

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
)

// Placeholder replaces every credential-shaped match.
const Placeholder = "[redacted]"

var (
	// tokenPattern is deliberately unanchored: a credential pasted in the
	// middle of a sentence, a URL or a JSON blob must still be caught.
	tokenPattern = regexp.MustCompile(
		`API_[A-Za-z0-9]{20,}` +
			`|whsec_[A-Za-z0-9]+` +
			`|sk_(?:live|test)_[A-Za-z0-9]+` +
			`|rk_(?:live|test)_[A-Za-z0-9]+` +
			`|ghp_[A-Za-z0-9]{20,}` +
			`|gho_[A-Za-z0-9]{20,}` +
			`|ghu_[A-Za-z0-9]{20,}` +
			`|ghs_[A-Za-z0-9]{20,}` +
			`|ghr_[A-Za-z0-9]{20,}` +
			`|github_pat_[A-Za-z0-9_]{20,}`)

	// userinfoPattern catches credentials embedded in a URL authority, which
	// is how a private git remote leaks a token.
	userinfoPattern = regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^/@\s]+@`)
)

// Redact returns value with every credential-shaped substring replaced by
// Placeholder. It never returns an error and never panics on arbitrary input.
func Redact(value string) string {
	if value == "" {
		return value
	}
	redacted := tokenPattern.ReplaceAllString(value, Placeholder)
	return userinfoPattern.ReplaceAllString(redacted, "${1}"+Placeholder+"@")
}

// Changed reports whether Redact would rewrite value.
func Changed(value string) bool {
	return Redact(value) != value
}

// Handler wraps an slog.Handler and applies Redact to the message and to every
// string-shaped attribute value, including values nested in groups.
type Handler struct {
	inner slog.Handler
}

// NewHandler wraps inner. A nil inner yields a nil handler, which slog treats
// as a programming error, so callers pass a real handler.
func NewHandler(inner slog.Handler) *Handler {
	return &Handler{inner: inner}
}

// NewLogger returns a logger whose records pass through Redact.
func NewLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return slog.New(NewHandler(logger.Handler()))
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	cleaned := slog.NewRecord(record.Time, record.Level, Redact(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		cleaned.AddAttrs(redactAttr(attr))
		return true
	})
	if err := h.inner.Handle(ctx, cleaned); err != nil {
		return fmt.Errorf("redacting log handler: %w", err)
	}
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		redacted = append(redacted, redactAttr(attr))
	}
	return &Handler{inner: h.inner.WithAttrs(redacted)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name)}
}

func redactAttr(attr slog.Attr) slog.Attr {
	attr.Key = Redact(attr.Key)
	attr.Value = redactValue(attr.Value)
	return attr
}

func redactValue(value slog.Value) slog.Value {
	switch value.Kind() {
	case slog.KindString:
		return slog.StringValue(Redact(value.String()))
	case slog.KindGroup:
		attrs := value.Group()
		redacted := make([]slog.Attr, 0, len(attrs))
		for _, attr := range attrs {
			redacted = append(redacted, redactAttr(attr))
		}
		return slog.GroupValue(redacted...)
	case slog.KindLogValuer:
		return redactValue(value.Resolve())
	case slog.KindAny:
		text := anyText(value.Any())
		if text != "" && Changed(text) {
			return slog.StringValue(Redact(text))
		}
		return value
	default:
		return value
	}
}

func anyText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case error:
		return typed.Error()
	case fmt.Stringer:
		return typed.String()
	case []byte:
		return string(typed)
	default:
		return ""
	}
}

var _ slog.Handler = (*Handler)(nil)
