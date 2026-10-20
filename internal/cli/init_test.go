package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knotra-dev/engine/examples/starter"
)

func invokeInit(t *testing.T, jsonOutput bool, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	s := &commandState{options: Options{Out: &out}, json: jsonOutput}
	cmd := s.initCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestInitCreatesOfflineStarter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my pipeline's $HOME", "nested")
	out, err := invokeInit(t, false, dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pipeline.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := starter.Files.ReadFile("hello/pipeline.yaml")
	if err != nil || !bytes.Equal(data, want) {
		t.Fatalf("starter does not match the embedded example: %v", err)
	}
	quoted := "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"
	if !strings.Contains(out, "knotra validate "+quoted) || !strings.Contains(out, "knotra run "+quoted+" --profile local --wait") {
		t.Fatalf("missing safely quoted next steps: %s", out)
	}
	out, _, err = invoke(t, []string{"validate", path, "--json"}, "")
	if err != nil || !strings.Contains(out, `"valid":true`) {
		t.Fatalf("scaffold failed offline validation: %s %v", out, err)
	}
}

func TestInitDefaultsToCurrentDirectoryAndEmitsJSON(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	out, err := invokeInit(t, true)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Path string `json:"path"`
	}
	if err = json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON: %s %v", out, err)
	}
	want, err := filepath.Abs("pipeline.yaml")
	if err != nil || result.Path != want {
		t.Fatalf("path=%q want=%q err=%v", result.Path, want, err)
	}
	if _, err = os.Stat(result.Path); err != nil {
		t.Fatal(err)
	}
}

func TestInitDoesNotOverwriteExistingFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yaml")
	data := []byte("my existing pipeline\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := invokeInit(t, false, dir)
	if err == nil || !strings.Contains(err.Error(), "already exists") || out != "" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("existing pipeline changed: %q %v", got, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("temporary files left behind: %v %v", files, err)
	}
}
