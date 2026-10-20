package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestMutationPresentation(t *testing.T) {
	for _, test := range []struct {
		name   string
		result map[string]any
		want   []string
	}{
		{"pipeline", map[string]any{"definition": map[string]any{"id": "pipeline", "name": "Example\x1b[2J", "packageDigest": "digest"}}, []string{"Pipeline:", "pipeline", "Name:", "Example", "digest"}},
		{"artifact", map[string]any{"artifact": map[string]any{"id": "artifact", "name": "report.txt", "mediaType": "text/plain", "size": float64(12)}}, []string{"Artifact:", "artifact", "report.txt", "text/plain", "12"}},
		{"run", map[string]any{"run": map[string]any{"id": "run", "status": "succeeded", "profile": "local"}}, []string{"Run:", "run", "Status:", "succeeded", "local"}},
		{"action", map[string]any{"accepted": true, "runId": "run"}, []string{"Accepted:", "true", "Run:", "run"}},
		{"response", map[string]any{"accepted": true, "requestId": "request"}, []string{"Accepted:", "true", "Request:", "request"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.result["operationId"] = "operation"
			var out bytes.Buffer
			s := commandState{options: Options{Out: &out}}
			if err := s.printMutation(test.result); err != nil {
				t.Fatal(err)
			}
			for _, want := range append(test.want, "Operation:", "operation") {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q in %q", want, out.String())
				}
			}
			if strings.ContainsAny(out.String(), "\x1b{}") {
				t.Fatalf("human output contains raw JSON or terminal controls: %q", out.String())
			}
			out.Reset()
			s.json = true
			if err := s.printMutation(test.result); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil || !reflect.DeepEqual(got, test.result) {
				t.Fatalf("JSON receipt changed: %s, %v", out.String(), err)
			}
		})
	}
}

func TestFieldsSanitizeTerminalControls(t *testing.T) {
	var out bytes.Buffer
	s := commandState{options: Options{Out: &out}}
	if err := s.fields([][]string{{"Name\n", "report\x1b[2J\t.txt"}}); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\t") || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("terminal controls escaped fields: %q", out.String())
	}
}
