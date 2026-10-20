//go:build unix

package adapters

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/knotra-dev/engine/internal/execution"
)

func TestWorkDirectoriesStaySeparateForExecutionHosts(t *testing.T) {
	first := &Runner{EngineID: "engine", HostID: "host-a", WorkDir: t.TempDir()}
	second := &Runner{EngineID: first.EngineID, HostID: "host-b", WorkDir: first.WorkDir}
	local := &Runner{EngineID: first.EngineID, HostID: "local", WorkDir: first.WorkDir}
	defaultHost := &Runner{EngineID: first.EngineID, WorkDir: first.WorkDir}
	if first.workRoot() == second.workRoot() || first.workRoot() == local.workRoot() || local.workRoot() != defaultHost.workRoot() {
		t.Fatal("host staging namespaces overlap or changed the default layout")
	}
	for _, runner := range []*Runner{first, second} {
		if err := os.MkdirAll(runner.workRoot(), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runner.workRoot(), "keep"), []byte(runner.HostID), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Removing one entire staging root cannot touch another host's workspace.
	if err := os.RemoveAll(first.workRoot()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(second.workRoot(), "keep"))
	if err != nil || string(data) != second.HostID {
		t.Fatalf("host cleanup crossed staging namespaces: %s error=%v", data, err)
	}
}

func TestCleanupResourceVerifiesLabelsAndConfinesDirectoryRemoval(t *testing.T) {
	for _, mode := range []string{"removed", "worker_mismatch", "generation_mismatch", "delete_failed", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			resource := execution.ResourceRecord{ID: uuid.NewString(), EngineID: uuid.NewString(), HostID: "local", Kind: "sandbox", State: "cleaning",
				Ownership: execution.Ownership{AttemptID: execution.AttemptID{RunID: "run", InstanceID: "node", Number: 1}, WorkerID: "owner", Generation: 1}}
			runner := &Runner{EngineID: resource.EngineID, HostID: resource.HostID, WorkDir: t.TempDir()}
			if err := os.MkdirAll(runner.workRoot(), 0700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("outside root"), 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(runner.workRoot(), resource.Name())
			if mode == "symlink" {
				if err := os.Symlink(outside, target); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			// Keep the Unix socket below the OS path limit, independently of TempDir.
			socketDir, err := os.MkdirTemp("/tmp", "knotra-resource-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
			socket := filepath.Join(socketDir, "docker.sock")
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			labels := resource.Labels()
			if mode == "worker_mismatch" {
				labels["io.knotra.worker"] = "another-worker"
			}
			if mode == "generation_mismatch" {
				labels["io.knotra.generation"] = "2"
			}
			var deletes atomic.Int32
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					var filters map[string][]string
					if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil || len(filters["label"]) != 3 {
						t.Errorf("cleanup filters=%v error=%v", filters, err)
					}
					if err := json.NewEncoder(w).Encode([]map[string]any{{"Id": "main", "Labels": labels}, {"Id": "firewall", "Labels": labels}}); err != nil {
						t.Error(err)
					}
					return
				}
				deletes.Add(1)
				if r.Method != http.MethodDelete || r.URL.Query().Get("force") != "true" {
					t.Errorf("unexpected cleanup request: %s %s", r.Method, r.URL)
				}
				if mode == "delete_failed" {
					w.WriteHeader(http.StatusInternalServerError)
				} else {
					w.WriteHeader(http.StatusNoContent)
				}
			})}
			go func() {
				if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					t.Error(err)
				}
			}()
			t.Cleanup(func() { _ = server.Close() })
			runner.DockerHost = "unix://" + socket
			err = runner.CleanupResource(t.Context(), resource)
			wantError := mode == "worker_mismatch" || mode == "generation_mismatch" || mode == "delete_failed"
			if (err != nil) != wantError {
				t.Fatalf("cleanup error=%v expectedError=%v", err, wantError)
			}
			_, statErr := os.Lstat(target)
			if (wantError && statErr != nil) || (!wantError && !os.IsNotExist(statErr)) {
				t.Fatalf("resource directory after cleanup: %v", statErr)
			}
			if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
				t.Fatal("cleanup escaped the resource directory", err)
			}
			if (mode == "worker_mismatch" || mode == "generation_mismatch") && deletes.Load() != 0 {
				t.Fatal("deleted a container with a different owner")
			}
			if !wantError {
				if err := runner.CleanupResource(t.Context(), resource); err != nil || deletes.Load() != 4 {
					t.Fatalf("cleanup retry deletes=%d error=%v", deletes.Load(), err)
				}
			}
		})
	}
}

func TestResourceInventoryPagesAndRejectsForeignLabels(t *testing.T) {
	resource := execution.ResourceRecord{ID: uuid.NewString(), EngineID: uuid.NewString(), HostID: "local", Kind: "sandbox",
		Ownership: execution.Ownership{AttemptID: execution.AttemptID{RunID: "run", InstanceID: "node", Number: 1}, WorkerID: "owner", Generation: 1}}
	socketDir, err := os.MkdirTemp("/tmp", "knotra-inventory-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "docker.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var filters map[string][]string
		if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodGet || r.URL.Query().Get("limit") != "64" || r.URL.Query().Get("all") != "true" || len(filters["label"]) != 2 || filters["label"][0] != "io.knotra.engine="+resource.EngineID || filters["label"][1] != "io.knotra.host="+resource.HostID {
			t.Errorf("unbounded or unconfined inventory: %s %s", r.Method, r.URL)
		}
		count := calls.Add(1)
		if count == 2 || count == 3 {
			if len(filters["before"]) != 1 || filters["before"][0] != "container-63" {
				t.Errorf("missing pagination anchor: %v", filters)
			}
		}
		if count == 3 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var containers []map[string]any
		size := 1
		if count == 1 {
			size = 64
		}
		if count == 5 {
			size = 65
		}
		for i := range size {
			containers = append(containers, map[string]any{"Id": fmt.Sprintf("container-%d", i), "Labels": resource.Labels()})
		}
		if count == 4 {
			foreign := resource.Labels()
			foreign["io.knotra.host"] = "foreign"
			containers = append(containers, map[string]any{"Id": "foreign", "Labels": foreign}, map[string]any{"Id": "invalid", "Labels": map[string]string{"io.knotra.engine": resource.EngineID}})
		}
		if err := json.NewEncoder(w).Encode(containers); err != nil {
			t.Error(err)
		}
	})}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	}()
	t.Cleanup(func() { _ = server.Close() })
	runner := &Runner{EngineID: resource.EngineID, HostID: resource.HostID, DockerHost: "unix://" + socket}
	page, cursor, err := runner.ResourcePage(t.Context(), "")
	if err != nil || len(page) != 64 || cursor != "container-63" || page[0].Ownership != resource.Ownership {
		t.Fatalf("first page=%d cursor=%q error=%v", len(page), cursor, err)
	}
	page, next, err := runner.ResourcePage(t.Context(), cursor)
	if err != nil || len(page) != 1 || next != "" {
		t.Fatalf("last page=%d cursor=%q error=%v", len(page), next, err)
	}
	if _, next, err := runner.ResourcePage(t.Context(), cursor); err == nil || next != "" {
		t.Fatalf("deleted anchor did not reset cursor: %q %v", next, err)
	}
	if page, _, err := runner.ResourcePage(t.Context(), ""); err == nil || len(page) != 1 {
		t.Fatalf("foreign inventory was accepted: %d %v", len(page), err)
	}
	if page, _, err := runner.ResourcePage(t.Context(), ""); err == nil || len(page) != 0 {
		t.Fatalf("oversized inventory was accepted: %d %v", len(page), err)
	}
}
