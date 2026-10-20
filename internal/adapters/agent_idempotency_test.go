package adapters

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/knotra-dev/engine/internal/contract"
)

func TestIdempotencyProjectionRejectsAffectedObjectConstraints(t *testing.T) {
	for _, tc := range []struct {
		name, keyword, constraint string
	}{
		{"minimum properties", "minProperties", `"minProperties":2`},
		{"maximum properties", "maxProperties", `"maxProperties":2`},
		{"dependency on reserved field", "dependentRequired", `"dependentRequired":{"name":["key"]}`},
		{"dependency triggered by reserved field", "dependentRequired", `"dependentRequired":{"key":["name"]}`},
	} {
		for _, nested := range []bool{false, true} {
			name, pointer := tc.name, "/key"
			schema := `{"type":"object","properties":{"key":{"type":"string"},"name":{"type":"string"}},"required":["key","name"],"additionalProperties":false,` + tc.constraint + `}`
			if nested {
				name, pointer = "nested "+name, "/meta/key"
				schema = `{"type":"object","properties":{"meta":` + schema + `},"required":["meta"]}`
			}
			t.Run(name, func(t *testing.T) {
				if _, err := projectIdempotency(json.RawMessage(schema), pointer); err == nil || !strings.Contains(err.Error(), tc.keyword) {
					t.Fatalf("expected %s projection error, got %v", tc.keyword, err)
				}
			})
		}
	}
}

func TestIdempotencyProjectionPreservesUnaffectedConstraints(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"meta":{"type":"object","properties":{
				"request/id~":{"type":"string"},"name":{"type":"string"},"tag":{"type":"string"}
			},"required":["request/id~"],"dependentRequired":{"name":["tag"]},"additionalProperties":false},
			"payload":{"type":"object","minProperties":1,"maxProperties":1}
		},
		"required":["meta"],"minProperties":1,"maxProperties":2,"dependentRequired":{"payload":["meta"]},"additionalProperties":false
	}`)
	const pointer = "/meta/request~1id~0"
	projected, err := projectIdempotency(schema, pointer)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, arguments string
		valid           bool
	}{
		{"reserved field only", `{"meta":{}}`, true},
		{"unrelated constraints satisfied", `{"meta":{"name":"a","tag":"b"},"payload":{"item":1}}`, true},
		{"unrelated dependency missing", `{"meta":{"name":"a"}}`, false},
		{"sibling minimum missing", `{"meta":{},"payload":{}}`, false},
		{"sibling maximum exceeded", `{"meta":{},"payload":{"a":1,"b":2}}`, false},
		{"reserved field still hidden", `{"meta":{"request/id~":"model-value"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := contract.ValidateValue(contract.Port{Schema: projected}, contract.Value{JSON: json.RawMessage(tc.arguments)}); (err == nil) != tc.valid {
				t.Fatalf("projected validation: expected valid=%v, got %v", tc.valid, err)
			}
			if !tc.valid {
				return
			}
			var arguments map[string]any
			if err := json.Unmarshal([]byte(tc.arguments), &arguments); err != nil {
				t.Fatal(err)
			}
			if err := insertIdempotency(arguments, pointer, "engine-value"); err != nil {
				t.Fatal(err)
			}
			complete, err := json.Marshal(arguments)
			if err != nil {
				t.Fatal(err)
			}
			if err := contract.ValidateValue(contract.Port{Schema: schema}, contract.Value{JSON: complete}); err != nil {
				t.Fatalf("injected arguments violate original schema: %v", err)
			}
		})
	}
}
