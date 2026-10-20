package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestErrorFormatRecovery(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		json bool
	}{
		{"unknown before json", []string{"status", "--bogus", "--json"}, true},
		{"unknown before format", []string{"status", "--bogus", "--format", "json"}, true},
		{"unknown before shorthand", []string{"status", "--bogus", "-j"}, true},
		{"invalid bool before format", []string{"runs", "--all=invalid", "--format", "json"}, true},
		{"invalid number before format", []string{"quickstart", "--postgres-port", "invalid", "--format=json"}, true},
		{"invalid json bool before format", []string{"status", "--json=invalid", "--format", "json"}, true},
		{"json flag is an input value", []string{"run", "--bogus", "--input", "--json"}, false},
		{"format flag is an input value", []string{"run", "--bogus", "--input", "--format", "json"}, false},
		{"json after delimiter", []string{"status", "--", "run", "--json"}, false},
		{"format after delimiter", []string{"status", "--", "--format", "json"}, false},
		{"later json false wins", []string{"status", "--json", "--bogus", "--json=false"}, false},
		{"later text format wins", []string{"status", "--format", "json", "--bogus", "--format=text"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			root, state := newCommand(Options{Out: &out, Err: &stderr, Getenv: func(string) string { return "" }})
			root.SetArgs(test.args)
			cmd, err := root.ExecuteC()
			if err == nil {
				t.Fatal("expected a command error")
			}
			state.errorFormat(root, test.args)
			state.reportError(cmd, err)
			if out.Len() != 0 || json.Valid(stderr.Bytes()) != test.json {
				t.Fatalf("stdout=%q stderr=%q, want JSON=%v", out.String(), stderr.String(), test.json)
			}
			if !test.json && !strings.Contains(stderr.String(), "Error:") {
				t.Fatalf("missing text error: %q", stderr.String())
			}
		})
	}
}

func TestErrorFormatDoesNotApplyFlagValues(t *testing.T) {
	root, state := newCommand(Options{Getenv: func(string) string { return "" }})
	flag := root.PersistentFlags().Lookup("endpoint")
	before := flag.Value.String()
	state.errorFormat(root, []string{"info", "--endpoint", "https://other.example", "--json"})
	if flag.Value.String() != before || flag.Changed {
		t.Fatalf("error formatting mutated endpoint: value=%q changed=%v", flag.Value.String(), flag.Changed)
	}
	if !state.json {
		t.Fatal("JSON selection was not recovered")
	}
}
