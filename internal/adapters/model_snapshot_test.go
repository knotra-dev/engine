package adapters

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/knotra-dev/engine/internal/contract"
)

func TestAdmittedModelCallsKeepBindingsAfterSourceChanges(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := decodeRequest(t, r)
		if r.Header.Get("Authorization") != "Bearer original-key" || string(body["model"]) != `"original-model"` {
			t.Errorf("admitted call changed credentials or model: %s", body["model"])
		}
		writeCloudOutput(w, "openai", "knotra_output", `{"answer":42}`, false)
	}))
	defer server.Close()
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("existing run called the replacement endpoint")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer replacement.Close()
	profile, diagnostics := contract.ParseProfile([]byte(fmt.Sprintf(`apiVersion: knotra/v1
kind: EngineProfile
metadata: {name: local}
spec:
  models:
    source:
      provider: openai
      model: original-model
      baseUrl: %q
      auth: {key: {secretRef: source_key}}
  secrets: {source_key: {env: ORIGINAL_MODEL_KEY}}
  limits: {timeout: 10m, maxConcurrentNodes: 4, maxNodeInstances: 100, maxModelCalls: 10, maxToolCalls: 20}
`, server.URL)))
	if contract.HasErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	source := `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: frozen-models}
spec:
  models: {main: {connection: source}}
  nodes:
    first:
      type: llm
      llm: {model: main, prompt: {text: Return 42}}
      outputs: {answer: {schema: {type: integer}}}
    second:
      type: llm
      inputs:
        previous: {schema: {type: integer}, bind: {from: nodes.first.outputs.answer}}
      llm: {model: main, prompt: {text: Return 42 again}}
      outputs: {answer: {schema: {type: integer}}}
  outputs:
    answer: {schema: {type: integer}, bind: {from: nodes.second.outputs.answer}}
`
	pkg := contract.Package{Entrypoint: "pipeline.yaml", Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
	plan, diagnostics := contract.Compile(pkg, &profile)
	if contract.HasErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	runner := &Runner{Hooks: newHooks(), LookupEnv: func(name string) (string, bool) {
		if name == "ORIGINAL_MODEL_KEY" {
			return "original-key", true
		}
		return "replacement-key", true
	}}
	for _, id := range []string{"first", "second"} {
		if id == "second" {
			profile.Spec.Models["source"] = contract.ModelConnection{
				Provider: "anthropic",
				Model:    "replacement-model",
				BaseURL:  replacement.URL,
				Auth:     map[string]contract.Credential{"key": {SecretRef: "source_key"}},
			}
			profile.Spec.Secrets["source_key"] = contract.SecretSource{Env: "REPLACEMENT_MODEL_KEY"}
			// Later deliveries restore the saved plan, never the current profile.
			data, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			plan = &contract.Plan{}
			if err := json.Unmarshal(data, plan); err != nil {
				t.Fatal(err)
			}
		}
		req := Request{
			RunID:      "frozen-models",
			InstanceID: id,
			Attempt:    1,
			Pipeline:   plan.Root,
			ScopeID:    "root",
			Plan:       plan,
			Node:       plan.Pipelines[plan.Root].Spec.Nodes[id],
			Inputs:     contract.Values{},
		}
		values, err := runner.Execute(t.Context(), req)
		if err != nil || string(values["answer"].JSON) != "42" {
			t.Fatalf("%s delivery: %v %v", id, values, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("original source received %d calls, want both deliveries", calls.Load())
	}
}
