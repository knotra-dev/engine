//go:build unix

package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store"
)

func TestThreeExecutorProcessesShareWorkAndSurviveDrain(t *testing.T) {
	r := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	directory := t.TempDir()
	files, err := execution.OpenOutcomeFiles(filepath.Join(directory, "outcomes"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	r.Outcomes = files
	r.Host.Artifacts.Root = filepath.Join(directory, "artifacts")
	schema := r.Store.Pool.Config().ConnConfig.RuntimeParams["search_path"]
	r.Client, err = New(r.Store.Pool, Config{Role: execution.RoleScheduler, EngineID: r.Store.EngineID,
		HostID: r.HostID, Schema: schema, Logger: r.Client.logger, ExecutionTimeout: time.Minute}, r.Handlers())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	var currentPhase atomic.Int32
	releases := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var releaseOnce [2]sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/api/chat" {
			t.Errorf("unexpected provider request: %s %s", req.Method, req.URL.Path)
		}
		calls.Add(1)
		select {
		case <-releases[currentPhase.Load()]:
		case <-req.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"role": "assistant", "content": `{"answer":"three-worker result"}`},
			"done":    true, "done_reason": "stop",
		})
	}))
	defer server.Close()
	defer func() {
		for i := range releases {
			releaseOnce[i].Do(func() { close(releases[i]) })
		}
	}()

	children := make([]*exec.Cmd, 3)
	for i := range children {
		host := fmt.Sprintf("executor-%d", i+1)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimeProcessCrashBoundaries$", "-test.timeout=2m")
		cmd.Env = append(os.Environ(), "KNOTRA_RUNTIME_CHILD=1", "KNOTRA_RUNTIME_MODE=executor",
			"KNOTRA_RUNTIME_ROLE=executor", "KNOTRA_RUNTIME_HOST="+host,
			"KNOTRA_RUNTIME_DSN="+r.Store.Pool.Config().ConnString(), "KNOTRA_RUNTIME_DIRECTORY="+directory,
			"KNOTRA_RUNTIME_TIMEOUT=1m")
		logPath := filepath.Join(directory, host+".log")
		log, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			_ = log.Close()
			t.Fatal(err)
		}
		_ = log.Close()
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			if t.Failed() {
				data, _ := os.ReadFile(logPath)
				t.Logf("%s: %s", host, data)
			}
		})
		children[i] = cmd
		t.Logf("%s pid=%d", host, cmd.Process.Pid)
	}
	await(t, 15*time.Second, func() (bool, error) {
		var ready int
		err := r.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM knotra_workers
			WHERE capabilities->>'role'='executor' AND delivery_client_id IS NOT NULL AND NOT draining`).Scan(&ready)
		return ready == 3, err
	})

	for phase, count := range []int{12, 6} {
		currentPhase.Store(int32(phase))
		before := calls.Load()
		runs := make([]string, count)
		for i := range runs {
			runs[i] = admitRuntimeRun(t, r, runtimePlan(server.URL))
		}
		// Hold one physical call per process to prove every live executor participates.
		await(t, 15*time.Second, func() (bool, error) {
			var owners int
			err := r.Store.Pool.QueryRow(ctx, `SELECT count(DISTINCT owner) FROM knotra_execution_attempts
				WHERE state='claimed' AND run_id=ANY($1)`, runs).Scan(&owners)
			return owners == 3-phase && calls.Load() == before+int32(3-phase), err
		})
		releaseOnce[phase].Do(func() { close(releases[phase]) })
		await(t, 30*time.Second, func() (bool, error) {
			for _, id := range runs {
				status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
				if err != nil || !protocol.Terminal(status) {
					return false, err
				}
			}
			return true, nil
		})
		for _, id := range runs {
			run, err := r.Store.Run(ctx, id)
			if err != nil || run.Status != "succeeded" || len(run.Instances) != 3 || string(run.Outputs["result"]) != `"three-worker result"` {
				t.Fatalf("run=%+v error=%v", run, err)
			}
		}
		rows, err := r.Store.Pool.Query(ctx, `SELECT w.host_id,count(*) FROM knotra_execution_attempts a
			JOIN knotra_workers w ON w.id=a.owner WHERE a.run_id=ANY($1) GROUP BY w.host_id ORDER BY w.host_id`, runs)
		if err != nil {
			t.Fatal(err)
		}
		owners := 0
		for rows.Next() {
			var host string
			var attempts int
			if err := rows.Scan(&host, &attempts); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if phase == 1 && host == "executor-1" {
				t.Error("drained executor received new work")
			}
			owners++
			t.Logf("phase=%d %s completed=%d", phase+1, host, attempts)
		}
		rows.Close()
		if err := rows.Err(); err != nil || owners != 3-phase {
			t.Fatalf("phase=%d owners=%d error=%v", phase+1, owners, err)
		}
		if phase == 0 {
			if err := writeSignal(directory, "stop-executor-1"); err != nil {
				t.Fatal(err)
			}
			if err := children[0].Wait(); err != nil {
				t.Fatal(err)
			}
		}
	}
	var attempts, invalid, debits, slots int
	if err := r.Store.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE state<>'completed' OR number<>1),
		(SELECT sum(used) FROM knotra_budgets WHERE kind='model' AND scope=run_id),
		(SELECT count(*) FROM knotra_execution_slots WHERE released_at IS NULL) FROM knotra_execution_attempts`).Scan(&attempts, &invalid, &debits, &slots); err != nil {
		t.Fatal(err)
	}
	if attempts != 54 || calls.Load() != 54 || debits != 54 || invalid != 0 || slots != 0 {
		t.Fatalf("attempts=%d calls=%d debits=%d invalid=%d slots=%d", attempts, calls.Load(), debits, invalid, slots)
	}
	for i := 1; i < len(children); i++ {
		if err := writeSignal(directory, fmt.Sprintf("stop-executor-%d", i+1)); err != nil {
			t.Fatal(err)
		}
		if err := children[i].Wait(); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("18 workflows, 54 physical calls and matching debits; three executors, then two after graceful drain")
}
