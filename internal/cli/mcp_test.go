package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type discoveryTraffic struct {
	initializations atomic.Int32
	lists           atomic.Int32
	deletions       atomic.Int32
	calls           atomic.Int32
}

func discoveryRemote(t *testing.T, sse bool, intercept func(http.ResponseWriter, *http.Request, string, json.RawMessage) bool) (*httptest.Server, *discoveryTraffic) {
	t.Helper()
	traffic := &discoveryTraffic{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-discovery-key" {
			t.Error("MCP credential was not forwarded")
		}
		if r.Method == http.MethodDelete {
			traffic.deletions.Add(1)
			if r.Header.Get("Mcp-Session-Id") != "discovery-session" || r.Header.Get("Mcp-Protocol-Version") == "" {
				t.Error("MCP cleanup lost negotiated session identity")
			}
			if intercept != nil && intercept(w, r, "DELETE", nil) {
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected discovery request: %s", r.Method)
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		var message struct {
			Method string
			ID     json.RawMessage
			Params struct {
				ProtocolVersion string
				Cursor          string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Error(err)
			return
		}
		switch message.Method {
		case "server/discover":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, message.ID)
			return
		case "initialize":
			traffic.initializations.Add(1)
		case "tools/list":
			traffic.lists.Add(1)
		case "tools/call":
			traffic.calls.Add(1)
		}
		if intercept != nil && intercept(w, r, message.Method, message.ID) {
			return
		}
		var result any
		switch message.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "discovery-session")
			result = map[string]any{"protocolVersion": message.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "discovery-session" {
				t.Error("catalog lost initialized session")
			}
			if message.Params.Cursor == "next" {
				result = map[string]any{"tools": []any{map[string]any{"name": "alpha", "annotations": map[string]any{"title": "Readable alpha"}, "inputSchema": map[string]any{"type": "object"}}}}
			} else {
				result = map[string]any{"tools": []any{map[string]any{"name": "zulu", "title": "Readable zulu", "description": "Find a document", "inputSchema": map[string]any{"type": "object"}, "outputSchema": map[string]any{"type": "object"}}}, "nextCursor": "next"}
			}
		default:
			t.Errorf("unexpected discovery method %s", message.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
		if err != nil {
			t.Error(err)
			return
		}
		if sse && message.Method == "tools/list" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
		}
	}))
	t.Cleanup(remote.Close)
	return remote, traffic
}

func discoveryInput(endpoint string) string {
	return fmt.Sprintf(`{"url":%q,"headers":{"Authorization":{"secretRef":"mcp_auth"}},"secrets":{"mcp_auth":{"env":"KNOTRA_MCP_KEY"}}}`, endpoint)
}

func invokeDiscovery(t *testing.T, ctx context.Context, input string) (string, string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	err := Execute(ctx, []string{"mcp", "discover", "--json"}, Options{
		In: strings.NewReader(input), Out: &out, Err: &stderr,
		Getenv: func(name string) string {
			if name == "KNOTRA_MCP_KEY" {
				return "Bearer private-discovery-key"
			}
			return ""
		},
	})
	return out.String(), stderr.String(), err
}

func TestMCPDiscoveryListsPaginatedToolsAndClosesSession(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprint(sse), func(t *testing.T) {
			remote, traffic := discoveryRemote(t, sse, nil)
			out, stderr, err := invokeDiscovery(t, t.Context(), discoveryInput(remote.URL))
			if err != nil || stderr != "" || !json.Valid([]byte(out)) {
				t.Fatalf("out=%s stderr=%s err=%v", out, stderr, err)
			}
			var catalog struct {
				Tools []struct {
					Name, Title, Description  string
					InputSchema, OutputSchema json.RawMessage
				}
			}
			if err := json.Unmarshal([]byte(out), &catalog); err != nil {
				t.Fatal(err)
			}
			if len(catalog.Tools) != 2 || catalog.Tools[0].Name != "alpha" || catalog.Tools[0].Title != "Readable alpha" || len(catalog.Tools[0].OutputSchema) != 0 || catalog.Tools[1].Title != "Readable zulu" || catalog.Tools[1].Description != "Find a document" || !json.Valid(catalog.Tools[1].OutputSchema) {
				t.Fatalf("invalid catalog: %s", out)
			}
			if traffic.initializations.Load() != 1 || traffic.lists.Load() != 2 || traffic.deletions.Load() != 1 || traffic.calls.Load() != 0 || strings.Contains(out, "private-discovery-key") {
				t.Fatalf("unsafe discovery: %s %+v", out, traffic)
			}
		})
	}
}

func TestMCPDiscoveryRejectsInvalidConfigurationBeforeNetwork(t *testing.T) {
	for _, input := range []string{
		`null`, `[]`, `{}`, `{"url":"http://remote.example/mcp"}`,
		`{"url":"https://user:private-discovery-key@remote.example/mcp"}`,
		`{"url":"https://remote.example/mcp?token=private-discovery-key"}`,
		`{"url":"https://remote.example/mcp?"}`, `{"url":"https://remote.example/mcp#"}`,
		`{"url":"https://remote.example/mcp","url":"https://other.example"}`,
		`{"url":"https://remote.example/mcp","token":"private-discovery-key"}`,
		`{"url":"https://remote.example/mcp","headers":{"Authorization":{"value":"private-discovery-key"}}}`,
		`{"url":"https://remote.example/mcp","headers":{"Authorization":{"secretRef":"missing"}}}`,
		`{"url":"https://remote.example/mcp","headers":{"Mcp-Session-Id":{"secretRef":"auth"}},"secrets":{"auth":{"env":"KNOTRA_MCP_KEY"}}}`,
		`{"url":"https://remote.example/mcp","headers":{"Authorization":{"secretRef":"auth"}},"secrets":{"auth":{"env":"MISSING_MCP_KEY"}}}`,
		strings.Repeat("x", 64<<10+1),
	} {
		out, stderr, err := invokeDiscovery(t, t.Context(), input)
		if err == nil || out != "" || !json.Valid([]byte(stderr)) || strings.Contains(stderr, "private-discovery-key") {
			t.Fatalf("invalid config: out=%s stderr=%s err=%v", out, stderr, err)
		}
	}
}

func TestMCPDiscoveryDoesNotFollowRedirectsOrEchoRemoteErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusFound, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var redirects atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirects.Add(1) }))
			defer target.Close()
			remote, _ := discoveryRemote(t, false, func(w http.ResponseWriter, _ *http.Request, method string, _ json.RawMessage) bool {
				if method != "initialize" {
					return false
				}
				w.Header().Set("Location", target.URL+"/private-discovery-key")
				http.Error(w, "private-discovery-key", status)
				return true
			})
			out, stderr, err := invokeDiscovery(t, t.Context(), discoveryInput(remote.URL))
			if err == nil || out != "" || !json.Valid([]byte(stderr)) || strings.Contains(stderr, "private-discovery-key") || redirects.Load() != 0 {
				t.Fatalf("out=%s stderr=%s err=%v redirects=%d", out, stderr, err, redirects.Load())
			}
		})
	}
}

func TestMCPDiscoveryCatalogFailuresStillCloseSession(t *testing.T) {
	for _, tools := range []string{
		`[{"name":"bad/name","inputSchema":{"type":"object"}}]`,
		`[{"name":"same","inputSchema":{}},{"name":"same","inputSchema":{}}]`,
		`[{"name":"missing_schema","inputSchema":null}]`,
		`[{"name":"invalid_output","inputSchema":{},"outputSchema":true}]`,
	} {
		remote, traffic := discoveryRemote(t, false, func(w http.ResponseWriter, _ *http.Request, method string, id json.RawMessage) bool {
			if method != "tools/list" {
				return false
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":%s}}`, id, tools)
			return true
		})
		out, stderr, err := invokeDiscovery(t, t.Context(), discoveryInput(remote.URL))
		if err == nil || out != "" || !json.Valid([]byte(stderr)) || traffic.deletions.Load() != 1 || traffic.calls.Load() != 0 {
			t.Fatalf("out=%s stderr=%s err=%v traffic=%+v", out, stderr, err, traffic)
		}
	}
}

func TestMCPDiscoveryCancellationAndSessionErrors(t *testing.T) {
	for _, failure := range []string{"cancel", "unknown session", "cleanup", "tool error"} {
		t.Run(failure, func(t *testing.T) {
			remote, traffic := discoveryRemote(t, false, func(w http.ResponseWriter, r *http.Request, method string, id json.RawMessage) bool {
				if failure == "cleanup" && method == "DELETE" {
					http.Error(w, "private-discovery-key", http.StatusServiceUnavailable)
					return true
				}
				if method != "tools/list" {
					return false
				}
				switch failure {
				case "cancel":
					<-r.Context().Done()
				case "unknown session":
					http.Error(w, "private-discovery-key", http.StatusNotFound)
				case "tool error":
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"private-discovery-key"}}`, id)
				default:
					return false
				}
				return true
			})
			ctx := t.Context()
			if failure == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			out, stderr, err := invokeDiscovery(t, ctx, discoveryInput(remote.URL))
			wantDeletes := int32(1)
			if failure == "unknown session" {
				wantDeletes = 0
			}
			if err == nil || out != "" || !json.Valid([]byte(stderr)) || strings.Contains(stderr, "private-discovery-key") || traffic.initializations.Load() != 1 || traffic.deletions.Load() < wantDeletes || traffic.calls.Load() != 0 {
				t.Fatalf("out=%s stderr=%s err=%v traffic=%+v", out, stderr, err, traffic)
			}
		})
	}
}
