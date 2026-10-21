package contract

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCELDecodesOnlyReferencedValues(t *testing.T) {
	invalid := Value{JSON: json.RawMessage(`invalid`)}
	item := val(t, int64(6))
	scope := Scope{
		Inputs: Values{"value": val(t, int64(1)), "unused": invalid},
		Args: Values{
			"object": val(t, map[string]any{"answer": int64(2)}),
			"key":    val(t, "answer"),
			"unused": invalid,
		},
		State:          Values{"value": val(t, int64(3)), "unused": invalid},
		Nodes:          map[string]Values{"ready": {"value": val(t, int64(4)), "unused": invalid}, "unused": {"value": invalid}},
		Body:           Values{"value": val(t, int64(5)), "unused": invalid},
		IterationItem:  &item,
		IterationIndex: 7,
	}
	for _, test := range []struct{ source, want string }{
		{`false`, `false`},
		{`inputs.value + args.object[args.key] + state.value + nodes.ready.outputs.value + body.outputs.value + iteration.item + iteration.index`, `28`},
		{`args.object.map(k, args.object[k])`, `[2]`},
		{`has(args.absent)`, `false`},
	} {
		t.Run(test.source, func(t *testing.T) {
			got, present, err := EvalBinding(Binding{Expr: test.source}, scope)
			if err != nil || !present || string(got.JSON) != test.want {
				t.Fatalf("got %s, present=%v, err=%v; want %s", got.JSON, present, err, test.want)
			}
		})
	}
	if !reflect.DeepEqual(scope.Args["unused"], invalid) || len(scope.Args) != 3 || len(scope.Nodes["ready"]) != 2 {
		t.Fatal("evaluation changed source values")
	}
}

func TestCELReferencedValuesRetainValidation(t *testing.T) {
	for _, value := range []Value{{JSON: json.RawMessage(`invalid`)}, {Artifacts: []Artifact{{ID: "artifact"}}}} {
		if _, _, err := EvalBinding(Binding{Expr: `true || args.value == 0`}, Scope{Args: Values{"value": value}}); err == nil {
			t.Fatal("referenced invalid JSON or artifact accepted")
		}
	}
	if _, present, err := EvalBinding(Binding{Expr: `true || inputs.absent == args.value`}, Scope{Args: Values{"value": {JSON: json.RawMessage(`invalid`)}}}); err != nil || present {
		t.Fatalf("missing graph source must dominate decoding: present=%v, err=%v", present, err)
	}
}

func BenchmarkCELScope(b *testing.B) {
	x, err := JSONValue(int64(1))
	if err != nil {
		b.Fatal(err)
	}
	large, err := JSONValue(strings.Repeat("x", 1<<20))
	if err != nil {
		b.Fatal(err)
	}
	for _, test := range []struct {
		name, source string
		scope        Scope
	}{
		{"constant_empty", `false`, Scope{}},
		{"constant_unused_1MiB", `false`, Scope{Args: Values{"unused": large}}},
		{"referenced_small", `args.x > 0`, Scope{Args: Values{"x": x}}},
		{"referenced_small_unused_1MiB", `args.x > 0`, Scope{Args: Values{"x": x, "unused": large}}},
	} {
		b.Run(test.name, func(b *testing.B) {
			if _, _, err := EvalBool(test.source, test.scope); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, _, err := EvalBool(test.source, test.scope); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
