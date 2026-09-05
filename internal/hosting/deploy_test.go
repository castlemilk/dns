package hosting

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/internal/platform"
)

func TestValidateGitSourceRejectsUnsafeRepositories(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		repository string
	}{
		{"userinfo", "https://x-access-token:ghp_secret@github.com/owner/name"},
		{"query", "https://github.com/owner/name?token=secret"},
		{"fragment", "https://github.com/owner/name#frag"},
		{"port", "https://github.com:8443/owner/name"},
		{"http", "http://github.com/owner/name"},
		{"other host", "https://gitlab.com/owner/name"},
		{"ssh", "git@github.com:owner/name.git"},
		{"ssh scheme", "ssh://git@github.com/owner/name"},
		{"too many parts", "https://github.com/owner/name/extra"},
		{"missing name", "https://github.com/owner"},
		{"traversal", "https://github.com/owner/../name"},
		{"empty", "   "},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateGitSource(test.repository, "main", "", "")
			if err == nil {
				t.Fatalf("validateGitSource(%q) accepted an unsafe repository", test.repository)
			}
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("code = %s, want invalid_argument", connect.CodeOf(err))
			}
			if strings.Contains(err.Error(), "ghp_secret") || strings.Contains(err.Error(), "token=secret") {
				t.Errorf("the error echoed the credential: %v", err)
			}
		})
	}
}

func TestValidateGitSourceStoresTheCanonicalURL(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"https://github.com/Acme/Site",
		"https://github.com/Acme/Site.git",
		"  https://github.com/Acme/Site  ",
	} {
		source, err := validateGitSource(raw, "main", "apps/web", "ghp_"+strings.Repeat("x", 30))
		if err != nil {
			t.Fatalf("validateGitSource(%q): %v", raw, err)
		}
		if source.Repository != "https://github.com/Acme/Site" {
			t.Errorf("canonical repository = %q", source.Repository)
		}
		if source.Path != "apps/web" || source.Revision != "main" {
			t.Errorf("source = %+v", source)
		}
	}
}

func TestValidateGitSourceBoundsTheRevisionAndToken(t *testing.T) {
	t.Parallel()

	repository := "https://github.com/owner/name"
	cases := []struct {
		name     string
		revision string
		path     string
		token    string
	}{
		{"revision traversal", "../etc", "", ""},
		{"revision characters", "main;rm -rf /", "", ""},
		{"revision too long", strings.Repeat("a", maxRevisionLength+1), "", ""},
		{"path escapes", "main", "../outside", ""},
		{"absolute path", "main", "/etc", ""},
		{"token too long", "main", "", strings.Repeat("x", maxGitTokenBytes+1)},
		{"token with a space", "main", "", "ghp with space"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateGitSource(repository, test.revision, test.path, test.token); err == nil {
				t.Fatal("validateGitSource accepted an unsafe input")
			}
		})
	}
}

func TestSanitizeUploadNameStripsControlAndBidiCharacters(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"site":                   "site",
		"si\u202ete":             "site",
		"si\x00te":               "site",
		"  spaced  ":             "spaced",
		strings.Repeat("n", 200): strings.Repeat("n", 80),
	}
	for input, want := range cases {
		if got := SanitizeUploadName(input); got != want {
			t.Errorf("SanitizeUploadName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDeployTimesReportEngineOnlyWhenBothEndsCameFromTheEngine(t *testing.T) {
	t.Parallel()

	engineStart := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	engineFinish := engineStart.Add(38 * time.Second)
	observedStart := engineStart.Add(-2 * time.Second)
	observedFinish := engineFinish.Add(2 * time.Second)

	cases := []struct {
		name string
		doc  platform.DeployDoc
		want hostingv1.TimeSource
	}{
		{
			name: "both from the engine",
			doc:  platform.DeployDoc{EngineStartedAt: &engineStart, EngineFinishedAt: &engineFinish},
			want: hostingv1.TimeSource_TIME_SOURCE_ENGINE,
		},
		{
			name: "engine start, observed finish",
			doc:  platform.DeployDoc{EngineStartedAt: &engineStart, ObservedTerminalAt: &observedFinish},
			want: hostingv1.TimeSource_TIME_SOURCE_OBSERVED,
		},
		{
			name: "both observed",
			doc:  platform.DeployDoc{ObservedBuildingAt: &observedStart, ObservedTerminalAt: &observedFinish},
			want: hostingv1.TimeSource_TIME_SOURCE_OBSERVED,
		},
		{
			name: "nothing known",
			doc:  platform.DeployDoc{},
			want: hostingv1.TimeSource_TIME_SOURCE_UNSPECIFIED,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, _, source := deployTimes(test.doc)
			if source != test.want {
				t.Errorf("time source = %v, want %v", source, test.want)
			}
		})
	}
}

func TestDeploySourceProtoSeparatesGitAndUploadFacts(t *testing.T) {
	t.Parallel()

	git := platform.DeployDoc{
		Kind: DeployKindGit,
		Source: platform.DeploySourceDoc{
			Repository:        "https://github.com/owner/name",
			PrivateRepository: true,
		},
	}
	rendered := deploySourceProto(git)
	if !rendered.GetPrivateRepository() {
		t.Error("private_repository = false, want true for a deploy that supplied a token")
	}
	if rendered.GetUploadName() != "" {
		t.Errorf("upload_name = %q, want empty for a git deploy", rendered.GetUploadName())
	}

	upload := platform.DeployDoc{
		Kind:   DeployKindUpload,
		Source: platform.DeploySourceDoc{UploadName: "site", UploadFiles: 3, UploadBytes: 99},
	}
	rendered = deploySourceProto(upload)
	if rendered.GetUploadName() != "site" || rendered.GetUploadFiles() != 3 || rendered.GetUploadBytes() != 99 {
		t.Errorf("upload source = %+v", rendered)
	}
	if rendered.GetPrivateRepository() {
		t.Error("private_repository = true for an upload deploy")
	}
}
