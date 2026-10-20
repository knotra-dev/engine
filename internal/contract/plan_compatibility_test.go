package contract

import (
	"encoding/json"
	"testing"
)

func TestLegacyPlanExpressionSnapshotsRemainCompatible(t *testing.T) {
	source := []byte(`{
		"apiVersion": "knotra/v1", "kind": "Pipeline", "metadata": {"name": "legacy"},
		"spec": {
			"nodes": {"ask": {
				"type": "human", "human": {"prompt": {"text": "Return a string."}},
				"outputs": {"answer": {"schema": {"type": "string"}}}
			}},
			"outputs": {"answer": {
				"schema": {"type": "string"}, "bind": {"expr": "nodes.ask.outputs.answer + '!'"}
			}}
		}
	}`)
	plan, diags := Compile(Package{Entrypoint: "pipeline.yaml", Files: []File{{Path: "pipeline.yaml", Content: source}}}, nil)
	if HasErrors(diags) {
		t.Fatal(diags)
	}
	raw, err := json.Marshal(struct {
		*Plan
		Expressions map[string]json.RawMessage `json:"expressions"`
	}{plan, map[string]json.RawMessage{"legacy": json.RawMessage(`{"expr":{"id":"1","constExpr":{"stringValue":"unused"}}}`)}})
	if err != nil {
		t.Fatal(err)
	}
	var restored Plan
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Version != "knotra/v1" || restored.CompilerVersion != CompilerVersion || restored.CELVersion != CELVersion {
		t.Fatal("restored plan versions changed")
	}
	output := restored.Pipelines[restored.Root].Spec.Outputs["answer"]
	got, present, err := EvalBinding(*output.Bind, Scope{Nodes: map[string]Values{"ask": {"answer": val(t, "hello")}}})
	if err != nil || !present || string(got.JSON) != `"hello!"` {
		t.Fatalf("restored expression result=%s, present=%v, err=%v", got.JSON, present, err)
	}
}
