package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/knotra-dev/engine/internal/contract"
)

type cliFixture struct {
	Output, Events, Auth string
	Fail                 bool
	Delay                bool
}

type cliFixtureCall struct {
	Args, Env []string
	Input     string
}

func TestMain(m *testing.M) {
	provider := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if cliModelProvider(provider) {
		data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".knotra-cli-fixture.json"))
		if err != nil {
			os.Exit(2)
		}
		var fixture cliFixture
		if json.Unmarshal(data, &fixture) != nil {
			os.Exit(2)
		}
		if slices.Contains(os.Args, "--help") {
			_, _ = fmt.Println("--restricted --json-schema --strict-mcp-config --ignore-user-config --ignore-rules --output-schema --ephemeral")
			os.Exit(0)
		}
		if slices.Contains(os.Args, "status") {
			if provider == "codex" {
				_, _ = fmt.Fprintln(os.Stderr, "Logged in using "+fixture.Auth)
			} else {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"authMethod": fixture.Auth})
			}
			os.Exit(0)
		}
		input, _ := io.ReadAll(os.Stdin)
		call, _ := json.Marshal(cliFixtureCall{Args: os.Args[1:], Env: os.Environ(), Input: string(input)})
		log, err := os.OpenFile(filepath.Join(os.Getenv("HOME"), "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(2)
		}
		_, _ = log.Write(append(call, '\n'))
		_ = log.Close()
		if fixture.Delay {
			time.Sleep(time.Minute)
		}
		if fixture.Fail {
			_, _ = fmt.Fprintln(os.Stderr, "secret-value and private prompt")
			os.Exit(1)
		}
		if provider == "codex" {
			index := slices.Index(os.Args, "--output-last-message")
			if index < 0 || index+1 >= len(os.Args) || os.WriteFile(os.Args[index+1], []byte(fixture.Output), 0600) != nil {
				os.Exit(2)
			}
			_, _ = fmt.Print(fixture.Events)
		} else {
			_, _ = fmt.Print(fixture.Output)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func installCLIFixture(t *testing.T, provider string, fixture cliFixture) string {
	t.Helper()
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := provider
	if strings.HasSuffix(executable, ".exe") {
		name += ".exe"
	}
	target := filepath.Join(dir, name)
	if err = os.Link(executable, target); err != nil {
		data, readErr := os.ReadFile(executable)
		if readErr != nil || os.WriteFile(target, data, 0700) != nil {
			t.Fatalf("install fixture executable: %v %v", err, readErr)
		}
	}
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if fixture.Auth == "" {
		fixture.Auth = "claude.ai"
		if provider == "codex" {
			fixture.Auth = "ChatGPT"
		}
	}
	if fixture.Events == "" {
		// Codex 0.160.0 can emit a nonfatal error item before a successful turn.
		fixture.Events = "{\"type\":\"thread.started\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"error\"}}\n{\"type\":\"turn.started\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\"}}\n{\"type\":\"turn.completed\"}\n"
	}
	writeCLIFixture(t, fixture)
	return dir
}

func writeCLIFixture(t *testing.T, fixture cliFixture) {
	t.Helper()
	data, err := json.Marshal(fixture)
	if err != nil || os.WriteFile(filepath.Join(os.Getenv("HOME"), ".knotra-cli-fixture.json"), data, 0600) != nil {
		t.Fatal("cannot write CLI fixture")
	}
}

func cliRequest(provider string) Request {
	req := testRequest("")
	req.Plan.Profile.Spec.Models["local"] = contract.ModelConnection{Provider: provider, Model: "default"}
	req.Plan.Pipelines[req.Pipeline].Spec.Sandboxes = nil
	return req
}

func TestCLIAccountStructuredOutputAndReplay(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			output := `{"answer":42}`
			if provider == "claude" {
				output = `{"is_error":false,"structured_output":{"answer":42}}`
			}
			dir := installCLIFixture(t, provider, cliFixture{Output: output})
			t.Setenv("OPENAI_API_KEY", "secret-value")
			t.Setenv("CODEX_API_KEY", "secret-value")
			t.Setenv("ANTHROPIC_API_KEY", "secret-value")
			t.Setenv("KNOTRA_DATABASE_URL", "secret-value")
			t.Setenv("KNOTRA_DESKTOP_CONTROL_TOKEN", "secret-value")
			runner := &Runner{Hooks: newHooks()}
			req := cliRequest(provider)
			if err := runner.Prepare(context.Background(), req.Plan); err != nil {
				t.Fatal(err)
			}
			if req.Plan.Pipelines[req.Pipeline].Spec.Models["model"].Model != "" {
				t.Fatal("admission rewrote declared model to a CLI digest")
			}
			for range 2 {
				values, err := runner.Execute(context.Background(), req)
				if err != nil || string(values["answer"].JSON) != "42" {
					t.Fatalf("values=%v err=%v", values, err)
				}
			}
			data, err := os.ReadFile(filepath.Join(dir, "calls.jsonl"))
			if err != nil || len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 1 {
				t.Fatalf("replay called CLI again: %s %v", data, err)
			}
			var call cliFixtureCall
			if json.Unmarshal(data, &call) != nil ||
				strings.Contains(strings.Join(call.Env, "\n"), "secret-value") ||
				!strings.Contains(call.Input, "Knotra input context") ||
				slices.Contains(call.Args, "--model") {
				t.Fatalf("unsafe CLI invocation: %s", data)
			}
			flags := []string{"--restricted", "--tools", "--disallowedTools", "--strict-mcp-config", "--settings", "--no-session-persistence"}
			if provider == "codex" {
				flags = []string{
					"--ignore-user-config",
					"--ignore-rules",
					"--ephemeral",
					"read-only",
					"shell_tool",
					"hooks",
					"plugins",
					"apps",
					"multi_agent",
					"view_image",
					"browser_use",
					`forced_login_method="chatgpt"`,
				}
			}
			for _, flag := range flags {
				if !slices.Contains(call.Args, flag) {
					t.Fatalf("missing isolation flag %s: %v", flag, call.Args)
				}
			}
		})
	}
}

func TestCLIAccountToolsRemainKnotraManaged(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			output := `{"tool":"work","arguments":"{\"value\":7}"}`
			if provider == "claude" {
				output = `{"structured_output":` + output + `}`
			}
			installCLIFixture(t, provider, cliFixture{Output: output})
			runner := &Runner{Hooks: newHooks()}
			req := cliRequest(provider)
			messages := []message{{Role: "user", Content: "Use work then finish."}}
			tools := []functionTool{toolDefinition("work", "Work", json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}}}`))}
			response, err := runner.chat(context.Background(), req, "model", 0, messages, tools, nil)
			if err != nil ||
				len(response.ToolCalls) != 1 ||
				response.ToolCalls[0].Function.Name != "work" ||
				string(response.ToolCalls[0].Function.Arguments) != `{"value":7}` {
				t.Fatalf("tool decision lost: %+v %v", response, err)
			}
		})
	}
}

func TestCLIRejectsAPIKeyLoginAndHidesDiagnostics(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			installCLIFixture(t, provider, cliFixture{Auth: "api_key"})
			if err := CheckCLIProvider(context.Background(), provider); err == nil || !strings.Contains(err.Error(), "account login") {
				t.Fatalf("API-key login accepted: %v", err)
			}
			writeCLIFixture(t, cliFixture{Fail: true})
			_, err := (&Runner{}).cliResponse(
				context.Background(),
				contract.ModelConnection{Provider: provider, Model: "default"},
				nil,
				nil,
				json.RawMessage(`{"type":"object"}`),
			)
			if err == nil || strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "private prompt") {
				t.Fatalf("private diagnostics escaped: %v", err)
			}
			writeCLIFixture(t, cliFixture{Delay: true})
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if _, err := runCLI(ctx, provider, []string{"--print"}, t.TempDir(), nil, false); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("canceled CLI did not stop: %v", err)
			}
		})
	}
}

func TestCodexRejectsNativeToolsAndIncompleteEvents(t *testing.T) {
	if err := checkCodexEvents([]byte("{\"type\":\"item.updated\",\"item\":{\"type\":\"todo_list\"}}\n{\"type\":\"turn.completed\"}")); err != nil {
		t.Fatalf("harmless planning metadata rejected: %v", err)
	}
	for _, events := range []string{
		`{"type":"item.completed","item":{"type":"command_execution"}}`,
		`{"type":"item.started","item":{"type":"file_change"}}`,
		`{"type":"turn.failed"}`, `{"type":"error"}`, `{"type":"item.completed","item":{"type":"error"}}`, `{"type":"turn.started"}`, `invalid`,
		"{\"type\":\"turn.completed\"}\n{\"type\":\"turn.started\"}",
		"{\"type\":\"turn.completed\"}\n{\"type\":\"thread.started\"}",
	} {
		if err := checkCodexEvents([]byte(events)); err == nil {
			t.Fatalf("unsafe/incomplete event accepted: %s", events)
		}
	}
}

func TestClaudeIsolationDoesNotConsumeAuthSubcommand(t *testing.T) {
	args := append(claudeIsolationArgs(), "auth", "status")
	if slices.Contains(args, "--mcp-config") ||
		!slices.Contains(args, `--mcp-config={"mcpServers":{}}`) ||
		args[len(args)-2] != "auth" ||
		args[len(args)-1] != "status" {
		t.Fatalf("variadic MCP config would consume auth/status: %v", args)
	}
}

func TestCLIInvalidOutputIsDurable(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			dir := installCLIFixture(t, provider, cliFixture{Output: `not-json`})
			runner := &Runner{Hooks: newHooks()}
			for range 2 {
				if _, err := runner.Execute(context.Background(), cliRequest(provider)); err == nil {
					t.Fatal("invalid CLI output accepted")
				}
			}
			data, err := os.ReadFile(filepath.Join(dir, "calls.jsonl"))
			if err != nil || len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 1 {
				t.Fatalf("invalid output caused another call: %s %v", data, err)
			}
		})
	}
}

func TestCLIRealAccountStructuredOutput(t *testing.T) {
	provider := os.Getenv("KNOTRA_TEST_CLI_PROVIDER")
	if !cliModelProvider(provider) {
		t.Skip("set KNOTRA_TEST_CLI_PROVIDER=codex or claude to use the existing account login")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runner := &Runner{Hooks: newHooks()}
	req := cliRequest(provider)
	if err := runner.Prepare(ctx, req.Plan); err != nil {
		t.Fatal(err)
	}
	messages, err := initialMessages(req, req.Node.LLM.Instructions, req.Node.LLM.Prompt)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runner.cliResponse(ctx, req.Plan.Profile.Spec.Models["local"], messages, nil, contract.PortObjectSchema(req.Node.Outputs))
	if err != nil {
		t.Fatal(err)
	}
	values, err := jsonOutputs(req.Node.Outputs, []byte(response.Message.Content))
	if err != nil || string(values["answer"].JSON) != "42" {
		t.Fatalf("account CLI structured output: values=%v err=%v", values, err)
	}
}
