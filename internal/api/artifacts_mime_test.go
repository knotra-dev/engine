package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store"
)

func TestArtifactUploadMediaTypesBindToCanonicalPorts(t *testing.T) {
	db := apiStore(t)
	profile, diags := contract.ParseProfile([]byte(`apiVersion: knotra/v1
kind: EngineProfile
metadata: {name: test}
spec:
  limits: {timeout: 10m, maxConcurrentNodes: 4, maxNodeInstances: 100, maxModelCalls: 10, maxToolCalls: 20}
`))
	if contract.HasErrors(diags) {
		t.Fatal(diags)
	}
	srv := Server{
		Store:     db,
		Artifacts: store.Artifacts{Root: t.TempDir(), Store: db},
		Profiles:  map[string]contract.Profile{"test": profile},
	}
	handler := srv.Handler()
	post := func(route, key string, payload any) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1"+route, bytes.NewReader(body))
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: artifact-input}
spec:
  inputs:
    file: {artifact: {mediaTypes: [text/plain]}}
  nodes:
    approval:
      type: human
      human: {prompt: {text: Approve}}
      outputs:
        ok: {schema: {type: boolean}}
  outputs:
    ok:
      schema: {type: boolean}
      bind: {from: nodes.approval.outputs.ok}
`}
	pkg.Files = []contract.File{{Path: pkg.Entrypoint, Content: []byte(pkg.Source)}}
	for _, tc := range []struct {
		name, mediaType string
		status          int
	}{
		{"canonical", "text/plain", 201},
		{"uppercase", "TEXT/PLAIN", 201},
		{"whitespace", " \ttext/plain\t ", 201},
		{"empty", "", 422},
		{"missing-subtype", "text", 422},
		{"invalid", "text//plain", 422},
		{"wildcard-subtype", "text/*", 422},
		{"wildcard-type", "*/plain", 422},
		{"parameters", "text/plain; charset=utf-8", 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"name": "file.txt", "mediaType": tc.mediaType, "content": []byte("file")}
			w := post("/artifacts", tc.name, payload)
			if w.Code != tc.status {
				t.Fatalf("upload status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status != 201 {
				var result protocol.Error
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Code != "INPUT_INVALID" {
					t.Fatalf("invalid MIME type must produce a structured input error: %s", w.Body.String())
				}
				return
			}
			var result struct {
				Artifact contract.Artifact `json:"artifact"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Artifact.MediaType != "text/plain" {
				t.Fatalf("upload MIME type %q is not canonical", result.Artifact.MediaType)
			}
			w = post("/packages/validate", "", map[string]any{
				"package": pkg, "profile": "test", "inputs": map[string]any{},
				"artifacts": map[string]any{"file": result.Artifact.ID},
			})
			var validation struct {
				Valid bool `json:"valid"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &validation); err != nil || w.Code != 200 || !validation.Valid {
				t.Fatalf("uploaded artifact could not bind to a canonical port: %s", w.Body.String())
			}
		})
	}
	artifacts, err := db.Artifacts(t.Context(), "")
	if err != nil || len(artifacts) != 3 {
		t.Fatalf("invalid MIME types registered artifacts: count=%d error=%v", len(artifacts), err)
	}
}
