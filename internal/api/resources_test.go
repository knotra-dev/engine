package api

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/knotra-dev/engine/internal/contract"
)

func TestResourceCatalogRecognizesCloudProvidersAndRequiresCredentials(t *testing.T) {
	t.Setenv("KNOTRA_CATALOG_KEY", "private-catalog-key")
	profile := contract.Profile{Spec: contract.ProfileSpec{
		Secrets: map[string]contract.SecretSource{"key": {Env: "KNOTRA_CATALOG_KEY"}, "missing": {Env: "KNOTRA_CATALOG_MISSING"}},
		Models: map[string]contract.ModelConnection{
			"local":           {Provider: "ollama"},
			"openai":          {Provider: "openai", Auth: map[string]contract.Credential{"key": {SecretRef: "key"}}},
			"anthropic":       {Provider: "anthropic", Auth: map[string]contract.Credential{"key": {SecretRef: "key"}}},
			"missing":         {Provider: "openai", Auth: map[string]contract.Credential{"key": {SecretRef: "missing"}}},
			"unauthenticated": {Provider: "anthropic"},
			"unknown":         {Provider: "unknown"},
			"compatible":      {Provider: "openai_compatible", BaseURL: "http://127.0.0.1:1234/v1"},
			"no_endpoint":     {Provider: "openai_compatible"},
		},
	}}
	t.Setenv("KNOTRA_CATALOG_MISSING", "")
	response := httptest.NewRecorder()
	(&Server{Profiles: map[string]contract.Profile{"cloud": profile}}).resources(response, httptest.NewRequestWithContext(t.Context(), "GET", "/v1/resources", nil))
	var body struct {
		Items []struct{ ID, Kind, Status string }
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{"local": "available", "openai": "available", "anthropic": "available", "missing": "unavailable", "unauthenticated": "unavailable", "unknown": "unavailable", "compatible": "available", "no_endpoint": "unavailable"}
	seen := map[string]bool{}
	for _, item := range body.Items {
		if item.Kind == "model" {
			seen[item.ID] = true
		}
		if item.Kind == "model" && item.Status != expected[item.ID] {
			t.Errorf("%s: %s", item.ID, item.Status)
		}
	}
	if len(seen) != len(expected) {
		t.Fatal("catalog omitted configured models")
	}
	if strings.Contains(response.Body.String(), "private-catalog-key") {
		t.Fatal("catalog exposed credentials")
	}
}

func TestCLICatalogChecksExecutableWithoutAPIKeys(t *testing.T) {
	dir := t.TempDir()
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	profile := contract.Profile{Spec: contract.ProfileSpec{Models: map[string]contract.ModelConnection{
		"installed": {Provider: "codex", Model: "default"},
		"missing":   {Provider: "claude", Model: "default"},
		"api_auth":  {Provider: "codex", Model: "default", Auth: map[string]contract.Credential{"key": {}}},
	}}}
	response := httptest.NewRecorder()
	(&Server{Profiles: map[string]contract.Profile{"local": profile}}).resources(response, httptest.NewRequestWithContext(t.Context(), "GET", "/v1/resources", nil))
	var body struct{ Items []struct{ ID, Status string } }
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, item := range body.Items {
		want := "unavailable"
		if item.ID == "installed" {
			want = "available"
		}
		if item.Status != want {
			t.Errorf("%s: got %s, want %s", item.ID, item.Status, want)
		}
	}
}
