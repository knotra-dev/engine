package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knotra-dev/engine/internal/contract"
)

func saveConnections(t *testing.T, s *Setup, connections Connections) {
	t.Helper()
	s.Config.ConnectionsFile = filepath.Join(s.Config.Dir, "sources.json")
	data, err := json.Marshal(connections)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Config.ConnectionsFile, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func sourceFixture() Connections {
	return Connections{Models: map[string]contract.ModelConnection{
		"api_one": {Provider: "openai", Model: "first-model", Auth: map[string]contract.Credential{"key": {SecretRef: "key_one"}}},
		"api_two": {Provider: "openai", Model: "second-model", Auth: map[string]contract.Credential{"key": {SecretRef: "key_two"}}},
		"codex":   {Provider: "codex", Model: "default"},
		"claude":  {Provider: "claude", Model: "sonnet"},
	}, Secrets: map[string]contract.SecretSource{"key_one": {Env: "KNOTRA_MODEL_KEY_ONE"}, "key_two": {Env: "KNOTRA_MODEL_KEY_TWO"}}}
}

func TestSavedSourcesPreserveExistingProfileAndRunIdentity(t *testing.T) {
	s := testSetup(t)
	first, err := s.WriteOnce()
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(s.Config.Dir, "profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	connections := sourceFixture()
	saveConnections(t, s, connections)
	s.Config.Provider, s.Config.Model, s.Config.BaseURL = "", "", ""
	second, err := s.WriteOnce()
	if err != nil {
		t.Fatal(err)
	}
	if first.Password != second.Password || first.OperationID != second.OperationID {
		t.Fatal("model sources changed durable run identity")
	}
	path, err := s.ProfilePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profile, diagnostics := contract.ParseProfile(data)
	if contract.HasErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	if len(profile.Spec.Models) != 5 || profile.Spec.Models["api_one"].Auth["key"].SecretRef == profile.Spec.Models["api_two"].Auth["key"].SecretRef {
		t.Fatal("saved sources or independent credentials were lost")
	}
	if len(profile.Spec.Sandboxes) != 2 || profile.Spec.Models["model_main"].Provider != "openai" {
		t.Fatal("legacy profile configuration was lost")
	}
	actual, err := os.ReadFile(filepath.Join(s.Config.Dir, "profile.json"))
	if err != nil || !bytes.Equal(original, actual) {
		t.Fatal("editable profile was rewritten")
	}
	if bytes.Contains(data, []byte("fixture-key")) {
		t.Fatal("API key was written to the managed profile")
	}
	delete(connections.Models, "claude")
	saveConnections(t, s, connections)
	if _, err = s.WriteOnce(); err != nil {
		t.Fatal(err)
	}
	path, err = s.ProfilePath()
	if err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	profile, _ = contract.ParseProfile(data)
	if _, exists := profile.Spec.Models["claude"]; exists {
		t.Fatal("removed managed source was restored")
	}
}

func TestManagedDoctorDoesNotRequireOrPullIndividualModels(t *testing.T) {
	s := testSetup(t)
	s.Config.Provider = "ollama"
	saveConnections(t, s, sourceFixture())
	s.Getenv = func(string) string { return "" }
	helper := make([]byte, 20)
	copy(helper, "\x7fELF")
	helper[18] = 183
	s.Config.HelperPath = filepath.Join(s.Config.Dir, "helper")
	if err := os.WriteFile(s.Config.HelperPath, helper, 0600); err != nil {
		t.Fatal(err)
	}
	s.Command = func(_ context.Context, command string, args, _ []string) ([]byte, error) {
		if command != "docker" {
			return nil, fmt.Errorf("unexpected infrastructure command %s", command)
		}
		if strings.Join(args, " ") == "info --format {{.OSType}} {{.Architecture}}" {
			return []byte("linux aarch64"), nil
		}
		if strings.Join(args, " ") == "compose version" || (len(args) > 1 && (args[0] == "compose" || args[0] == "image")) {
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected infrastructure arguments %v", args)
	}
	if err := s.Doctor(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.StartInfrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagedSourceConfigurationRejectsInlineKeysAndMalformedFields(t *testing.T) {
	for _, input := range []string{
		`{"models":{"api":{"provider":"openai","model":"m","auth":{"key":{"value":"private-key"}}}}}`,
		`{"models":{"api":{"provider":"openai","model":"m","base_url":"https://example.test"}}}`,
		`{"models":{"api":{"provider":"unknown","model":"m"}}}`,
		`{"models":{"api":{"provider":"openai","model":"m","auth":{"key":{"secretRef":"missing"}}}}}`,
		`{"mcp":{"remote":{"transport":"streamable_http","url":"https://mcp.example","allowedTools":[],"headers":{"Authorization":{"value":"private-key"}}}}}`,
		`{"mcp":{"remote":{"transport":"streamable_http","url":"https://mcp.example","allowedTools":[],"headers":{"Authorization":{"secretRef":"missing"}}}}}`,
		`{"mcp":{"local":{"transport":"stdio","sandbox":"node_box","command":["node","server.js"],"allowedTools":[],"env":{"API_KEY":{"value":"private-key"}}}}}`,
		`{"mcp":{"remote":{"transport":"unsupported","url":"https://mcp.example","allowedTools":[]}}}`,
	} {
		t.Run(input, func(t *testing.T) {
			s := testSetup(t)
			s.Config.ConnectionsFile = filepath.Join(s.Config.Dir, "sources.json")
			if err := os.WriteFile(s.Config.ConnectionsFile, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.WriteOnce(); err == nil || strings.Contains(err.Error(), "private-key") {
				t.Fatalf("invalid source accepted or credential exposed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(s.Config.Dir, "settings.json")); !os.IsNotExist(err) {
				t.Fatal("invalid source created infrastructure settings")
			}
		})
	}
}

func TestManagedMCPOverlaysAndRemovalPreserveEditableProfile(t *testing.T) {
	s := testSetup(t)
	if _, err := s.WriteOnce(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Config.Dir, "profile.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := contract.ParseProfile(data)
	profile.Spec.MCP = map[string]contract.MCPConnection{
		"manual_remote": {Transport: "streamable_http", URL: "https://manual.example/mcp", AllowedTools: []string{"search"}},
		"mcp_retired":   {Transport: "streamable_http", URL: "https://retired.example/mcp", AllowedTools: []string{"search"}},
	}
	original, err := checkedProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	connections := sourceFixture()
	connections.MCP = map[string]contract.MCPConnection{
		"mcp_docs": {
			Transport:    "streamable_http",
			URL:          "https://docs.example/mcp",
			Headers:      map[string]contract.Credential{"Authorization": {SecretRef: "mcp_docs_auth"}},
			AllowedTools: []string{"search"},
		},
		"mcp_box": {Transport: "stdio", Sandbox: "node_box", Command: []string{"node", "/workspace/server.js"}, AllowedTools: []string{}},
	}
	connections.Secrets["mcp_docs_auth"] = contract.SecretSource{Env: "KNOTRA_MCP_DOCS_AUTH"}
	connections.RemoveMCP = []string{"mcp_retired"}
	saveConnections(t, s, connections)
	if _, err := s.WriteOnce(); err != nil {
		t.Fatal(err)
	}
	managedPath, err := s.ProfilePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	managed, diagnostics := contract.ParseProfile(data)
	if contract.HasErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	if len(managed.Spec.MCP) != 3 ||
		managed.Spec.MCP["manual_remote"].URL != profile.Spec.MCP["manual_remote"].URL ||
		managed.Spec.MCP["mcp_docs"].Headers["Authorization"].SecretRef != "mcp_docs_auth" ||
		managed.Spec.MCP["mcp_box"].Transport != "stdio" {
		t.Fatalf("MCP overlay lost configuration: %+v", managed.Spec.MCP)
	}
	if _, ok := managed.Spec.MCP["mcp_retired"]; ok {
		t.Fatal("retired desktop MCP source was restored")
	}
	if len(managed.Spec.Models) != 5 ||
		len(managed.Spec.Sandboxes) != 2 ||
		managed.Spec.Secrets["mcp_docs_auth"].Env != "KNOTRA_MCP_DOCS_AUTH" ||
		bytes.Contains(data, []byte("fixture-key")) {
		t.Fatal("MCP overlay changed unrelated resources or serialized credentials")
	}
	delete(connections.MCP, "mcp_docs")
	saveConnections(t, s, connections)
	if _, err := s.WriteOnce(); err != nil {
		t.Fatal(err)
	}
	managedPath, err = s.ProfilePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	managed, _ = contract.ParseProfile(data)
	if len(managed.Spec.MCP) != 2 {
		t.Fatal("removed managed MCP source survived in the generated profile")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, actual) {
		t.Fatal("MCP settings rewrote the editable engine profile")
	}
}

func TestUnavailableSourcesStillRestoreEngineHistoryWithoutLegacyModel(t *testing.T) {
	s := testSetup(t)
	if _, err := s.WriteOnce(); err != nil {
		t.Fatal(err)
	}
	saveConnections(t, s, Connections{Models: map[string]contract.ModelConnection{}, Remove: []string{"model_main"}})
	if _, err := s.WriteOnce(); err != nil {
		t.Fatal(err)
	}
	path, err := s.ProfilePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profile, diagnostics := contract.ParseProfile(data)
	if contract.HasErrors(diagnostics) || len(profile.Spec.Models) != 0 {
		t.Fatalf("unavailable sources prevented an empty model profile: %v", diagnostics)
	}
}
