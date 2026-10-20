package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunInstancesReadsAllPages(t *testing.T) {
	var cursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/run/instances" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		cursor := r.URL.Query().Get("cursor")
		cursors = append(cursors, cursor)
		w.Header().Set("Content-Type", "application/json")
		if cursor == "" {
			_, _ = fmt.Fprint(w, `{"items":[{"id":"n1","nodeId":"work","scope":"pipeline.yaml","status":"succeeded"}],"nextCursor":"n1"}`)
		} else {
			_, _ = fmt.Fprint(w, `{"items":[{"id":"n2","nodeId":"work","scope":"pipeline.yaml","status":"succeeded"}],"nextCursor":null}`)
		}
	}))
	defer server.Close()
	out, _, err := invoke(t, []string{"--endpoint", server.URL, "--state-dir", t.TempDir(), "runs", "instances", "run", "--all", "--json"}, "")
	if err != nil || !strings.Contains(out, `"n1"`) || !strings.Contains(out, `"n2"`) || len(cursors) != 2 || cursors[1] != "n1" {
		t.Fatalf("instance pages: %s, %v, cursors %v", out, err, cursors)
	}
}
