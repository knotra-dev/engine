package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func TestJavaScriptPreviewOmitsWholePortsWithoutChangingValues(t *testing.T) {
	for _, text := range []string{`9007199254740993`, `-9223372036854775808`, `{"nested":[9223372036854775807]}`, `1e20`} {
		values := map[string]json.RawMessage{"unsafe": json.RawMessage(text), "safe": json.RawMessage(`{"n":9007199254740991,"text":"9223372036854775807"}`)}
		run := protocol.Run{Inputs: values, Outputs: values}
		preview := previewRun(run)
		if !preview.DataTruncated || len(preview.Inputs) != 1 || len(preview.Outputs) != 1 || !bytes.Equal(preview.Inputs["safe"], values["safe"]) {
			t.Fatalf("incorrect preview for %s: %+v", text, preview)
		}
		if string(run.Inputs["unsafe"]) != text || string(run.Outputs["unsafe"]) != text || run.DataTruncated {
			t.Fatal("preview mutated the authoritative value")
		}
	}
}

func TestJavaScriptRunPreviewPreservesExactAdmissionReceiptsAndSavedValues(t *testing.T) {
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
	handler := (&Server{Store: db, Profiles: map[string]contract.Profile{"test": profile}, Wake: func(context.Context, pgx.Tx, string, int64) error { return nil }}).Handler()
	call := func(method, path, key string, payload any) []byte {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequestWithContext(t.Context(), method, "/v1"+path, bytes.NewReader(body))
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK && response.Code != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, response.Code, response.Body.String())
		}
		return response.Body.Bytes()
	}
	const integer = `9007199254740993`
	pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: integers}
spec:
  inputs:
    exact: {schema: {type: integer}}
    safe: {schema: {type: string}}
  nodes:
    review:
      type: human
      human: {prompt: {text: Review}}
      outputs:
        approved: {schema: {type: boolean}}
  outputs:
    exact: {schema: {type: integer}, bind: {from: inputs.exact}}
`}
	pkg.Files = []contract.File{{Path: pkg.Entrypoint, Content: []byte(pkg.Source)}}
	var definition struct{ Definition protocol.Definition }
	if err := json.Unmarshal(call("POST", "/definitions", "define-int64", map[string]any{"package": pkg}), &definition); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"definitionId": definition.Definition.ID, "profile": "test", "inputs": map[string]any{"exact": json.RawMessage(integer), "safe": "kept"}, "artifacts": map[string]any{}}
	var receipt struct{ Run protocol.Run }
	if err := json.Unmarshal(call("POST", "/runs?preview=javascript", "start-int64", payload), &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Run.DataTruncated || len(receipt.Run.Inputs) != 1 || string(receipt.Run.Inputs["safe"]) != `"kept"` {
		t.Fatalf("start preview: %+v", receipt.Run)
	}
	id := receipt.Run.ID
	rawReceipt := call("POST", "/runs", "start-int64", payload)
	receipt.Run = protocol.Run{}
	if err := json.Unmarshal(rawReceipt, &receipt); err != nil || string(receipt.Run.Inputs["exact"]) != integer || receipt.Run.DataTruncated {
		t.Fatalf("exact persisted receipt: %s %v", rawReceipt, err)
	}
	if again := call("POST", "/runs", "start-int64", payload); !bytes.Equal(rawReceipt, again) {
		t.Fatal("preview changed the stored command receipt")
	}
	if err := pgx.BeginFunc(t.Context(), db.Pool, func(tx pgx.Tx) error {
		return db.SaveRequestTx(t.Context(), tx, execution.Request{ID: "int64-review", RunID: id, InstanceID: "review", Kind: "human", Status: "open", Prompt: "Review", Deadline: time.Now().Add(time.Hour),
			Inputs: contract.Values{"exact": {JSON: json.RawMessage(integer)}}, Outputs: map[string]contract.Port{"exact": {Schema: json.RawMessage(`{"type":"integer","const":9223372036854775807}`)}}})
	}); err != nil {
		t.Fatal(err)
	}
	for _, preview := range []bool{false, true} {
		path := "/requests"
		if preview {
			path += "?preview=javascript"
		}
		body := call("GET", path, "", nil)
		var requests protocol.Page[protocol.HumanRequest]
		if err := json.Unmarshal(body, &requests); err != nil || len(requests.Items) != 1 {
			t.Fatalf("requests: %s %v", body, err)
		}
		request := requests.Items[0]
		if preview {
			if !request.DataTruncated || string(request.ResponseSchema) != "null" || !strings.Contains(request.ResponseSchemaJSON, "9223372036854775807") {
				t.Fatalf("request preview: %s", body)
			}
		} else if !bytes.Contains(body, []byte(integer)) || !bytes.Contains(request.ResponseSchema, []byte("9223372036854775807")) || request.DataTruncated {
			t.Fatalf("exact request changed: %s", body)
		}
	}
	if err := pgx.BeginFunc(t.Context(), db.Pool, func(tx pgx.Tx) error {
		return db.ProjectTx(t.Context(), tx, execution.Projection{RunID: id, Kind: "run", Sequence: 1, Status: "succeeded", Time: time.Now(), Outputs: contract.Values{"exact": {JSON: json.RawMessage(integer)}}})
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/runs/" + id, "/runs"} {
		for _, preview := range []bool{false, true} {
			target := path
			if preview {
				target += "?preview=javascript"
			}
			body := call("GET", target, "", nil)
			var run protocol.Run
			if path == "/runs" {
				var page protocol.Page[protocol.Run]
				if err := json.Unmarshal(body, &page); err != nil || len(page.Items) != 1 {
					t.Fatalf("run list: %s %v", body, err)
				}
				run = page.Items[0]
			} else {
				receipt.Run = protocol.Run{}
				if err := json.Unmarshal(body, &receipt); err != nil {
					t.Fatal(err)
				}
				run = receipt.Run
			}
			if preview {
				if !run.DataTruncated || len(run.Inputs) != 1 || len(run.Outputs) != 0 || strings.Contains(string(body), integer) {
					t.Fatalf("unsafe preview: %s", body)
				}
			} else if string(run.Inputs["exact"]) != integer || string(run.Outputs["exact"]) != integer || run.DataTruncated {
				t.Fatalf("exact run changed: %s", body)
			}
		}
	}
	stored, err := db.Run(t.Context(), id)
	if err != nil || string(stored.Inputs["exact"]) != integer || string(stored.Outputs["exact"]) != integer || stored.DataTruncated {
		t.Fatalf("stored values changed: %+v %v", stored, err)
	}
}

func TestJavaScriptRequestPreviewRetainsUnsafeSchemaAsExactText(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer","const":9223372036854775807}}}`)
	inputs := contract.ContextEnvelope(contract.Values{"unsafe": {JSON: json.RawMessage(`9007199254740993`)}, "safe": {JSON: json.RawMessage(`true`)}})
	request := protocol.HumanRequest{Inputs: inputs, ResponseSchema: schema}
	preview := previewRequest(request)
	if !preview.DataTruncated || string(preview.ResponseSchema) != "null" || preview.ResponseSchemaJSON != string(schema) {
		t.Fatalf("unsafe schema preview: %+v", preview)
	}
	encoded, err := json.Marshal(preview.Inputs)
	if err != nil || string(encoded) != `{"artifacts":{},"values":{"safe":true}}` {
		t.Fatalf("request input preview: %s %v", encoded, err)
	}
	original, err := json.Marshal(inputs)
	if err != nil || string(request.ResponseSchema) != string(schema) || !bytes.Contains(original, []byte("9007199254740993")) {
		t.Fatal("preview changed the original request")
	}
}

func TestJavaScriptRunPreviewPreservesByteLimitedPageCursor(t *testing.T) {
	db := apiStore(t)
	value := json.RawMessage(`{"integer":9007199254740993,"padding":"` + strings.Repeat("x", 12<<20) + `"}`)
	if err := contract.ValidateValue(contract.Port{Schema: json.RawMessage(`{"type":"object"}`)}, contract.Value{JSON: value}); err != nil {
		t.Fatal(err)
	}
	plan := contract.Plan{Version: "knotra/v1", CompilerVersion: contract.CompilerVersion, CELVersion: contract.CELVersion, Root: "pipeline.yaml", Digest: "large-results",
		Pipelines: map[string]*contract.Pipeline{"pipeline.yaml": {}}, Profile: contract.Profile{Spec: contract.ProfileSpec{Limits: contract.Limits{Timeout: "1h"}}}}
	if err := pgx.BeginFunc(t.Context(), db.Pool, func(tx pgx.Tx) error {
		definition := protocol.Definition{ID: "large-results", Name: "large-results", PackageDigest: "large-results", Package: contract.Package{Entrypoint: "pipeline.yaml", Files: []contract.File{{Path: "pipeline.yaml", Content: []byte("pipeline")}}}}
		if _, err := store.PutDefinition(t.Context(), tx, definition); err != nil {
			return err
		}
		for _, id := range []string{"r1", "r2", "r3"} {
			run := protocol.Run{ID: id, DefinitionID: definition.ID, Title: id, Status: "pending", Profile: "test",
				Outputs: map[string]json.RawMessage{"large": value}, Artifacts: []contract.Artifact{}, Diagnostics: []contract.Diagnostic{}, AvailableActions: []string{}}
			if _, err := store.PutRiverRun(t.Context(), tx, run, plan, contract.Values{}, func(context.Context, pgx.Tx, string, int64) error { return nil }); err != nil {
				return err
			}
			if err := db.ProjectTx(t.Context(), tx, execution.Projection{RunID: id, Kind: "run", Sequence: 1, Status: "succeeded", Time: time.Now(), Outputs: contract.Values{"large": {JSON: value}}}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	handler := (&Server{Store: db}).Handler()
	cursor := ""
	for _, want := range [][]string{{"r3", "r2"}, {"r1"}} {
		response := httptest.NewRecorder()
		request := httptest.NewRequestWithContext(t.Context(), "GET", "/v1/runs?preview=javascript&cursor="+cursor, nil)
		handler.ServeHTTP(response, request)
		var page protocol.Page[protocol.Run]
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || response.Code != http.StatusOK || len(page.Items) != len(want) {
			t.Fatalf("preview page: %d %s %v", response.Code, response.Body.String(), err)
		}
		for i, id := range want {
			if page.Items[i].ID != id || !page.Items[i].DataTruncated || len(page.Items[i].Outputs) != 0 || len(page.Items[i].Package.Files) != 1 {
				t.Fatalf("incomplete or reordered preview: %+v", page.Items[i])
			}
		}
		if len(want) == 2 {
			if page.NextCursor == nil || *page.NextCursor != "r2" {
				t.Fatalf("preview lost byte-budget continuation: %+v", page.NextCursor)
			}
			cursor = *page.NextCursor
		} else if page.NextCursor != nil {
			t.Fatal("unexpected final page continuation")
		}
	}
}
