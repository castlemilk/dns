package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
)

// stringList is a flag that may be repeated: --target a --target b.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("must not be empty")
	}
	*l = append(*l, value)
	return nil
}

var _ flag.Value = (*stringList)(nil)

// provided reports whether a flag was actually given, which is how the CLI
// tells "leave it as it is" from "set it to the zero value" for the optional
// proto fields.
func provided(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// parseRecordType accepts the type an operator types, in any case.
func parseRecordType(raw string) (dnsv1.RecordType, error) {
	name := "RECORD_TYPE_" + strings.ToUpper(strings.TrimSpace(raw))
	value, ok := dnsv1.RecordType_value[name]
	if !ok || value == 0 {
		return 0, usagef("--type: must be one of A, AAAA, CNAME, MX, TXT, NS, SRV, CAA, SOA")
	}
	return dnsv1.RecordType(value), nil
}

func recordTypeLabel(recordType dnsv1.RecordType) string {
	return strings.ToUpper(label(recordType.String(), "RECORD_TYPE_"))
}

func parseFramework(raw string) (hostingv1.Framework, error) {
	name := "FRAMEWORK_" + strings.ToUpper(strings.TrimSpace(raw))
	value, ok := hostingv1.Framework_value[name]
	if !ok || value == 0 {
		return 0, usagef("--framework: must be one of static, node, nextjs")
	}
	return hostingv1.Framework(value), nil
}

func parseWwwMode(raw string) (hostingv1.WwwMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return hostingv1.WwwMode_WWW_MODE_UNSPECIFIED, nil
	case "serve":
		return hostingv1.WwwMode_WWW_MODE_SERVE, nil
	case "redirect":
		return hostingv1.WwwMode_WWW_MODE_REDIRECT, nil
	default:
		return 0, usagef("--www-mode: must be serve or redirect")
	}
}

// parseSince accepts an RFC 3339 instant or a duration back from now, so
// `--since 2h` and `--since 2026-09-04T00:00:00Z` both work. Days are accepted
// because an event log is read in days more often than in hours.
func parseSince(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if instant, err := time.Parse(time.RFC3339, raw); err == nil {
		return instant, nil
	}
	value := raw
	if days, found := strings.CutSuffix(value, "d"); found {
		value = days + "h"
		if hours, err := time.ParseDuration(value); err == nil {
			return now.Add(-hours * 24), nil
		}
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return time.Time{}, usagef("--since: must be an RFC 3339 time or a duration such as 2h or 7d")
	}
	return now.Add(-duration), nil
}

// readSecretValue reads a write-only value: an environment variable's value or
// a git token. It comes from a file or from stdin, never from a flag, so it is
// not visible in the process list of a shared machine.
func readSecretValue(e *env, field, path string) (string, error) {
	const maxSecretBytes = 1 << 20
	reader := e.stdin
	if strings.TrimSpace(path) != "" && path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", field, err)
		}
		defer func() { ignore(file.Close()) }()
		reader = file
	}
	if reader == nil {
		return "", usagef("%s: no value was supplied on stdin", field)
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	if len(raw) > maxSecretBytes {
		return "", usagef("%s: must be at most %d bytes", field, maxSecretBytes)
	}
	// A trailing newline is what a shell heredoc or `echo` adds; nothing else
	// is trimmed, because a value's leading and trailing spaces may matter.
	value := strings.TrimRight(string(raw), "\r\n")
	if value == "" {
		return "", usagef("%s: is required", field)
	}
	return value, nil
}

// ignore drops an error deliberately, on a path that has nothing better to do
// with it: closing a file that was only read, or a browser the operator was
// already given the URL for. It exists because this repository's errcheck
// rejects the `_ = f()` form, and a named call makes each such decision
// reviewable. It mirrors internal/deephostcli/auth's own.
func ignore(error) {}
