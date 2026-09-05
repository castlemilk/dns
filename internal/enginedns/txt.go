package enginedns

import (
	"strconv"
	"strings"
)

// TxtText decodes a stored TXT value into the text a resolver would see: the
// concatenation of its character-strings, unquoted and unescaped.
//
// It exists because the same DKIM key can be stored as one 428-character
// string or as the parenthesised two-string form a zone-file import produces.
// Both put identical bytes on the wire, so the planner must treat them as
// equal and adopt the existing record instead of adding a duplicate.
//
// A value that is not in character-string form is returned trimmed and
// unchanged, so callers may pass unquoted text.
func TxtText(value string) string {
	rest := strings.TrimSpace(value)
	if !strings.HasPrefix(rest, `"`) {
		return rest
	}
	var text strings.Builder
	for rest != "" {
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" {
			break
		}
		if rest[0] != '"' {
			// Malformed tail: keep it verbatim rather than silently dropping
			// bytes that would change the comparison result.
			text.WriteString(rest)
			break
		}
		end := 1
		for end < len(rest) {
			if rest[end] == '\\' {
				end += 2
				continue
			}
			if rest[end] == '"' {
				break
			}
			end++
		}
		if end >= len(rest) {
			text.WriteString(rest[1:])
			break
		}
		segment, err := strconv.Unquote(rest[:end+1])
		if err != nil {
			segment = rest[1:end]
		}
		text.WriteString(segment)
		rest = rest[end+1:]
	}
	return text.String()
}

// TxtEqual reports whether two stored TXT values carry the same wire text.
func TxtEqual(left, right string) bool {
	return TxtText(left) == TxtText(right)
}
