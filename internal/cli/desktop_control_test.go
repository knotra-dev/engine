package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const fixtureDesktopToken = "private-desktop-control-token-0123456789abcdef"

func TestDesktopToolErrorsDescribeRetryScope(t *testing.T) {
	for code, want := range map[string]string{
		"busy":        "new requestId",
		"storage":     "confirming that the earlier change was not applied",
		"unavailable": "wait for the workspace to load",
	} {
		result := desktopToolError(code)
		content, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatal("expected a text error response")
		}
		text := content.Text
		if !result.IsError || !strings.Contains(text, want) || strings.Contains(text, "enable assistant control") {
			t.Fatalf("%s: %s", code, text)
		}
	}
	if !strings.Contains(desktopControlGuide, "not durable across app restarts") || !strings.Contains(desktopControlGuide, "saved as local drafts") {
		t.Fatal("control guide lost replay scope or local draft behavior")
	}
}

func writeDesktopDescriptor(t *testing.T, path, endpoint, token string) {
	t.Helper()
	data, err := json.Marshal(desktopDescriptor{Protocol: desktopControlProtocol, Endpoint: endpoint, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopDescriptorRequiresPrivateLoopbackFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.json")
	writeDesktopDescriptor(t, path, "http://127.0.0.1:12345", fixtureDesktopToken)
	if descriptor, err := readDesktopDescriptor(path); err != nil || descriptor.Token != fixtureDesktopToken {
		t.Fatalf("private descriptor: %v", err)
	}
	for _, endpoint := range []string{
		"http://localhost:12345", "https://127.0.0.1:12345", "http://192.0.2.1:12345", "http://127.0.0.1",
		"http://secret@127.0.0.1:12345", "http://127.0.0.1:12345?token=secret", "http://127.0.0.1:12345#secret", "http://127.0.0.1:12345/other",
	} {
		writeDesktopDescriptor(t, path, endpoint, fixtureDesktopToken)
		if _, err := readDesktopDescriptor(path); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid descriptor accepted or echoed: %s %v", endpoint, err)
		}
	}
	writeDesktopDescriptor(t, path, "http://[::1]:12345", fixtureDesktopToken)
	if _, err := readDesktopDescriptor(path); err != nil {
		t.Fatal(err)
	}
	if _, err := readDesktopDescriptor("relative-descriptor.json"); err == nil {
		t.Fatal("relative descriptor was accepted")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := readDesktopDescriptor(path); err == nil {
			t.Fatal("readable descriptor was accepted")
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "bridge-link")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		if _, err := readDesktopDescriptor(link); err == nil {
			t.Fatal("symlink descriptor was accepted")
		}
	}
	for _, content := range []string{`{}`, `null`, `{"protocol":"wrong"}`, strings.Repeat("x", 16<<10+1)} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readDesktopDescriptor(path); err == nil {
			t.Fatal("invalid descriptor was accepted")
		}
	}
}

func TestDesktopBridgeReloadsDescriptorAndSanitizesFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.json")
	var calls atomic.Int32
	var redirects atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirects.Add(1) }))
	defer target.Close()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+fixtureDesktopToken || r.Header.Get("X-Knotra-Request-Id") == "" {
			t.Error("desktop request lost its private authentication or identity")
		}
		switch strings.TrimPrefix(r.URL.Path, "/v1/tools/") {
		case "redirect":
			w.Header().Set("Location", target.URL)
			w.WriteHeader(http.StatusFound)
		case "error":
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprintf(w, `{"error":{"code":"conflict","message":%q}}`, fixtureDesktopToken)
		case "large":
			_, _ = io.WriteString(w, strings.Repeat("x", maxDesktopControlBytes+1))
		case "echo":
			_, _ = fmt.Fprint(w, `{"result":{"credential":"private-provider-key"}}`)
		default:
			_, _ = fmt.Fprint(w, `{"result":{"ok":true}}`)
		}
	}))
	defer remote.Close()
	writeDesktopDescriptor(t, path, remote.URL, fixtureDesktopToken)
	bridge := newDesktopBridge(path)
	defer bridge.client.CloseIdleConnections()
	if result := bridge.call(t.Context(), "ok", []byte(`{}`)); result.IsError {
		t.Fatal(result)
	}
	for _, name := range []string{"redirect", "error", "large", "echo"} {
		result := bridge.call(t.Context(), name, []byte(`{"apiKey":"private-provider-key"}`))
		data, _ := json.Marshal(result)
		if !result.IsError ||
			strings.Contains(string(data), fixtureDesktopToken) ||
			strings.Contains(string(data), "private-provider-key") ||
			redirects.Load() != 0 {
			t.Fatalf("unsafe failure %s: %s", name, data)
		}
	}
	var rotated atomic.Int32
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fixtureDesktopToken+"-rotated" {
			t.Error("descriptor rotation kept the old token")
		}
		rotated.Add(1)
		_, _ = fmt.Fprint(w, `{"result":{"ok":true}}`)
	}))
	defer next.Close()
	writeDesktopDescriptor(t, path, next.URL, fixtureDesktopToken+"-rotated")
	if result := bridge.call(t.Context(), "ok", []byte(`{}`)); result.IsError || rotated.Load() != 1 || calls.Load() != 5 {
		t.Fatalf("descriptor rotation failed: %+v", result)
	}
}

func TestDesktopControlActualCLIStdioProtocol(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	work := t.TempDir()
	binary := filepath.Join(work, "knotra")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/knotra")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v %s", err, output)
	}
	var mu sync.Mutex
	var requests int
	var sourceCreates int
	var source string
	revision := "revision-a"
	publicSource := map[string]any{
		"id":            "source-fixture",
		"provider":      "openai",
		"revision":      1,
		"hasCredential": true,
		"models":        []any{map[string]any{"id": "fixture-model"}},
	}
	memo := map[string][]byte{}
	fingerprints := map[string]string{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		if r.Header.Get("Authorization") != "Bearer "+fixtureDesktopToken || r.Method != http.MethodPost {
			t.Error("invalid desktop bridge authentication")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var args map[string]any
		if json.Unmarshal(data, &args) != nil {
			t.Error("invalid bridge arguments")
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v1/tools/")
		requestID := r.Header.Get("X-Knotra-Request-Id")
		fingerprint := sha256.Sum256(append([]byte(name), data...))
		if previous, ok := memo[requestID]; ok {
			if fingerprints[requestID] != hex.EncodeToString(fingerprint[:]) {
				t.Error("request identity was reused with different arguments")
			}
			_, _ = w.Write(previous)
			return
		}
		var result map[string]any
		switch name {
		case "pipeline_create":
			if _, exists := args["requestId"]; exists {
				t.Error("bridge request ID was not removed from the typed body")
			}
			source, _ = args["source"].(string)
			result = map[string]any{"workspace": map[string]any{"id": "workspace-fixture", "source": source}, "revision": revision}
		case "pipeline_get":
			result = map[string]any{"workspace": map[string]any{"id": "workspace-fixture", "source": source}, "revision": revision}
		case "pipeline_list":
			result = map[string]any{"pipelines": []any{map[string]any{"id": "workspace-fixture", "revision": revision}}}
		case "pipeline_update":
			if args["expectedRevision"] != revision {
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprint(w, `{"error":{"code":"conflict","message":"stale edit"}}`)
				return
			}
			source, _ = args["source"].(string)
			revision = "revision-b"
			result = map[string]any{"workspace": map[string]any{"id": "workspace-fixture", "source": source}, "revision": revision}
		case "pipeline_delete":
			result = map[string]any{"deleted": args["id"]}
		case "model_source_save":
			sourceCreates++
			if args["apiKey"] != "private-provider-key" {
				t.Error("source credential was not forwarded")
			}
			result = map[string]any{"connection": publicSource}
		case "model_source_list":
			result = map[string]any{"connections": []any{publicSource}}
		case "engine_call":
			if args["op"] == "respond" {
				if args["requestId"] != "human-request-fixture" || requestID != "durable-response-operation" {
					t.Error("engine response lost its human request or durable operation ID")
				}
				result = map[string]any{"accepted": true, "requestId": args["requestId"]}
			} else {
				result = map[string]any{"items": []any{}}
			}
		default:
			t.Errorf("unexpected bridge operation %s", name)
			return
		}
		encoded, err := json.Marshal(map[string]any{"result": result})
		if err != nil {
			t.Error(err)
			return
		}
		memo[requestID], fingerprints[requestID] = encoded, hex.EncodeToString(fingerprint[:])
		_, _ = w.Write(encoded)
	}))
	defer remote.Close()
	path := filepath.Join(work, "bridge.json")
	writeDesktopDescriptor(t, path, remote.URL, fixtureDesktopToken)
	command := exec.CommandContext(ctx, binary, "mcp", "serve", "--desktop", "--bridge-file", path)
	command.Env = []string{"HOME=" + work, "PATH=" + os.Getenv("PATH")}
	client := mcp.NewClient(
		&mcp.Implementation{Name: "desktop-control-proof", Version: "1"},
		&mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}},
	)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	catalog, err := session.ListTools(ctx, nil)
	if err != nil || len(catalog.Tools) != 29 {
		t.Fatalf("tool catalog: %v %+v", err, catalog)
	}
	resources, err := session.ListResources(ctx, nil)
	if err != nil || len(resources.Resources) != 2 {
		t.Fatalf("resource catalog: %v %+v", err, resources)
	}
	guide, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "knotra://guide"})
	if err != nil || len(guide.Contents) != 1 || !strings.Contains(guide.Contents[0].Text, "expectedRevision") {
		t.Fatalf("guide: %v %+v", err, guide)
	}
	schema, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "knotra://schema/v1"})
	if err != nil || len(schema.Contents) != 1 || !json.Valid([]byte(schema.Contents[0].Text)) {
		t.Fatalf("schema resource: %v %+v", err, schema)
	}
	call := func(name string, args map[string]any, wantError bool) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		encoded, _ := json.Marshal(result)
		if err != nil ||
			result.IsError != wantError ||
			strings.Contains(string(encoded), fixtureDesktopToken) ||
			strings.Contains(string(encoded), "private-provider-key") {
			t.Fatalf("%s: err=%v result=%s", name, err, encoded)
		}
		return result
	}
	call("pipeline_create", map[string]any{"source": minimalPipeline, "requestId": "create-pipeline-001"}, false)
	call("pipeline_list", map[string]any{}, false)
	read := call("pipeline_get", map[string]any{"id": "workspace-fixture"}, false)
	encoded, _ := json.Marshal(read.StructuredContent)
	if !strings.Contains(string(encoded), "revision-a") {
		t.Fatalf("get lost its revision: %s", encoded)
	}
	call(
		"pipeline_update",
		map[string]any{
			"id":               "workspace-fixture",
			"expectedRevision": "revision-a",
			"source":           minimalPipeline + "\n",
			"requestId":        "update-pipeline-001",
		},
		false,
	)
	call(
		"pipeline_update",
		map[string]any{
			"id":               "workspace-fixture",
			"expectedRevision": "revision-a",
			"source":           minimalPipeline,
			"requestId":        "update-pipeline-stale",
		},
		true,
	)
	newSource := map[string]any{"provider": "openai", "apiKey": "private-provider-key", "requestId": "create-source-001"}
	call("model_source_save", newSource, false)
	call("model_source_save", newSource, false)
	call("model_source_list", map[string]any{}, false)
	call("engine_call", map[string]any{"op": "resources"}, false)
	call(
		"engine_call",
		map[string]any{
			"op":          "respond",
			"requestId":   "human-request-fixture",
			"outputs":     map[string]any{"approved": true},
			"operationId": "durable-response-operation",
		},
		false,
	)
	mu.Lock()
	before := requests
	mu.Unlock()
	call("engine_call", map[string]any{"op": "arbitrary_http", "url": "https://example.test"}, true)
	call("engine_call", map[string]any{"op": "resources", "url": "https://example.test"}, true)
	call("model_source_save", map[string]any{"id": "source-fixture", "provider": "openai", "requestId": "edit-source-no-revision"}, true)
	call("pipeline_create", map[string]any{"source": minimalPipeline}, true)
	mu.Lock()
	if requests != before || sourceCreates != 1 {
		t.Errorf("invalid commands reached bridge or retry duplicated source: requests=%d before=%d creates=%d", requests, before, sourceCreates)
	}
	mu.Unlock()
	call("pipeline_delete", map[string]any{"id": "workspace-fixture", "expectedRevision": "revision-b", "requestId": "delete-pipeline-001"}, false)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("Built CLI child completed MCP initialize, tool/resource discovery, schema/guide reads, authenticated pipeline CRUD/CAS, source creation replay/list and closed engine/respond calls over actual stdio")
}
