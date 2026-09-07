package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/deephostcli/config"
)

// environment is a getenv over a fixed map, so a test never reads the
// developer's own settings or credential.
func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestRunUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		arguments []string
		want      int
		wantOut   string
	}{
		{name: "version", arguments: []string{"--version"}, want: exitOK, wantOut: "deephost-mcp "},
		{name: "help", arguments: []string{"--help"}, want: exitOK},
		{name: "unknown flag", arguments: []string{"--token", "secret"}, want: exitUsage},
		{name: "positional argument", arguments: []string{"serve"}, want: exitUsage},
		{name: "negative timeout", arguments: []string{"--timeout", "-1s"}, want: exitUsage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			code := run(test.arguments, environment(map[string]string{"HOME": t.TempDir()}), strings.NewReader(""), &stdout, &stderr)
			if code != test.want {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, test.want, stderr.String())
			}
			if test.wantOut != "" && !strings.Contains(stdout.String(), test.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), test.wantOut)
			}
		})
	}
}

// There is no --token flag, on purpose: a command line is readable by every
// process on the machine. The test pins that decision.
func TestRunHasNoTokenFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	code := run([]string{"--token", "operator-token"}, environment(map[string]string{"HOME": t.TempDir()}), strings.NewReader(""), &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if strings.Contains(stdout.String()+stderr.String(), "operator-token") {
		t.Error("the rejected flag's value was echoed")
	}
}

func TestRunServesTheProtocolOnStdoutAndTheReportOnStderr(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	getenv := environment(map[string]string{"HOME": home, config.EnvAPIURL: "https://api.deephost.test"})

	const token = "operator-token-from-the-settings-file"
	path, err := config.Path(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(path, config.Config{APIURL: "https://stored.deephost.test", Token: token}); err != nil {
		t.Fatal(err)
	}

	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deephost_version","arguments":{}}}` + "\n"
	var stdout, stderr strings.Builder
	if code := run(nil, getenv, strings.NewReader(request), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}

	// Everything on stdout is protocol, one JSON document per line.
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var answer map[string]any
		if err := json.Unmarshal([]byte(line), &answer); err != nil {
			t.Fatalf("stdout carried something that is not JSON-RPC: %q", line)
		}
		if answer["jsonrpc"] != "2.0" {
			t.Fatalf("stdout carried something that is not JSON-RPC: %q", line)
		}
	}
	// The environment beats the settings file, which is what the CLI does.
	if !strings.Contains(stdout.String(), "https://api.deephost.test") {
		t.Errorf("the resolved control plane is missing from the answer: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "credential from environment") && !strings.Contains(stderr.String(), "credential from file") {
		t.Errorf("stderr does not report where the credential came from: %q", stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), token) {
		t.Fatal("the operator credential was written to stdout or stderr")
	}
}

func TestRunStartsWithoutACredentialAndSaysSo(t *testing.T) {
	t.Parallel()
	getenv := environment(map[string]string{"HOME": t.TempDir()})
	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deephost_domain_add","arguments":{"name":"acme.dev"}}}` + "\n"

	var stdout, stderr strings.Builder
	if code := run(nil, getenv, strings.NewReader(request), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no operator credential") {
		t.Errorf("stderr does not say the server has no credential: %q", stderr.String())
	}
	// The mutating tool refuses; nothing was dialled.
	if !strings.Contains(stdout.String(), "credential_required") {
		t.Errorf("a mutating tool did not refuse: %s", stdout.String())
	}
}

// A settings file something else left world-readable is not a reason to refuse
// to start: the server serves what it can and carries the reason into the
// refusal a mutating tool gives.
func TestRunSurvivesAnUnreadableSettingsFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	getenv := environment(map[string]string{"HOME": home})
	path, err := config.Path(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), config.DirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"api_url":"https://api.deephost.test","token":"exposed"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deephost_version","arguments":{}}}` + "\n"
	var stdout, stderr strings.Builder
	if code := run(nil, getenv, strings.NewReader(request), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
	if !strings.Contains(stderr.String(), "settings file could not be read") {
		t.Errorf("stderr does not explain the settings file: %q", stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "exposed") {
		t.Fatal("the credential from the rejected file was rendered")
	}
	if !strings.Contains(stdout.String(), "credential_note") {
		t.Errorf("the reason did not reach the caller: %s", stdout.String())
	}
}
