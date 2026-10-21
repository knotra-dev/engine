package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knotra-dev/engine/internal/protocol"
)

func TestHelpExplainsWorkflowAndResourceDefaults(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"help"}} {
		out, stderr, err := invoke(t, args, "")
		if err != nil || stderr != "" {
			t.Fatalf("args=%v err=%v stderr=%s", args, err, stderr)
		}
		for _, want := range []string{"Getting started:", "Work with workflows:", "Manage engine data:", "knotra init", "knotra status", "--format"} {
			if !strings.Contains(out, want) {
				t.Fatalf("help missing %q:\n%s", want, out)
			}
		}
	}
	for _, resource := range []string{"runs", "pipelines", "requests", "artifacts", "operations"} {
		out, _, err := invoke(t, []string{resource, "--help"}, "")
		if err != nil || !strings.Contains(out, "Omit the subcommand to list") || !strings.Contains(out, "Examples:") {
			t.Fatalf("%s help: %s, %v", resource, out, err)
		}
	}
}

func TestProviderSetupHelpAndEarlyValidation(t *testing.T) {
	for _, command := range []string{"quickstart", "doctor"} {
		out, _, err := invoke(t, []string{command, "--help"}, "")
		for _, want := range []string{"--base-url", "--model", "--provider", "codex", "claude", "openai-compatible", "API key"} {
			if err != nil || !strings.Contains(out, want) {
				t.Fatalf("%s help missing %q: %s %v", command, want, out, err)
			}
		}
		_, _, err = invoke(t, []string{command, "--provider", "claude", "--model", "fixture-model", "--base-url", "https://gateway.example/v1"}, "")
		if err == nil || !strings.Contains(err.Error(), "not used by claude") {
			t.Fatalf("%s allowed an API endpoint for the Claude account connection: %v", command, err)
		}
		_, _, err = invoke(t, []string{command, "--provider", "openai-compatible", "--model", "org/model"}, "")
		if err == nil || !strings.Contains(err.Error(), "--base-url is required") {
			t.Fatalf("%s did not explain compatible server setup: %v", command, err)
		}
	}
}

func TestResourceDefaultsAndListAliases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") != "next" {
			t.Errorf("missing cursor: %s", r.URL)
		}
		_, _ = fmt.Fprint(w, `{"items":[],"nextCursor":null}`)
	}))
	defer server.Close()
	for _, resource := range []string{"runs", "pipelines", "definitions", "requests", "artifacts"} {
		for _, sub := range []string{"", "list", "ls"} {
			args := []string{"-e", server.URL, resource}
			if sub != "" {
				args = append(args, sub)
			}
			args = append(args, "--cursor", "next", "--format", "json")
			out, stderr, err := invoke(t, args, "")
			if err != nil || stderr != "" || strings.TrimSpace(out) != `{"items":[],"nextCursor":null}` {
				t.Fatalf("args=%v: out=%s stderr=%s err=%v", args, out, stderr, err)
			}
		}
	}
	for _, sub := range []string{"", "list", "ls"} {
		args := []string{"operations", "--state-dir", t.TempDir()}
		if sub != "" {
			args = append(args, sub)
		}
		out, _, err := invoke(t, args, "")
		if err != nil || out != "No results.\n" {
			t.Fatalf("empty operations: %q, %v", out, err)
		}
	}
}

func TestErrorsUseRequestedFormatAndStderr(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "unknown"},
		{"--format", "json", "runs", "unknown"},
		{"runs", "show", "--json"},
		{"--json", "run", "--wrong-flag"},
		{"--json", "--format", "yaml", "runs"},
		{"--json", "--format", "text", "runs"},
	} {
		out, stderr, err := invoke(t, args, "")
		var response struct {
			Error struct {
				Message, Hint string
			}
			ExitCode int
		}
		if err == nil ||
			out != "" ||
			json.Unmarshal([]byte(stderr), &response) != nil ||
			response.ExitCode != ExitCode(err) ||
			response.Error.Message == "" ||
			response.Error.Hint == "" {
			t.Fatalf("args=%v out=%s stderr=%s err=%v", args, out, stderr, err)
		}
	}
	out, stderr, err := invoke(t, []string{"rnu"}, "")
	if err == nil || out != "" || !strings.Contains(stderr, "Did you mean: run") || !strings.Contains(stderr, "--help") {
		t.Fatalf("unknown command: out=%s stderr=%s err=%v", out, stderr, err)
	}
}

func TestErrorsRetainEngineDiagnostics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"code":"FORBIDDEN","message":"access denied","diagnostics":[{"code":"SCOPE","message":"missing scope"}]}`)
	}))
	defer server.Close()
	out, stderr, err := invoke(t, []string{"info", "-e", server.URL, "-j"}, "")
	if err == nil ||
		out != "" ||
		!json.Valid([]byte(stderr)) ||
		!strings.Contains(stderr, `"code":"FORBIDDEN"`) ||
		!strings.Contains(stderr, "missing scope") ||
		!strings.Contains(stderr, "KNOTRA_TOKEN") {
		t.Fatalf("out=%s stderr=%s err=%v", out, stderr, err)
	}
}

func TestRunShortcutsPreserveResponseAndIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/info":
			_, _ = fmt.Fprintf(w, `{"protocol":%q,"engineId":"engine","principalId":"user"}`, protocol.Version)
		case "/v1/runs/run":
			_, _ = fmt.Fprint(w, `{"run":{"id":"run","title":"Example","status":"running","inputs":{"count":9007199254740993},"availableActions":["cancel"]}}`)
		case "/v1/runs/run/cancel":
			if r.Header.Get("Idempotency-Key") != "cancel-key" {
				t.Error("cancel shortcut lost idempotency key")
			}
			_, _ = fmt.Fprint(w, `{"accepted":true,"runId":"run"}`)
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	for _, args := range [][]string{{"status", "run"}, {"runs", "show", "run"}, {"runs", "get", "run"}} {
		out, _, err := invoke(t, append(args, "-e", server.URL, "-j"), "")
		if err != nil || !strings.Contains(out, `"run":{`) || !strings.Contains(out, "9007199254740993") {
			t.Fatalf("%v: out=%s err=%v", args, out, err)
		}
	}
	out, _, err := invoke(t, []string{"status", "run", "-e", server.URL}, "")
	for _, want := range []string{"Example", "9007199254740993", "Actions:", "cancel"} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("human status missing %s: %s, %v", want, out, err)
		}
	}
	out, _, err = invoke(t, []string{"cancel", "run", "-e", server.URL, "--state-dir", t.TempDir(), "--idempotency-key", "cancel-key", "--json"}, "")
	if err != nil || !strings.Contains(out, `"operationId":"cancel-key"`) || !strings.Contains(out, `"accepted":true`) {
		t.Fatalf("cancel: %s, %v", out, err)
	}
}

func TestValidationDefaultsAndProfileSeparation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pipeline.yaml"), []byte(minimalPipeline), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	out, _, err := invoke(t, []string{"validate", "--format", "json"}, "")
	if err != nil || !strings.Contains(out, `"valid":true`) {
		t.Fatalf("default validation: %s, %v", out, err)
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"validate", "--profile", "local"}, "--profile-file"},
		{[]string{"validate", "--remote", "--profile-file", "profile.yaml"}, "offline validation"},
		{[]string{"validate", "--remote"}, "KNOTRA_PROFILE"},
		{[]string{"runs", "wait", "run", "--timeout", "-1s"}, "--timeout"},
		{[]string{"run", "pipeline.yaml", "--definition", "pipeline", "-p", "local"}, "cannot be combined"},
	} {
		_, _, err := invoke(t, test.args, "")
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%v: %v, want %s", test.args, err, test.want)
		}
	}
	// An engine profile in the environment must not turn an offline check into a file read.
	err = Execute(context.Background(), []string{"validate"}, Options{Out: io.Discard, Err: io.Discard, Getenv: func(key string) string {
		if key == "KNOTRA_PROFILE" {
			return "local"
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
}
