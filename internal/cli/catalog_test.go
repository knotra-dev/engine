package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/knotra-dev/engine/internal/protocol"
)

func TestPipelineShowKeepsLegacyCommandsAndJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/definitions/pipeline" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(
			w,
			`{"definition":{"id":"pipeline","name":"example","title":"Example","packageDigest":"digest","package":{"entrypoint":"pipeline.yaml","source":"exact bytes","files":[]}}}`,
		)
	}))
	defer server.Close()
	for _, command := range [][]string{{"pipelines", "show", "pipeline"}, {"definitions", "get", "pipeline"}} {
		args := append([]string{"--endpoint", server.URL}, command...)
		out, _, err := invoke(t, args, "")
		if err != nil || !strings.Contains(out, "Pipeline:") || !strings.Contains(out, "example") || strings.Contains(out, "exact bytes") {
			t.Fatalf("human output for %v: %q, %v", command, out, err)
		}
		out, _, err = invoke(t, append(args, "--json"), "")
		var result struct {
			Definition struct {
				ID      string         `json:"id"`
				Package map[string]any `json:"package"`
			} `json:"definition"`
		}
		if err != nil ||
			json.Unmarshal([]byte(out), &result) != nil ||
			result.Definition.ID != "pipeline" ||
			result.Definition.Package["source"] != "exact bytes" {
			t.Fatalf("JSON for %v: %q, %v", command, out, err)
		}
	}
}

func TestRequestFiltersSearchLaterPages(t *testing.T) {
	for _, filter := range [][]string{{"--run", "target"}, {"--status", "pending"}} {
		t.Run(filter[0], func(t *testing.T) {
			var cursors []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/requests" {
					t.Errorf("unexpected route %s", r.URL.Path)
				}
				cursor := r.URL.Query().Get("cursor")
				cursors = append(cursors, cursor)
				if cursor == "" {
					_, _ = fmt.Fprint(w, `{"items":[{"id":"unrelated","runId":"other","status":"answered"}],"nextCursor":"next"}`)
				} else {
					_, _ = fmt.Fprint(w, `{"items":[{"id":"wanted","runId":"target","status":"pending"}],"nextCursor":null}`)
				}
			}))
			defer server.Close()
			args := append([]string{"--endpoint", server.URL, "requests", "list", "--json"}, filter...)
			out, _, err := invoke(t, args, "")
			var result protocol.Page[protocol.HumanRequest]
			if err != nil ||
				json.Unmarshal([]byte(out), &result) != nil ||
				len(result.Items) != 1 ||
				result.Items[0].ID != "wanted" ||
				result.NextCursor != nil {
				t.Fatalf("filtered requests: %s, %v", out, err)
			}
			if len(cursors) != 2 || cursors[1] != "next" {
				t.Fatalf("did not search later pages: %v", cursors)
			}
		})
	}
}

func TestPipelinePublishKeepsLegacyCommand(t *testing.T) {
	file := fixtureFile(t, minimalPipeline)
	for _, command := range [][]string{{"pipelines", "publish"}, {"definitions", "create"}} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			var published bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/info":
					_, _ = fmt.Fprintf(w, `{"protocol":%q,"engineId":"engine","principalId":"user"}`, protocol.Version)
				case "/v1/definitions":
					var body struct {
						Package struct {
							Source string `json:"source"`
						} `json:"package"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					published = body.Package.Source == minimalPipeline && r.Method == http.MethodPost
					_, _ = fmt.Fprint(w, `{"definition":{"id":"pipeline"}}`)
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			}))
			defer server.Close()
			args := append([]string{"--endpoint", server.URL, "--state-dir", t.TempDir(), "--json"}, command...)
			args = append(args, file, "--idempotency-key", "publish")
			out, _, err := invoke(t, args, "")
			var result struct {
				Definition  protocol.Definition `json:"definition"`
				OperationID string              `json:"operationId"`
			}
			if err != nil ||
				json.Unmarshal([]byte(out), &result) != nil ||
				!published ||
				result.Definition.ID != "pipeline" ||
				result.OperationID != "publish" {
				t.Fatalf("pipeline publish changed: %s, %v (published=%v)", out, err, published)
			}
		})
	}
}
