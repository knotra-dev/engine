package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/knotra-dev/engine/internal/contract"
)

func TestObjectPreservesNumericTypes(t *testing.T) {
	const input = `{"x":1.0,"nested":{"values":[1,1.0,{"x":2.0}]}}`

	for _, test := range []struct {
		name        string
		file        string
		assignments []string
	}{
		{name: "assignments", assignments: []string{"x=1.0", `nested={"values":[1,1.0,{"x":2.0}]}`}},
		{name: "file", file: fixtureFile(t, input)},
		{name: "stdin", file: "-"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := commandState{options: Options{In: strings.NewReader(input)}}
			values, err := s.object(test.file, test.assignments)
			if err != nil {
				t.Fatal(err)
			}
			result, present, err := contract.EvalBinding(
				contract.Binding{Expr: "args.x + 0.5"},
				contract.Scope{Args: contract.Values{"x": {JSON: values["x"]}}},
			)
			if err != nil || !present || string(result.JSON) != "1.5" {
				t.Fatalf("float arithmetic changed: result=%s present=%v err=%v", result.JSON, present, err)
			}
			nested, err := contract.DecodeJSON(values["nested"])
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"values": []any{int64(1), float64(1), map[string]any{"x": float64(2)}}}
			if !reflect.DeepEqual(nested, want) {
				t.Fatalf("nested numeric types changed: got %#v, want %#v", nested, want)
			}
		})
	}
}
