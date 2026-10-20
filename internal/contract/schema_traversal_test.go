package contract

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDataProfileMemoizesSharedReferences(t *testing.T) {
	defs := map[string]any{"d0": true}
	for i := 1; i <= 31; i++ {
		defs[fmt.Sprintf("d%d", i)] = map[string]any{"allOf": []any{
			map[string]any{"$ref": fmt.Sprintf("#/$defs/d%d", i-1)},
			map[string]any{"$ref": fmt.Sprintf("#/%%24defs/d%d", i-1)},
		}}
	}
	aliases := []any{}
	fragment := "/$defs/d31"
	for mask := range 1 << len(fragment) {
		ref := "#"
		for i := range len(fragment) {
			if mask&(1<<i) != 0 {
				ref += fmt.Sprintf("%%%02x", fragment[i])
			} else {
				ref += fragment[i : i+1]
			}
		}
		aliases = append(aliases, map[string]any{"$ref": ref})
	}
	value := map[string]any{"$defs": defs, "allOf": aliases}
	refs := map[string]int{}
	height, err := validateDataProfile(value, value, refs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if height != MaxDepth || len(refs) != len(defs) {
		t.Fatalf("height=%d, memoized targets=%d; want %d and %d", height, len(refs), MaxDepth, len(defs))
	}
	for i := 0; i <= 31; i++ {
		if got := refs[fmt.Sprintf("/$defs/d%d", i)]; got != 2*i {
			t.Errorf("d%d height=%d, want %d", i, got, 2*i)
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compileDataSchema(raw); err != nil {
		t.Fatal(err)
	}
}

func TestDataProfileMemoizedDepth(t *testing.T) {
	for _, depth := range []int{32, 33} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			defs := map[string]any{"d0": true}
			for i := 1; i <= 31; i++ {
				defs[fmt.Sprintf("d%d", i)] = map[string]any{"$ref": fmt.Sprintf("#/$defs/d%d", i-1)}
			}
			var nested any = map[string]any{"$ref": "#/$defs/d31"}
			for range depth - 1 {
				nested = map[string]any{"not": nested}
			}
			value := map[string]any{"$defs": defs, "not": nested}
			refs := map[string]int{}
			height, err := validateDataProfile(value, value, refs, 0)
			if depth == 32 {
				if err != nil || height != MaxDepth {
					t.Fatalf("height=%d, err=%v; want height %d", height, err, MaxDepth)
				}
			} else if err == nil || !strings.Contains(err.Error(), "depth exceeds 64") {
				t.Fatalf("deep cached reference accepted: %v", err)
			}
		})
	}
}

func TestDataProfileReferenceRejections(t *testing.T) {
	for _, raw := range []string{
		`{"$ref":"#"}`,
		`{"$defs":{"a":{"$ref":"#/%24defs/b"},"b":{"$ref":"#/$defs/a"}}}`,
		`{"$defs":{"a/b~c":{"$ref":"#%2f$defs%2fa~1b~0c"}}}`,
		`{"$ref":"#/missing"}`,
		`{"$ref":"#anchor"}`,
		`{"$ref":"#/%zz"}`,
		`{"$ref":"#/$defs/a~2","$defs":{"a":true}}`,
		`{"$ref":"#/allOf/00","allOf":[true]}`,
		`{"$ref":"#/const","const":42}`,
		`{"$defs":{"unused":{"format":"date"}}}`,
		`{"$defs":{"unused":{"pattern":"(?=a)a"}}}`,
		`{"$defs":{"unused":null}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := compileDataSchema(json.RawMessage(raw)); err == nil {
				t.Fatal("invalid reference or unreferenced definition accepted")
			}
		})
	}
}
