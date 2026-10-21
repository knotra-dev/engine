package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/knotra-dev/engine/internal/bootstrap"
	"github.com/knotra-dev/engine/internal/cli"
	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store"
)

func TestManagedMCPDiscoveryManifestAndToolExecution(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_DATABASE_URL for managed MCP acceptance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	const envName = "KNOTRA_TEST_MANAGED_MCP_AUTH"
	const token = "Bearer managed-mcp-fixture-key"
	t.Setenv(envName, token)
	var calls, contentCalls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "managed-mcp", Version: "1"}, nil)
	type query struct {
		Query string `json:"query"`
	}
	type result struct {
		Text string `json:"text"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "search", Title: "Search documents", Description: "Find a document by topic"},
		func(_ context.Context, _ *mcp.CallToolRequest, input query) (*mcp.CallToolResult, result, error) {
			calls.Add(1)
			return nil, result{Text: "Found: " + input.Query}, nil
		})
	server.AddTool(&mcp.Tool{Name: "read_text", Title: "Read text", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			contentCalls.Add(1)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "A plain text MCP result"}}}, nil
		})
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != token {
			t.Error("managed MCP request lost its environment credential")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		transport.ServeHTTP(w, r)
	}))
	defer remote.Close()
	connection := contract.MCPConnection{Transport: "streamable_http", URL: remote.URL, Headers: map[string]contract.Credential{"Authorization": {SecretRef: "mcp_fixture_auth"}}, AllowedTools: []string{}}
	secrets := map[string]contract.SecretSource{"mcp_fixture_auth": {Env: envName}}
	input, err := json.Marshal(map[string]any{"url": remote.URL, "headers": connection.Headers, "secrets": secrets})
	if err != nil {
		t.Fatal(err)
	}
	var output, stderr bytes.Buffer
	if err := cli.Execute(ctx, []string{"mcp", "discover", "--json"}, cli.Options{In: bytes.NewReader(input), Out: &output, Err: &stderr, Getenv: os.Getenv}); err != nil {
		t.Fatalf("discover: %v %s", err, &stderr)
	}
	var catalog struct {
		Tools []struct {
			Name, Title  string
			OutputSchema json.RawMessage
		}
	}
	if json.Unmarshal(output.Bytes(), &catalog) != nil || len(catalog.Tools) != 2 || catalog.Tools[0].Name != "read_text" || len(catalog.Tools[0].OutputSchema) != 0 || catalog.Tools[1].Name != "search" || catalog.Tools[1].Title != "Search documents" || calls.Load() != 0 || contentCalls.Load() != 0 {
		t.Fatalf("discovery called a tool or lost its catalog: %s calls=%d", &output, calls.Load())
	}
	connection.AllowedTools = []string{catalog.Tools[0].Name, catalog.Tools[1].Name}
	directory := t.TempDir()
	manifest := filepath.Join(directory, "connections.json")
	writeJSON(t, manifest, bootstrap.Connections{Models: map[string]contract.ModelConnection{}, MCP: map[string]contract.MCPConnection{"mcp_fixture": connection}, Secrets: secrets, Remove: []string{"model_main"}})
	setup := &bootstrap.Setup{Config: bootstrap.Config{Dir: directory, ConnectionsFile: manifest, PostgresPort: 25432}}
	if _, err := setup.WriteOnce(); err != nil {
		t.Fatal(err)
	}
	managedPath, err := setup.ProfilePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	profile, diagnostics := contract.ParseProfile(data)
	if contract.HasErrors(diagnostics) || bytes.Contains(data, []byte(token)) || len(profile.Spec.Models) != 0 {
		t.Fatalf("managed profile invalid or exposed credentials: %v", diagnostics)
	}
	options, api, _ := startRiverService(t, ctx, profile)
	const source = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: managed-mcp}
spec:
  mcp: {docs: {connection: mcp_fixture}}
  nodes:
    review:
      type: human
      human: {prompt: {text: "Run the selected MCP tool?"}}
      outputs: {approved: {schema: {type: boolean}}}
    lookup:
      type: tool
      needs: [review]
      tool:
        server: docs
        name: search
        arguments: {value: {query: Knotra}}
        response: structured
      outputs: {result: {schema: {type: object, additionalProperties: true}}}
  outputs:
    result: {schema: {type: object, additionalProperties: true}, bind: {from: nodes.lookup.outputs.result}}
`
	pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
	var validation struct {
		Valid       bool
		Diagnostics []contract.Diagnostic
	}
	if err := api.Validate(ctx, map[string]any{"package": pkg, "profile": "local"}, &validation); err != nil || !validation.Valid || calls.Load() != 0 {
		t.Fatalf("validation: valid=%v diagnostics=%v err=%v calls=%d", validation.Valid, validation.Diagnostics, err, calls.Load())
	}
	var definition struct{ Definition protocol.Definition }
	if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
		t.Fatal(err)
	}
	var accepted struct{ Run protocol.Run }
	if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": "local"}, uuid.NewString(), &accepted); err != nil {
		t.Fatal(err)
	}
	request := awaitHuman(t, ctx, api, accepted.Run.ID)
	if calls.Load() != 0 {
		t.Fatal("tool executed before the human answer")
	}
	var replacements atomic.Int32
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		replacements.Add(1)
		http.Error(w, "replacement must not serve the admitted run", http.StatusServiceUnavailable)
	}))
	defer replacement.Close()
	next := connection
	next.URL = replacement.URL
	next.Headers = map[string]contract.Credential{"Authorization": {SecretRef: "mcp_fixture_next_auth"}}
	writeJSON(t, manifest, bootstrap.Connections{Models: map[string]contract.ModelConnection{}, MCP: map[string]contract.MCPConnection{"mcp_fixture": next}, Secrets: map[string]contract.SecretSource{"mcp_fixture_next_auth": {Env: "KNOTRA_TEST_MCP_NEXT_AUTH"}}, Remove: []string{"model_main"}})
	if _, err := setup.WriteOnce(); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.ProfilePath(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, options.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pinned, err := db.Plan(ctx, accepted.Run.ID)
	if err != nil || pinned.Profile.Spec.MCP["mcp_fixture"].URL != remote.URL || pinned.Profile.Spec.MCP["mcp_fixture"].Headers["Authorization"].SecretRef != "mcp_fixture_auth" || pinned.Profile.Spec.Secrets["mcp_fixture_auth"].Env != envName || pinned.MCPTools["mcp_fixture"]["search"].Title != "Search documents" {
		t.Fatalf("unfinished run lost its admitted MCP configuration: %v %v", pinned, err)
	}
	var response any
	if err := api.Command(ctx, "/requests/"+request.ID+"/response", map[string]any{"outputs": map[string]any{"approved": true}}, uuid.NewString(), &response); err != nil {
		t.Fatal(err)
	}
	final := awaitTerminal(t, ctx, api, accepted.Run.ID)
	if final.Status != "succeeded" || calls.Load() != 1 || replacements.Load() != 0 || !strings.Contains(string(final.Outputs["result"]), "Found: Knotra") {
		t.Fatalf("managed MCP did not execute the chosen tool once: %+v calls=%d", final, calls.Load())
	}
	var tools, completed int
	if err := db.Pool.QueryRow(ctx, `SELECT
		COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool'),0),
		(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind='tool' AND completed)`, final.ID).Scan(&tools, &completed); err != nil || tools != 1 || completed != 1 {
		t.Fatalf("managed MCP execution evidence: tools=%d completed=%d err=%v", tools, completed, err)
	}
	const contentSource = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: managed-mcp-content}
spec:
  mcp: {docs: {connection: mcp_fixture}}
  nodes:
    read:
      type: tool
      tool:
        server: docs
        name: read_text
        arguments: {value: {}}
        response: content
      outputs: {result: {schema: {type: array}}}
  outputs:
    result: {schema: {type: array}, bind: {from: nodes.read.outputs.result}}
`
	contentPackage := contract.Package{Entrypoint: "pipeline.yaml", Source: contentSource, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(contentSource)}}}
	if err := api.Command(ctx, "/definitions", map[string]any{"package": contentPackage}, uuid.NewString(), &definition); err != nil {
		t.Fatal(err)
	}
	if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": "local"}, uuid.NewString(), &accepted); err != nil {
		t.Fatal(err)
	}
	contentFinal := awaitTerminal(t, ctx, api, accepted.Run.ID)
	var contents []struct{ Type, Text string }
	if contentFinal.Status != "succeeded" || contentCalls.Load() != 1 || calls.Load() != 1 || json.Unmarshal(contentFinal.Outputs["result"], &contents) != nil || len(contents) != 1 || contents[0].Type != "text" || contents[0].Text != "A plain text MCP result" {
		t.Fatalf("content-only MCP result did not match its array port: %+v calls=%d", contentFinal, contentCalls.Load())
	}
	if err := db.Pool.QueryRow(ctx, `SELECT
		COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool'),0),
		(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind='tool' AND completed)`, contentFinal.ID).Scan(&tools, &completed); err != nil || tools != 1 || completed != 1 {
		t.Fatalf("content MCP execution evidence: tools=%d completed=%d err=%v", tools, completed, err)
	}
	t.Log("HTTP discovery, managed profile overlay, API admission and selected MCP tool execution passed; a pending manifest replacement preserved the unfinished run's endpoint and credential reference")
	t.Log("A content-only MCP tool without outputSchema or structuredContent executed once and returned a text-content array through its array result port")
}
