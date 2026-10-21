package adapters

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/knotra-dev/engine/internal/contract"
)

func cliModelProvider(provider string) bool { return provider == "codex" || provider == "claude" }

// CheckCLIProvider checks installed capabilities and the existing account login,
// without starting a model request or reading credentials into the engine.
func CheckCLIProvider(ctx context.Context, provider string) error {
	if !cliModelProvider(provider) {
		return fmt.Errorf("unsupported CLI provider %q", provider)
	}

	dir, err := os.MkdirTemp("", "knotra-cli-check-*")
	if err != nil {
		return err
	}

	defer func() { _ = os.RemoveAll(dir) }()
	helpArgs := []string{"--help"}
	required := []string{"--restricted", "--json-schema", "--strict-mcp-config"}
	statusArgs := append(claudeIsolationArgs(), "auth", "status")
	login := "claude auth login"
	if provider == "codex" {
		helpArgs = []string{"exec", "--help"}
		required = []string{"--ignore-user-config", "--ignore-rules", "--output-schema", "--ephemeral"}
		statusArgs = []string{"login", "status", "-c", `forced_login_method="chatgpt"`}
		login = "codex login"
	}
	help, err := runCLI(ctx, provider, helpArgs, dir, nil, true)
	if err != nil {
		return fmt.Errorf("install or update %s on the engine computer, then run %s as the engine user", provider, login)
	}

	for _, flag := range required {
		if !bytes.Contains(help, []byte(flag)) {
			return fmt.Errorf("update %s on the engine computer; this integration requires %s", provider, flag)
		}
	}
	status, err := runCLI(ctx, provider, statusArgs, dir, nil, provider == "codex")
	var account bool
	if provider == "codex" {
		account = bytes.Contains(status, []byte("Logged in using ChatGPT"))
	} else {
		var auth struct {
			AuthMethod string `json:"authMethod"`
		}
		account = json.Unmarshal(status, &auth) == nil && auth.AuthMethod == "claude.ai"
	}
	if err != nil || !account {
		return fmt.Errorf(
			"%s requires an account login; run %s as the same OS user that starts the engine, then retry (API-key logins are not used)",
			provider,
			login,
		)
	}

	return nil
}

func claudeIsolationArgs() []string {
	return []string{
		"--restricted",
		"--setting-sources",
		"",
		"--settings",
		`{"disableAllHooks":true,"forceLoginMethod":"claudeai"}`,
		`--mcp-config={"mcpServers":{}}`,
		"--strict-mcp-config",
	}
}

// Preserve login/keychain locations and network settings, never the engine's
// secrets or API credentials that would override subscription authentication.
func cliEnvironment() []string {
	allowed := map[string]bool{
		"HOME":    true,
		"USER":    true,
		"LOGNAME": true,
		"PATH":    true,
		"SHELL":   true,

		"USERPROFILE": true,
		"HOMEDRIVE":   true,
		"HOMEPATH":    true,
		"SYSTEMROOT":  true,
		"WINDIR":      true,

		"APPDATA":      true,
		"LOCALAPPDATA": true,
		"TEMP":         true,
		"TMP":          true,
		"TMPDIR":       true,

		"LANG":              true,
		"CODEX_HOME":        true,
		"CLAUDE_CONFIG_DIR": true,

		"XDG_CONFIG_HOME": true,
		"XDG_DATA_HOME":   true,
		"XDG_CACHE_HOME":  true,
		"XDG_RUNTIME_DIR": true,

		"DBUS_SESSION_BUS_ADDRESS": true,
		"HTTP_PROXY":               true,
		"HTTPS_PROXY":              true,
		"NO_PROXY":                 true,

		"SSL_CERT_FILE":        true,
		"SSL_CERT_DIR":         true,
		"NODE_EXTRA_CA_CERTS":  true,
		"CODEX_CA_CERTIFICATE": true,
	}
	result := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if allowed[upper] || strings.HasPrefix(upper, "LC_") {
			result = append(result, entry)
		}
	}
	return result
}

func runCLI(ctx context.Context, provider string, args []string, dir string, input []byte, mergeStderr bool) ([]byte, error) {
	if !cliModelProvider(provider) {
		return nil, fmt.Errorf("unsupported CLI provider")
	}

	executable, err := exec.LookPath(provider)
	if err != nil {
		return nil, fmt.Errorf("%s executable is unavailable", provider)
	}

	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(callCtx, executable, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, cliEnvironment(), bytes.NewReader(input)
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	cmd.Stderr = io.Discard
	if mergeStderr {
		cmd.Stderr = cmd.Stdout
	}
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot start %s; check its installation on the engine computer", provider)
	}

	data, readErr := io.ReadAll(io.LimitReader(stdout, maxResponseBytes+1))
	if readErr != nil || len(data) > maxResponseBytes {
		cancel()
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if readErr != nil || len(data) > maxResponseBytes {
		return nil, invalidModelResponse()
	}
	if err != nil {
		// CLI diagnostics can contain account details, tokens or private prompts.
		return nil, fmt.Errorf("%s failed; check its account login, model access and usage limits in a terminal", provider)
	}

	return data, nil
}

func (r *Runner) cliResponse(ctx context.Context, c contract.ModelConnection, messages []message, tools []functionTool, format json.RawMessage) (chatResponse, error) {
	dir, err := os.MkdirTemp("", "knotra-cli-model-*")
	if err != nil {
		return chatResponse{}, err
	}

	defer func() { _ = os.RemoveAll(dir) }()
	schema := format
	if len(tools) > 0 {
		names := make([]string, 0, len(tools))
		for _, tool := range tools {
			names = append(names, tool.Function.Name)
		}
		schema, err = json.Marshal(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool":      map[string]any{"type": "string", "enum": names},
				"arguments": map[string]any{"type": "string", "description": "JSON-encoded arguments for the selected tool"},
			},
			"required":             []string{"tool", "arguments"},
			"additionalProperties": false,
		})
		if err != nil {
			return chatResponse{}, err
		}
	}
	input, err := json.Marshal(map[string]any{"messages": messages, "tools": tools})
	if err != nil {
		return chatResponse{}, err
	}

	prompt := append([]byte("You are the model for one Knotra workflow turn. Read the conversation below and follow its system instructions. Return only the declared JSON output. When tools are supplied, select exactly one declared tool and return {tool, arguments}, with arguments encoded as a JSON string. Do not execute tools yourself: Knotra runs them with the workflow's permissions and returns their results in the conversation.\n\n"), input...)
	var args []string
	var outputPath string
	if c.Provider == "codex" {
		schemaPath := filepath.Join(dir, "output-schema.json")
		if err = os.WriteFile(schemaPath, schema, 0600); err != nil {
			return chatResponse{}, err
		}
		outputPath = filepath.Join(dir, "output.json")
		args = []string{
			"exec",
			"--ignore-user-config",
			"--ignore-rules",
			"--ephemeral",
			"--skip-git-repo-check",
			"--sandbox",
			"read-only",
			"--color",
			"never",
			"--json",
			"--output-schema",
			schemaPath,
			"--output-last-message",
			outputPath,
		}
		for _, option := range []string{`forced_login_method="chatgpt"`, `approval_policy="never"`, `web_search="disabled"`, "project_doc_max_bytes=0"} {
			args = append(args, "-c", option)
		}
		for _, feature := range []string{
			"shell_tool",
			"shell_snapshot",
			"hooks",
			"apps",
			"plugins",
			"remote_plugin",
			"multi_agent",
			"computer_use",
			"browser_use",
			"browser_use_external",
			"image_generation",
			"view_image",
			"code_mode",
			"code_mode_host",
			"artifact",
		} {
			args = append(args, "--disable", feature)
		}
	} else {
		args = append(
			claudeIsolationArgs(),
			"--print",
			"--tools",
			"",
			"--disallowedTools",
			"mcp__*",
			"--disable-slash-commands",
			"--no-chrome",
			"--permission-mode",
			"dontAsk",
			"--no-session-persistence",
			"--output-format",
			"json",
			"--json-schema",
			string(schema),
		)
	}
	if c.Model != "default" {
		args = append(args, "--model", c.Model)
	}
	if c.Provider == "codex" {
		args = append(args, "-")
	}
	output, err := runCLI(ctx, c.Provider, args, dir, prompt, false)
	if err != nil {
		return chatResponse{}, err
	}
	if c.Provider == "codex" {
		if err = checkCodexEvents(output); err != nil {
			return chatResponse{}, err
		}
		file, err := os.Open(outputPath)
		if err != nil {
			return chatResponse{}, failure("OUTPUT_INVALID", fmt.Errorf("codex did not write its final JSON output"))
		}
		defer func() { _ = file.Close() }()
		output, err = io.ReadAll(io.LimitReader(file, maxResponseBytes+1))
		if err != nil || len(output) > maxResponseBytes {
			return chatResponse{}, invalidModelResponse()
		}
	} else {
		var result struct {
			IsError          bool            `json:"is_error"`
			StructuredOutput json.RawMessage `json:"structured_output"`
		}
		if json.Unmarshal(output, &result) != nil || result.IsError || len(result.StructuredOutput) == 0 {
			return chatResponse{}, invalidModelResponse()
		}
		output = result.StructuredOutput
	}
	result := chatResponse{Message: message{Role: "assistant", Content: string(output)}, Done: true, DoneReason: "stop"}
	if !json.Valid(output) {
		return chatResponse{}, failure("OUTPUT_INVALID", fmt.Errorf("CLI final output is not JSON"))
	}
	if len(tools) > 0 {
		var decision struct{ Tool, Arguments string }
		if json.Unmarshal(output, &decision) != nil || !json.Valid([]byte(decision.Arguments)) {
			return chatResponse{}, invalidModelResponse()
		}
		for _, tool := range tools {
			if tool.Function.Name == decision.Tool {
				call := toolCall{ID: "cli_call"}
				call.Function.Name, call.Function.Arguments = decision.Tool, json.RawMessage(decision.Arguments)
				result.Message.Content, result.Message.ToolCalls = "", []toolCall{call}
				return result, nil
			}
		}
		return chatResponse{}, invalidModelResponse()
	}

	return result, nil
}

func checkCodexEvents(data []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxResponseBytes)
	completed := false
	for scanner.Scan() {
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return failure("OUTPUT_INVALID", fmt.Errorf("codex emitted malformed JSON events"))
		}

		switch event.Type {
		case "thread.started", "turn.started":
			completed = false
		case "turn.completed":
			completed = true
		case "item.started", "item.updated", "item.completed":
			switch event.Item.Type {
			case "agent_message", "reasoning", "todo_list", "error":
			case "command_execution", "file_change", "mcp_tool_call", "collab_tool_call", "web_search":
				return failure("CLI_TOOL_UNEXPECTED", fmt.Errorf("codex reported %s; this connection only permits Knotra-managed tools", event.Item.Type))
			default:
				return failure("OUTPUT_INVALID", fmt.Errorf("codex emitted an error or unsupported item type"))
			}
		default:
			return failure("OUTPUT_INVALID", fmt.Errorf("codex emitted a failed or unsupported turn event"))
		}
	}
	if scanner.Err() != nil || !completed {
		return failure("OUTPUT_INVALID", fmt.Errorf("codex event stream ended without turn completion"))
	}

	return nil
}
