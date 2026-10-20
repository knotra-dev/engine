package client

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/knotra-dev/engine/internal/protocol"
)

func TestCommandInitializesPrivateJournalBeforeRequest(t *testing.T) {
	for _, storage := range []string{"first use", "existing directory", "symlink"} {
		t.Run(storage, func(t *testing.T) {
			client := &Client{BaseURL: "http://localhost", StateDir: filepath.Join(t.TempDir(), "new", "state")}
			outside := t.TempDir()
			switch storage {
			case "existing directory":
				if err := os.MkdirAll(client.commandDir(), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(client.commandDir(), 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.MkdirAll(filepath.Dir(client.commandDir()), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, client.commandDir()); err != nil {
					t.Skipf("filesystem cannot create test symlink: %v", err)
				}
			}
			requests, posts := 0, 0
			client.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				info, err := os.Lstat(client.commandDir())
				if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0700) {
					t.Fatalf("journal directory is not private before request: %v, %v", info, err)
				}
				body := fmt.Sprintf(`{"protocol":%q,"engineId":"engine","principalId":"operator"}`, protocol.Version)
				if req.Method == http.MethodPost {
					posts++
					pending, err := client.load("stable")
					if err != nil || pending.Status != "pending" {
						t.Fatalf("request sent without pending journal: %+v, %v", pending, err)
					}
					body = `{"run":{"id":"run","status":"pending"}}`
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			var out any
			err := client.Command(t.Context(), "/runs", map[string]any{}, "stable", &out)
			if storage == "symlink" {
				if err == nil || requests != 0 {
					t.Fatalf("invalid journal reached engine: err=%v requests=%d", err, requests)
				}
				entries, readErr := os.ReadDir(outside)
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("invalid journal target was modified: %v, %v", entries, readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Retry(t.Context(), "stable", &out); err != nil {
				t.Fatal(err)
			}
			if requests != 3 || posts != 1 {
				t.Fatalf("completed command reissued during reconciliation: requests=%d posts=%d", requests, posts)
			}
		})
	}
}
