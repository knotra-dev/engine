package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/schemas"
)

const desktopControlProtocol = "knotra.desktop-control/1"
const maxDesktopControlBytes = 8 << 20

type desktopDescriptor struct {
	Protocol string `json:"protocol"`
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
}

func readDesktopDescriptor(path string) (desktopDescriptor, error) {
	var descriptor desktopDescriptor
	invalid := fmt.Errorf("desktop control is unavailable. Open Knotra, wait for the workspace to load, then reconnect the MCP client")
	if !filepath.IsAbs(path) {
		return descriptor, invalid
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return descriptor, invalid
	}
	file, err := os.Open(path)
	if err != nil {
		return descriptor, invalid
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return descriptor, invalid
	}
	data, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
	if err != nil || len(data) > 16<<10 || json.Unmarshal(data, &descriptor) != nil || descriptor.Protocol != desktopControlProtocol || len(descriptor.Token) < 32 || len(descriptor.Token) > 4096 || strings.ContainsAny(descriptor.Token, "\r\n\x00") {
		return desktopDescriptor{}, invalid
	}
	for _, character := range descriptor.Token {
		if character < 0x21 || character > 0x7e {
			return desktopDescriptor{}, invalid
		}
	}
	endpoint, err := url.Parse(descriptor.Endpoint)
	if err != nil || endpoint.Scheme != "http" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || strings.Contains(descriptor.Endpoint, "#") || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.Port() == "" {
		return desktopDescriptor{}, invalid
	}
	ip := net.ParseIP(endpoint.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return desktopDescriptor{}, invalid
	}
	descriptor.Endpoint = strings.TrimRight(descriptor.Endpoint, "/")
	return descriptor, nil
}

type desktopBridge struct {
	path   string
	client *http.Client
}

func newDesktopBridge(path string) *desktopBridge {
	return &desktopBridge{path: path, client: &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func desktopToolError(code string) *mcp.CallToolResult {
	message := "Knotra could not complete this action. Check the app and retry with the same requestId."
	switch strings.ToLower(code) {
	case "invalid", "invalid_arguments":
		code, message = "INVALID_ARGUMENTS", "The tool arguments do not match its schema. Read the tool definition and try again."
	case "conflict", "workspace_conflict", "request_conflict":
		code, message = "CONFLICT", "The pipeline or request changed. Read it again; use its current revision and a new requestId for a different edit."
	case "dirty", "unsaved_changes":
		code, message = "UNSAVED_CHANGES", "The pipeline has unsaved edits. Read it first and set allowDirty only when you intend to replace those edits."
	case "busy":
		code, message = "BUSY", "Knotra rejected this action while an edit or update was active. Finish it in the app, read the latest state, then retry with a new requestId."
	case "not_found":
		code, message = "NOT_FOUND", "This item no longer exists. List the available items again."
	case "disabled", "unavailable":
		code, message = "UNAVAILABLE", "Open Knotra, wait for the workspace to load, then reconnect the MCP client."
	case "timeout":
		code, message = "TIMEOUT", "Knotra did not confirm the result in time. Check its state and retry with the same requestId; do not create a new mutation ID."
	case "limit":
		code, message = "LIMIT_EXCEEDED", "This response exceeds the desktop control limit of 8 MiB. Inspect the file or large result in the app."
	case "storage":
		code, message = "STORAGE_ERROR", "Knotra could not save this change. Read the latest pipeline or source state first. Use a new requestId only after confirming that the earlier change was not applied."
	case "credential_echo":
		code, message = "CREDENTIAL_REDACTED", "Knotra returned credential data, so this response was withheld. Check the connection in Settings."
	default:
		code = "CONTROL_ERROR"
	}
	data, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}
}

func containsDesktopSecret(value any, secret string) bool {
	switch value := value.(type) {
	case string:
		return strings.Contains(value, secret)
	case []any:
		for _, item := range value {
			if containsDesktopSecret(item, secret) {
				return true
			}
		}
	case map[string]any:
		for key, item := range value {
			if strings.Contains(key, secret) || containsDesktopSecret(item, secret) {
				return true
			}
		}
	}
	return false
}

func (b *desktopBridge) call(ctx context.Context, name string, arguments json.RawMessage) *mcp.CallToolResult {
	descriptor, err := readDesktopDescriptor(b.path)
	if err != nil {
		return desktopToolError("unavailable")
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(arguments, &args) != nil {
		return desktopToolError("invalid")
	}
	requestID := uuid.NewString()
	if raw, ok := args["requestId"]; name != "engine_call" && ok {
		_ = json.Unmarshal(raw, &requestID)
		delete(args, "requestId")
	} else if raw, ok := args["operationId"]; name == "engine_call" && ok {
		_ = json.Unmarshal(raw, &requestID)
	}
	body, err := json.Marshal(args)
	if err != nil {
		return desktopToolError("invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, descriptor.Endpoint+"/v1/tools/"+name, bytes.NewReader(body))
	if err != nil {
		return desktopToolError("unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+descriptor.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Knotra-Request-Id", requestID)
	response, err := b.client.Do(req)
	if err != nil {
		return desktopToolError("timeout")
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDesktopControlBytes+1))
	if err != nil || len(data) > maxDesktopControlBytes {
		return desktopToolError("control_error")
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return desktopToolError("unavailable")
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return desktopToolError("control_error")
	}
	if envelope.Error != nil {
		return desktopToolError(envelope.Error.Code)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || len(envelope.Result) == 0 {
		return desktopToolError("control_error")
	}
	result, err := contract.DecodeJSONLimit(envelope.Result, maxDesktopControlBytes)
	if err != nil {
		return desktopToolError("control_error")
	}
	secrets := []string{descriptor.Token}
	for _, field := range []string{"apiKey", "token"} {
		var secret string
		if json.Unmarshal(args[field], &secret) == nil && secret != "" {
			secrets = append(secrets, secret)
		}
	}
	for _, secret := range secrets {
		if containsDesktopSecret(result, secret) {
			return desktopToolError("credential_echo")
		}
	}
	if _, ok := result.(map[string]any); !ok {
		return desktopToolError("control_error")
	}
	return &mcp.CallToolResult{StructuredContent: result, Content: []mcp.Content{&mcp.TextContent{Text: string(envelope.Result)}}}
}

func newDesktopControlServer(bridge *desktopBridge, version string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "knotra-desktop", Version: version}, &mcp.ServerOptions{
		Instructions: "Control the running Knotra app. First read knotra://guide and knotra://schema/v1. Read a pipeline before editing it; preserve its revision and unsaved edits. Request-ID replay lasts only for the current app session: after app restart, read and reconcile pipelines/sources before retrying an uncertain action. Engine operationId receipts are durable. Credentials are write-only; engine_call accepts only its closed command vocabulary.",
	})
	for _, definition := range desktopControlTools() {
		server.AddTool(&mcp.Tool{Name: definition.name, Title: definition.title, Description: definition.description, InputSchema: definition.schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: definition.readOnly}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				valid := len(req.Params.Arguments) <= maxDesktopControlBytes && contract.ValidateValue(contract.Port{Schema: definition.schema}, contract.Value{JSON: req.Params.Arguments}) == nil
				if !valid {
					return desktopToolError("invalid"), nil
				}
				return bridge.call(ctx, definition.name, req.Params.Arguments), nil
			})
	}
	for _, resource := range []struct{ uri, name, mime, text string }{
		{"knotra://guide", "Knotra desktop control guide", "text/markdown", desktopControlGuide},
		{"knotra://schema/v1", "Knotra pipeline and engine profile schema", "application/schema+json", string(schemas.V1)},
	} {
		server.AddResource(&mcp.Resource{URI: resource.uri, Name: resource.name, MIMEType: resource.mime},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: resource.uri, MIMEType: resource.mime, Text: resource.text}}}, nil
			})
	}
	return server
}

type desktopMCPWriter struct{ io.Writer }

func (desktopMCPWriter) Close() error { return nil }

func (s *commandState) desktopMCPCommand() *cobra.Command {
	var desktop bool
	var path string
	cmd := &cobra.Command{Use: "serve", Short: "Expose the running desktop app to an MCP client over stdio", Args: cobra.NoArgs,
		Long:    "Expose the running Knotra desktop app over MCP stdio. Open Knotra and copy its MCP configuration from Settings. The private bridge descriptor stays on this computer; credentials are never printed. The app must remain open with its workspace loaded. This command does not start an engine or run workflows on its own.",
		Example: "  knotra mcp serve --desktop --bridge-file /private/path/desktop-control.json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !desktop {
				return fmt.Errorf("use --desktop and the bridge file supplied by Knotra Settings")
			}
			if _, err := readDesktopDescriptor(path); err != nil {
				return err
			}
			bridge := newDesktopBridge(path)
			defer bridge.client.CloseIdleConnections()
			return newDesktopControlServer(bridge, s.options.Version).Run(cmd.Context(), &mcp.IOTransport{Reader: io.NopCloser(s.options.In), Writer: desktopMCPWriter{s.options.Out}, MaxLineLength: maxDesktopControlBytes})
		},
	}
	cmd.Flags().BoolVar(&desktop, "desktop", false, "Control the running Knotra desktop app")
	cmd.Flags().StringVar(&path, "bridge-file", "", "Private desktop bridge descriptor from Settings")
	_ = cmd.MarkFlagRequired("bridge-file")
	return cmd
}
