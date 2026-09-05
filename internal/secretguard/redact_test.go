package secretguard_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/secretguard"
)

func TestRedact(t *testing.T) {
	t.Parallel()

	const stalwartKey = "API_" + "abcdefghijklmnopqrstuvwxyz0123456789AB"
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "no secret", input: "deploy acme.dev @ main", want: "deploy acme.dev @ main"},
		{name: "stalwart api key", input: "auth " + stalwartKey + " ok", want: "auth [redacted] ok"},
		{name: "stripe webhook secret", input: "whsec_ZZZzzz123", want: "[redacted]"},
		{name: "stripe live key", input: "sk_live_51aBcD", want: "[redacted]"},
		{name: "stripe test key", input: "sk_test_51aBcD", want: "[redacted]"},
		{name: "stripe restricted key", input: "rk_live_51aBcD", want: "[redacted]"},
		{
			name:  "github classic token",
			input: "token ghp_0123456789abcdefghijABCDEF",
			want:  "token [redacted]",
		},
		{name: "github oauth token", input: "gho_0123456789abcdefghijABCDEF", want: "[redacted]"},
		{name: "github user token", input: "ghu_0123456789abcdefghijABCDEF", want: "[redacted]"},
		{name: "github server token", input: "ghs_0123456789abcdefghijABCDEF", want: "[redacted]"},
		{name: "github refresh token", input: "ghr_0123456789abcdefghijABCDEF", want: "[redacted]"},
		{
			name:  "github fine grained token",
			input: "github_pat_11ABCDEFG0abcdefghij_more",
			want:  "[redacted]",
		},
		{
			name:  "embedded url credentials",
			input: "clone https://x-access-token:ghp_0123456789abcdefghijABCDEF@github.com/o/r",
			want:  "clone https://[redacted]@github.com/o/r",
		},
		{
			name:  "embedded url credentials without a token shape",
			input: "postgres://admin:hunter2@db.internal:5432/app",
			want:  "postgres://[redacted]@db.internal:5432/app",
		},
		{
			name:  "short github prefix is not a token",
			input: "ghp_short",
			want:  "ghp_short",
		},
		{
			name:  "plain mailbox address is untouched",
			input: "mara@acme.dev",
			want:  "mara@acme.dev",
		},
		{
			name:  "two secrets in one string",
			input: "a whsec_one b sk_test_two c",
			want:  "a [redacted] b [redacted] c",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := secretguard.Redact(test.input); got != test.want {
				t.Errorf("Redact(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestChanged(t *testing.T) {
	t.Parallel()

	if secretguard.Changed("nothing to see") {
		t.Error("Changed reported a rewrite for clean text")
	}
	if !secretguard.Changed("whsec_abc") {
		t.Error("Changed did not report a rewrite for a secret")
	}
}

func TestHandlerRedactsMessagesAndAttributes(t *testing.T) {
	t.Parallel()

	buffer := &bytes.Buffer{}
	logger := secretguard.NewLogger(slog.New(slog.NewJSONHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	logger = logger.With("static", "whsec_staticsecret")
	logger.Info(
		"cloning https://user:ghp_0123456789abcdefghijABCDEF@github.com/o/r",
		"token", "sk_live_abcdef",
		"error", errors.New("dial failed for API_"+strings.Repeat("x", 38)),
		"group", slog.GroupValue(slog.String("nested", "gho_0123456789abcdefghijABCDEF")),
		"count", 3,
	)

	raw := buffer.String()
	for _, forbidden := range []string{"ghp_", "sk_live_", "gho_", "whsec_", "API_x"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("log output still contains %q: %s", forbidden, raw)
		}
	}

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buffer.Bytes()), &entry); err != nil {
		t.Fatalf("decode log line: %v", err)
	}
	if entry["msg"] != "cloning https://[redacted]@github.com/o/r" {
		t.Errorf("msg = %v", entry["msg"])
	}
	if entry["token"] != "[redacted]" {
		t.Errorf("token = %v", entry["token"])
	}
	if entry["static"] != "[redacted]" {
		t.Errorf("static = %v", entry["static"])
	}
	if entry["count"] != float64(3) {
		t.Errorf("count = %v, want the non-string value to survive", entry["count"])
	}
	group, ok := entry["group"].(map[string]any)
	if !ok {
		t.Fatalf("group = %#v, want an object", entry["group"])
	}
	if group["nested"] != "[redacted]" {
		t.Errorf("group.nested = %v", group["nested"])
	}
}

func TestHandlerWithGroup(t *testing.T) {
	t.Parallel()

	buffer := &bytes.Buffer{}
	handler := secretguard.NewHandler(slog.NewJSONHandler(buffer, nil)).WithGroup("engine")
	logger := slog.New(handler)
	logger.Info("start", "key", "whsec_abcdef")

	if !handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled(info) = false")
	}
	if strings.Contains(buffer.String(), "whsec_") {
		t.Errorf("grouped attribute leaked: %s", buffer.String())
	}
	if !strings.Contains(buffer.String(), `"engine"`) {
		t.Errorf("group name missing: %s", buffer.String())
	}
}
