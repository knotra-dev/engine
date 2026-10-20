package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/protocol"
)

func TestArtifactsListByRun(t *testing.T) {
	for _, artifacts := range []string{`[{"id":"output","name":"report.txt","mediaType":"text/plain","size":12}]`, `null`} {
		t.Run(artifacts, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/runs/run" {
					t.Errorf("unexpected route %s", r.URL.Path)
				}
				_, _ = fmt.Fprintf(w, `{"run":{"id":"run","artifacts":%s}}`, artifacts)
			}))
			defer server.Close()
			out, _, err := invoke(t, []string{"--endpoint", server.URL, "artifacts", "list", "--run", "run", "--json"}, "")
			var result protocol.Page[contract.Artifact]
			if err != nil || json.Unmarshal([]byte(out), &result) != nil || result.Items == nil || result.NextCursor != nil {
				t.Fatalf("run artifact page: %s, %v", out, err)
			}
			if artifacts != "null" && (len(result.Items) != 1 || result.Items[0].ID != "output") {
				t.Fatalf("run outputs missing: %s", out)
			}
		})
	}
}

func TestArtifactsRunRejectsPagination(t *testing.T) {
	for _, flags := range [][]string{{"--all"}, {"--cursor", "next"}} {
		args := append([]string{"artifacts", "list", "--run", "run"}, flags...)
		_, _, err := invoke(t, args, "")
		if err == nil || !strings.Contains(err.Error(), "--run returns all run artifacts") {
			t.Fatalf("silently ignored pagination %v: %v", flags, err)
		}
	}
}

func TestArtifactShowHumanAndJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/artifacts/artifact" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{"artifact":{"id":"artifact","name":"report\u001b[2J.txt","mediaType":"text/plain","size":12,"sha256":"digest"}}`)
	}))
	defer server.Close()
	for _, verb := range []string{"show", "get"} {
		args := []string{"--endpoint", server.URL, "artifacts", verb, "artifact"}
		out, _, err := invoke(t, args, "")
		if err != nil || !strings.Contains(out, "Artifact:") || !strings.Contains(out, "text/plain") || strings.ContainsAny(out, "\x1b{}") {
			t.Fatalf("human output: %q, %v", out, err)
		}
		out, _, err = invoke(t, append(args, "--json"), "")
		var result struct {
			Artifact contract.Artifact `json:"artifact"`
		}
		if err != nil || json.Unmarshal([]byte(out), &result) != nil || result.Artifact.ID != "artifact" || result.Artifact.Name != "report\x1b[2J.txt" {
			t.Fatalf("JSON changed: %q, %v", out, err)
		}
	}
}
