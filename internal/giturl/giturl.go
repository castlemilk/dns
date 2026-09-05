// Package giturl validates repository URLs before they reach a builder.
//
// Copied verbatim from github.com/benebsworth/deephost internal/giturl
// (internal/giturl/giturl.go, commit 3ab3e16 plus the owner's working tree)
// because Go-internal packages are not importable across modules and the
// hosting facade must apply exactly the validation the DeepHost control plane
// applies to a repository URL. Do not diverge: if DeepHost changes this file,
// re-copy it. spec2 §11 (WP1) and hosting-facts.md §5 record the copy.
package giturl

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Validate permits GitHub repositories by default. In-cluster git:// sources
// are only allowed when explicitly enabled for local E2E use.
func Validate(raw string, allowInCluster bool) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("git repository URL is required")
	}
	if strings.HasPrefix(raw, "git@") {
		if strings.HasPrefix(raw, "git@github.com:") && validRepoPath(strings.TrimPrefix(raw, "git@github.com:")) {
			return nil
		}
		return fmt.Errorf("SSH repositories must use git@github.com:owner/name.git")
	}

	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" {
		return fmt.Errorf("invalid git repository URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		if strings.EqualFold(u.Hostname(), "github.com") && u.Port() == "" && validRepoPath(u.Path) {
			return nil
		}
		return fmt.Errorf("HTTPS repositories must be hosted on github.com")
	case "git":
		if !allowInCluster {
			return fmt.Errorf("git:// repositories are disabled")
		}
		if u.Port() != "9418" || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".svc") || !validInClusterPath(u.Path) {
			return fmt.Errorf("in-cluster git repositories must use a .svc host on port 9418")
		}
		return nil
	default:
		return fmt.Errorf("git scheme %q is not allowed", u.Scheme)
	}
}

// ValidateSubpath prevents a monorepo path from escaping the checkout.
func ValidateSubpath(value string) error {
	if value == "" {
		return nil
	}
	if strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") {
		return fmt.Errorf("repository path must use relative slash-separated components")
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(value, "/") {
		return fmt.Errorf("repository path escapes the checkout")
	}
	return nil
}

// NeedsSSH reports whether the validated repository uses the GitHub SSH form.
func NeedsSSH(raw string) bool { return strings.HasPrefix(strings.TrimSpace(raw), "git@github.com:") }

func validRepoPath(raw string) bool {
	value := strings.Trim(strings.TrimSuffix(raw, ".git"), "/")
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	return validPart(parts[0]) && validPart(parts[1])
}

func validInClusterPath(raw string) bool {
	value := strings.Trim(strings.TrimSuffix(raw, ".git"), "/")
	if value == "" || path.Clean(value) != value || strings.HasPrefix(value, "../") || strings.HasPrefix(value, "/") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if !validPart(part) {
			return false
		}
	}
	return true
}

func validPart(value string) bool {
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return value != "." && value != ".."
}
