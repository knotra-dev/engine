package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/execution"
)

type discoveryMessage struct {
	Method string
	ID     json.RawMessage
}

type discoveryCleanupRemote struct {
	initializations atomic.Int32
	deleteStatus    atomic.Int32
	mu              sync.Mutex
	deletions       []http.Header
}

func newDiscoveryCleanupRemote(t *testing.T, intercept func(http.ResponseWriter, *http.Request, discoveryMessage) bool) (*httptest.Server, *discoveryCleanupRemote) {
	t.Helper()
	traffic := &discoveryCleanupRemote{}
	traffic.deleteStatus.Store(http.StatusServiceUnavailable)
	server := mcp.NewServer(&mcp.Implementation{Name: "discovery-cleanup", Version: "1"}, nil)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{SessionTimeout: time.Second})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			traffic.mu.Lock()
			traffic.deletions = append(traffic.deletions, req.Header.Clone())
			traffic.mu.Unlock()
			status := int(traffic.deleteStatus.Load())
			if status != http.StatusOK {
				if status == http.StatusNotFound {
					transport.ServeHTTP(httptest.NewRecorder(), req)
				}
				w.WriteHeader(status)
				return
			}
		}
		if req.Method == http.MethodPost {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Error(err)
				return
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
			var message discoveryMessage
			if err := json.Unmarshal(body, &message); err != nil {
				t.Error(err)
				return
			}
			if message.Method == "initialize" {
				traffic.initializations.Add(1)
			}
			if intercept != nil && intercept(w, req, message) {
				return
			}
		}
		transport.ServeHTTP(w, req)
	}))
	t.Cleanup(remote.Close)
	return remote, traffic
}

func discoveryRequest(endpoint string) Request {
	req := testRequest("")
	pipeline(req).Spec.Models = nil
	pipeline(req).Spec.Sandboxes = nil
	credential := "original-discovery-key"
	req.Plan.Profile.Spec.MCP = map[string]contract.MCPConnection{"local": {
		Transport: "streamable_http", URL: endpoint,
		Headers: map[string]contract.Credential{"X-Cleanup-Key": {Value: &credential}},
	}}
	pipeline(req).Spec.MCP = map[string]contract.MCPResource{"tools": {Connection: "local"}}
	return req
}

func assertDiscoveryCleanup(t *testing.T, runner *Runner, traffic *discoveryCleanupRemote, wantDeletes int) {
	t.Helper()
	traffic.mu.Lock()
	defer traffic.mu.Unlock()
	if traffic.initializations.Load() != 1 || len(traffic.deletions) != wantDeletes {
		t.Fatalf("cleanup replaced or lost session: initializations=%d deletes=%d", traffic.initializations.Load(), len(traffic.deletions))
	}
	original := traffic.deletions[0]
	if original.Get("Mcp-Session-Id") == "" || original.Get("Mcp-Protocol-Version") == "" {
		t.Fatalf("cleanup lacks negotiated identity: %v", original)
	}
	for _, headers := range traffic.deletions {
		for _, name := range []string{"Mcp-Session-Id", "Mcp-Protocol-Version", "X-Cleanup-Key"} {
			if headers.Get(name) != original.Get(name) {
				t.Fatalf("cleanup changed %s: %v", name, headers)
			}
		}
	}
	if original.Get("X-Cleanup-Key") != "original-discovery-key" {
		t.Fatal("cleanup lost original credential")
	}
	cache := runner.cache()
	cache.discoveryGate <- struct{}{}
	defer func() { <-cache.discoveryGate }()
	if cache.discovery != nil {
		t.Fatal("successful cleanup retained admission bookkeeping")
	}
	if page, err := runner.SessionPage(""); err != nil || len(page) != 0 {
		t.Fatalf("admission cleanup entered run-session inventory: %v, %v", page, err)
	}
}

func TestMCPDiscoveryCleanupRetainsOneSessionUntilClose(t *testing.T) {
	for _, retryStatus := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(http.StatusText(retryStatus), func(t *testing.T) {
			remote, traffic := newDiscoveryCleanupRemote(t, nil)
			runner := &Runner{}
			req := discoveryRequest(remote.URL)
			if err := runner.Prepare(t.Context(), req.Plan); err == nil || !strings.Contains(err.Error(), "cleanup") {
				t.Fatalf("admission discarded DELETE failure: %v", err)
			}
			if _, ok := req.Plan.MCPTools["local"]; ok {
				t.Fatal("failed admission published its capability snapshot")
			}
			cancelled, cancel := context.WithCancel(t.Context())
			cancel()
			if err := runner.Prepare(cancelled, discoveryRequest(remote.URL).Plan); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled admission attempted cleanup: %v", err)
			}
			if err := runner.Close(); err == nil {
				t.Fatal("shutdown discarded repeated DELETE failure")
			}
			for range 8 {
				if err := runner.WithHooks(nil).Prepare(t.Context(), discoveryRequest(remote.URL).Plan); err == nil {
					t.Fatal("new admission ignored pending cleanup")
				}
			}
			if traffic.initializations.Load() != 1 {
				t.Fatal("new admission initialized replacements while cleanup failed")
			}
			traffic.deleteStatus.Store(int32(retryStatus))
			if err := runner.WithHooks(nil).Close(); err != nil {
				t.Fatal(err)
			}
			if err := runner.Close(); err != nil {
				t.Fatal(err)
			}
			assertDiscoveryCleanup(t, runner, traffic, 11)
		})
	}
}

func TestMCPDiscoveryCleanupCoversCatalogFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		field string
		large int
		want  string
	}{
		{"list error", 0, "error", 0, "fixture discovery failed"},
		{"duplicate", 2, "duplicate", 0, "duplicate tool"},
		{"catalog count", 4097, "", 0, "catalog exceeds"},
		{"input size", 1, "inputSchema", 1<<20 + 1, "snapshot limits"},
		{"output size", 1, "outputSchema", 1<<20 + 1, "snapshot limits"},
		{"catalog size", 9, "description", 1 << 20, "snapshot limits"},
		{"allowed count", 1025, "", 0, "snapshot limits"},
		{"idempotency", 1, "idempotency", 0, "idempotency"},
		{"missing tool", 0, "", 0, "not discovered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := make([]map[string]any, tc.count)
			allowed := make([]string, tc.count)
			for i := range tools {
				name := fmt.Sprintf("tool-%d", i)
				allowed[i] = name
				tools[i] = map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}}
				switch tc.field {
				case "duplicate":
					tools[i]["name"] = "tool-0"
				case "description":
					tools[i]["description"] = strings.Repeat("x", tc.large)
				case "inputSchema", "outputSchema":
					tools[i][tc.field] = map[string]any{"type": "object", "description": strings.Repeat("x", tc.large)}
				}
			}
			remote, traffic := newDiscoveryCleanupRemote(t, func(w http.ResponseWriter, _ *http.Request, message discoveryMessage) bool {
				if message.Method != "tools/list" {
					return false
				}
				response := map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": map[string]any{"tools": tools}}
				if tc.field == "error" {
					delete(response, "result")
					response["error"] = map[string]any{"code": -32603, "message": "fixture discovery failed"}
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
				return true
			})
			runner := &Runner{}
			req := discoveryRequest(remote.URL)
			connection := req.Plan.Profile.Spec.MCP["local"]
			connection.AllowedTools = allowed
			if tc.name == "catalog count" {
				connection.AllowedTools = nil
			}
			if tc.name == "missing tool" {
				connection.AllowedTools = []string{"missing"}
			}
			if tc.field == "idempotency" {
				connection.ToolPolicies = map[string]contract.ToolPolicy{"tool-0": {IdempotencyArgument: "/requestId"}}
			}
			req.Plan.Profile.Spec.MCP["local"] = connection
			err := runner.Prepare(t.Context(), req.Plan)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "cleanup") {
				t.Fatalf("lost catalog or cleanup error: %v", err)
			}
			traffic.deleteStatus.Store(http.StatusOK)
			if err := runner.Close(); err != nil {
				t.Fatal(err)
			}
			assertDiscoveryCleanup(t, runner, traffic, 2)
		})
	}
}

func TestMCPDiscoveryCleanupRetainsFailedHandshake(t *testing.T) {
	remote, traffic := newDiscoveryCleanupRemote(t, func(w http.ResponseWriter, _ *http.Request, message discoveryMessage) bool {
		if message.Method != "notifications/initialized" {
			return false
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	runner := &Runner{}
	err := runner.Prepare(t.Context(), discoveryRequest(remote.URL).Plan)
	if err == nil || !strings.Contains(err.Error(), "connection failed") || !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("lost failed-handshake cleanup: %v", err)
	}
	traffic.deleteStatus.Store(http.StatusOK)
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	assertDiscoveryCleanup(t, runner, traffic, 4)
}

func TestMCPDiscoveryWaitIsCancelableAndIndependent(t *testing.T) {
	started, unblock := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(unblock) }) })
	remote, traffic := newDiscoveryCleanupRemote(t, func(_ http.ResponseWriter, req *http.Request, message discoveryMessage) bool {
		if message.Method != "tools/list" {
			return false
		}
		close(started)
		select {
		case <-unblock:
		case <-req.Context().Done():
		}
		return false
	})
	runner := &Runner{}
	t.Cleanup(func() {
		release.Do(func() { close(unblock) })
		_ = runner.Close()
	})
	first := make(chan error, 1)
	go func() { first <- runner.Prepare(t.Context(), discoveryRequest(remote.URL).Plan) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("discovery did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := runner.Prepare(ctx, discoveryRequest(remote.URL).Plan); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending discovery ignored cancellation: %v", err)
	}
	if page, err := runner.SessionPage(""); err != nil || len(page) != 0 {
		t.Fatalf("admission blocked execution-session inventory: %v, %v", page, err)
	}
	fast, fastTraffic := newDiscoveryCleanupRemote(t, nil)
	fastTraffic.deleteStatus.Store(http.StatusOK)
	fastReq := runSessionRequest(fast.URL)
	fastCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if _, _, err := runner.WithHooks(newHooks()).session(fastCtx, fastReq, "tools"); err != nil {
		t.Fatalf("admission blocked execution-session initialization: %v", err)
	}
	if err := runner.ReleaseSession(fastReq.RunID + "/" + fastReq.ScopeID + "/tools"); err != nil {
		t.Fatalf("admission blocked execution-session cleanup: %v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- runner.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("shutdown missed active discovery: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	traffic.deleteStatus.Store(http.StatusOK)
	release.Do(func() { close(unblock) })
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	assertDiscoveryCleanup(t, runner, traffic, 1)
}

func TestMCPDiscoveryCleanupCompletesBeforeNextAdmission(t *testing.T) {
	remote, traffic := newDiscoveryCleanupRemote(t, nil)
	runner := &Runner{}
	if err := runner.Prepare(t.Context(), discoveryRequest(remote.URL).Plan); err == nil {
		t.Fatal("admission accepted failed cleanup")
	}
	traffic.deleteStatus.Store(http.StatusOK)
	req := discoveryRequest(remote.URL)
	if err := runner.Prepare(t.Context(), req.Plan); err != nil {
		t.Fatal(err)
	}
	if _, ok := req.Plan.MCPTools["local"]; !ok {
		t.Fatal("successful cleanup blocked the next snapshot")
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	traffic.mu.Lock()
	defer traffic.mu.Unlock()
	if traffic.initializations.Load() != 2 || len(traffic.deletions) != 3 {
		t.Fatalf("wrong initialization/cleanup traffic: %d, %d", traffic.initializations.Load(), len(traffic.deletions))
	}
	first, retry, next := traffic.deletions[0], traffic.deletions[1], traffic.deletions[2]
	if first.Get("Mcp-Session-Id") != retry.Get("Mcp-Session-Id") || first.Get("Mcp-Session-Id") == next.Get("Mcp-Session-Id") {
		t.Fatal("admission replaced pending session before its deletion")
	}
}

func TestMCPDiscoveryCleanupSurvivesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	remote, traffic := newDiscoveryCleanupRemote(t, func(_ http.ResponseWriter, req *http.Request, message discoveryMessage) bool {
		if message.Method != "tools/list" {
			return false
		}
		cancel()
		<-req.Context().Done()
		return true
	})
	runner := &Runner{}
	err := runner.Prepare(ctx, discoveryRequest(remote.URL).Plan)
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("cancelled discovery lost cleanup failure: %v", err)
	}
	traffic.deleteStatus.Store(http.StatusOK)
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	assertDiscoveryCleanup(t, runner, traffic, 2)
}

func TestMCPDiscoveryFailedHandshakePreservesUnknownResource(t *testing.T) {
	hooks := &resourceCloseHooks{memoryHooks: newHooks()}
	session := &mcpSession{hooks: hooks, resource: execution.ResourceRecord{Ownership: execution.Ownership{WorkerID: "worker"}}}
	if err := session.close(); err != nil {
		t.Fatal(err)
	}
	if hooks.closed {
		t.Fatal("unanswered initialization was treated as a deleted resource")
	}
}

type ownedMCPHandshakeHooks struct {
	*memoryHooks
	resource  execution.ResourceRecord
	recordErr error
	closed    int
}

func (h *ownedMCPHandshakeHooks) RegisterResource(_ context.Context, id, kind, lifetime string) (execution.ResourceRecord, error) {
	h.resource = execution.ResourceRecord{
		ID: id, Kind: kind, Lifetime: lifetime, EngineID: "engine", HostID: "host", State: "active",
		Ownership: execution.Ownership{AttemptID: execution.AttemptID{RunID: "run", InstanceID: "node", Number: 1}, WorkerID: "worker", Generation: 1},
	}
	return h.resource, nil
}

func (h *ownedMCPHandshakeHooks) RecordMCPSession(ctx context.Context, id string, session execution.MCPSessionRecord) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if id != h.resource.ID {
		return errors.New("recorded a different resource")
	}
	if h.recordErr != nil {
		return h.recordErr
	}
	h.resource.MCP = &session
	return nil
}

func (h *ownedMCPHandshakeHooks) CloseResource(context.Context, string) error {
	h.closed++
	return nil
}

func TestMCPOwnedFailedHandshakeKeepsDurableCleanup(t *testing.T) {
	for _, scope := range []string{"node", "run"} {
		for _, evidenceFailure := range []string{"none", "database", "file"} {
			t.Run(scope+"/"+evidenceFailure, func(t *testing.T) {
				remote, traffic := newDiscoveryCleanupRemote(t, func(w http.ResponseWriter, _ *http.Request, message discoveryMessage) bool {
					if message.Method != "notifications/initialized" {
						return false
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					return true
				})
				directory := t.TempDir()
				files, err := execution.OpenOutcomeFiles(directory)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = files.Close() })
				hooks := &ownedMCPHandshakeHooks{memoryHooks: newHooks()}
				if evidenceFailure == "database" {
					hooks.recordErr = errors.New("fixture database unavailable")
				}
				if evidenceFailure == "file" {
					if err := files.Close(); err != nil {
						t.Fatal(err)
					}
				}
				runner := &Runner{Hooks: hooks, EngineID: "engine", HostID: "host", ResourceEvidence: files}
				req := discoveryRequest(remote.URL)
				resource := pipeline(req).Spec.MCP["tools"]
				resource.Session = scope
				pipeline(req).Spec.MCP["tools"] = resource
				connection := req.Plan.Profile.Spec.MCP["local"]
				connection.AllowRunSession = true
				req.Plan.Profile.Spec.MCP["local"] = connection
				session, release, err := runner.session(t.Context(), req, "tools")
				if err == nil || session != nil || release != nil || !strings.Contains(err.Error(), "HTTP 503") {
					t.Fatalf("failed handshake became usable or lost cleanup failure: session=%v release=%t error=%v", session, release != nil, err)
				}
				if hooks.closed != 0 {
					t.Fatal("failed deletion marked durable resource closed")
				}
				if evidenceFailure != "database" && hooks.resource.MCP == nil {
					t.Fatal("failed handshake discarded its database cleanup identity")
				}
				if evidenceFailure == "database" && !strings.Contains(err.Error(), "cannot persist MCP cleanup identity") {
					t.Fatalf("database evidence failure was discarded: %v", err)
				}
				if evidenceFailure == "file" && !strings.Contains(err.Error(), "cannot save MCP cleanup evidence") {
					t.Fatalf("file evidence failure was discarded: %v", err)
				}
				if err := runner.Close(); err != nil {
					t.Fatal(err)
				}
				if page, err := runner.SessionPage(""); err != nil || len(page) != 0 {
					t.Fatalf("broken SDK session entered the usable cache: %v, %v", page, err)
				}
				if scope == "run" {
					if _, _, err := runner.session(t.Context(), req, "tools"); err == nil {
						t.Fatal("failed run handshake silently initialized a replacement")
					}
				}
				// A fresh runner has only the resource journal and persisted files.
				freshFiles, err := execution.OpenOutcomeFiles(directory)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = freshFiles.Close() })
				cleaning := hooks.resource
				cleaning.State = "cleaning"
				if evidenceFailure != "file" {
					cleaning.MCP, err = freshFiles.GetMCPSession(cleaning)
					if err != nil {
						t.Fatalf("known failed-handshake identity was not durable: %v", err)
					}
				}
				traffic.deleteStatus.Store(http.StatusOK)
				fresh := &Runner{EngineID: "engine", HostID: "host", ResourceEvidence: freshFiles}
				if err := fresh.CleanupMCPSession(t.Context(), cleaning, req.Plan.Profile); err != nil {
					t.Fatalf("durable reconciler could not close failed handshake: %v", err)
				}
				assertDiscoveryCleanup(t, runner, traffic, 3)
			})
		}
	}
}

func TestMCPOwnedUnknownHandshakeRemainsOpen(t *testing.T) {
	remote, traffic := newDiscoveryCleanupRemote(t, func(w http.ResponseWriter, _ *http.Request, message discoveryMessage) bool {
		if message.Method != "initialize" {
			return false
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	files, err := execution.OpenOutcomeFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	hooks := &ownedMCPHandshakeHooks{memoryHooks: newHooks()}
	runner := &Runner{Hooks: hooks, EngineID: "engine", HostID: "host", ResourceEvidence: files}
	if session, _, err := runner.session(t.Context(), discoveryRequest(remote.URL), "tools"); err == nil || session != nil {
		t.Fatalf("unanswered initialization became usable: %v", err)
	}
	if hooks.resource.MCP != nil || hooks.closed != 0 {
		t.Fatalf("unanswered initialization acquired a false cleanup identity or closure: %+v, closes=%d", hooks.resource, hooks.closed)
	}
	traffic.mu.Lock()
	defer traffic.mu.Unlock()
	if len(traffic.deletions) != 0 {
		t.Fatal("unknown initialization invented a remote DELETE identity")
	}
}
