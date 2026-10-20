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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store"
)

func TestRemoteMCPCleanupSurvivesOwnerHostRemoval(t *testing.T) {
	r := newTestRuntime(t)
	ctx := t.Context()
	owner := uuid.NewString()
	if err := r.Store.RegisterWorker(ctx, owner, "removed-host"); err != nil {
		t.Fatal(err)
	}
	var cancellations, deletions atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Mcp-Session-Id") != "original-session" || request.Header.Get("Mcp-Protocol-Version") != "2025-11-25" {
			t.Error("replacement host changed remote session identity")
		}
		switch request.Method {
		case http.MethodPost:
			var notification struct {
				Method string `json:"method"`
				Params struct {
					RequestID int `json:"requestId"`
				} `json:"params"`
			}
			if err := json.NewDecoder(request.Body).Decode(&notification); err != nil || notification.Method != "notifications/cancelled" || notification.Params.RequestID != 42 {
				t.Errorf("cleanup repeated execution or lost current call: %+v %v", notification, err)
			}
			cancellations.Add(1)
			w.WriteHeader(http.StatusAccepted)
		case http.MethodDelete:
			deletions.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected remote method: %s", request.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer remote.Close()
	plan := deliveryPlan("")
	plan.Profile.Spec.MCP = map[string]contract.MCPConnection{"tools": {Transport: "streamable_http", URL: remote.URL}}
	id := admitRuntimeRun(t, r, plan)
	var claim *execution.Claim
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if err := r.advanceTx(ctx, tx, plan, id, ""); err != nil {
			return err
		}
		var err error
		claim, err = r.Store.ClaimAttempt(ctx, tx, execution.AttemptID{RunID: id, InstanceID: execution.StableID("n", id+"/root/a"), Number: 1}, 1, owner)
		return err
	}); err != nil || claim == nil {
		t.Fatalf("claim=%+v error=%v", claim, err)
	}
	resource, err := r.Store.RegisterOwnedResource(ctx, claim.Ownership, uuid.NewString(), "mcp_http", "run")
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := r.Store.RegisterOwnedResource(ctx, claim.Ownership, uuid.NewString(), "sandbox", "run")
	if err != nil {
		t.Fatal(err)
	}
	resource.MCP = &execution.MCPSessionRecord{Connection: "tools", SessionID: "original-session", ProtocolVersion: "2025-11-25"}
	if err := r.Outcomes.PutMCPSession(resource); err != nil {
		t.Fatal(err)
	}
	if err := r.Outcomes.PutMCPCall(resource, []byte("42"), false); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileResources(ctx); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind='knotra_cleanup_v1'").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("live remote owner was exposed to cleanup: jobs=%d error=%v", pending, err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_workers SET heartbeat_at=clock_timestamp()-interval '21 seconds' WHERE id=$1", owner); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileResources(ctx); err != nil {
		t.Fatal(err)
	}
	var args CleanupArgs
	var encoded []byte
	var queue string
	if err := r.Store.Pool.QueryRow(ctx, "SELECT args,queue FROM river_job WHERE kind='knotra_cleanup_v1'").Scan(&encoded, &queue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &args); err != nil || !args.Portable || args.HostID != "removed-host" || args.ResourceID != resource.ID || queue != r.Client.execute {
		t.Fatalf("remote cleanup lost original ownership/global routing: %+v queue=%s error=%v", args, queue, err)
	}
	for _, portable := range []bool{false, true} {
		if err := r.cleanup(ctx, &river.Job[CleanupArgs]{Args: CleanupArgs{HostID: sandbox.HostID, Portable: portable, ResourceID: sandbox.ID,
			WorkerID: sandbox.Ownership.WorkerID, OwnershipGeneration: sandbox.Ownership.Generation, DispatchGeneration: sandbox.DispatchGeneration}}); err == nil {
			t.Fatalf("foreign sandbox cleanup accepted portable=%t", portable)
		}
	}
	r.Host.Runner.EngineID, r.Host.Runner.HostID, r.Host.Runner.ResourceEvidence = r.Store.EngineID, r.HostID, r.Outcomes
	client, err := New(r.Store.Pool, Config{Role: execution.RoleExecutor, EngineID: r.Store.EngineID, HostID: r.HostID,
		Schema: r.Client.River.Schema(), Logger: r.Client.logger, ExecutionWorkers: 2, ExecutionTimeout: time.Minute},
		Handlers{Execute: func(context.Context, *river.Job[ExecuteArgs]) error { return nil }, Cleanup: r.cleanup})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Stop(stop); err != nil {
			t.Error(err)
		}
	})
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, 5*time.Second, func() (bool, error) {
		var closed bool
		err := r.Store.Pool.QueryRow(ctx, "SELECT state='closed' FROM knotra_resources WHERE id=$1", resource.ID).Scan(&closed)
		return closed, err
	})
	var host, worker, sandboxState string
	var generation int64
	if err := r.Store.Pool.QueryRow(ctx, "SELECT host_id,worker_id,ownership_generation,(SELECT state FROM knotra_resources WHERE id=$2) FROM knotra_resources WHERE id=$1", resource.ID, sandbox.ID).Scan(&host, &worker, &generation, &sandboxState); err != nil {
		t.Fatal(err)
	}
	if host != "removed-host" || worker != owner || generation != claim.Generation || sandboxState != "active" || cancellations.Load() != 1 || deletions.Load() != 1 {
		t.Fatalf("cleanup changed ownership or sandbox locality: host=%s worker=%s generation=%d sandbox=%s cancellations=%d deletions=%d", host, worker, generation, sandboxState, cancellations.Load(), deletions.Load())
	}
}

func TestResourceScannerPagesPastLiveSessionsAndRepairsNativeDelivery(t *testing.T) {
	r := newTestRuntime(t)
	ctx := t.Context()
	owner := uuid.NewString()
	if err := r.Store.RegisterWorker(ctx, owner, r.HostID); err != nil {
		t.Fatal(err)
	}
	plan := deliveryPlan("")
	id := admitRuntimeRun(t, r, plan)
	var claim *execution.Claim
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if err := r.advanceTx(ctx, tx, plan, id, ""); err != nil {
			return err
		}
		var err error
		claim, err = r.Store.ClaimAttempt(ctx, tx, execution.AttemptID{RunID: id, InstanceID: execution.StableID("n", id+"/root/a"), Number: 1}, 1, owner)
		return err
	}); err != nil || claim == nil {
		t.Fatalf("claim=%+v error=%v", claim, err)
	}
	for range 65 {
		key := "00000000-" + uuid.NewString()[9:]
		if _, err := r.Store.RegisterOwnedResource(ctx, claim.Ownership, key, "sandbox", "run"); err != nil {
			t.Fatal(err)
		}
	}
	key := "ffffffff-ffff-4fff-afff-" + uuid.NewString()[24:]
	if _, err := r.Store.RegisterOwnedResource(ctx, claim.Ownership, key, "sandbox", "attempt"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_execution_attempts SET state='completed' WHERE run_id=$1", id); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileResources(ctx); err != nil || r.resourceCursor == "" {
		t.Fatalf("first page cursor=%q error=%v", r.resourceCursor, err)
	}
	for _, state := range []string{"initial", "completed", "cancelled", "discarded", "deleted"} {
		if state != "initial" {
			if state == "deleted" {
				_, err := r.Store.Pool.Exec(ctx, "DELETE FROM river_job WHERE kind='knotra_cleanup_v1'")
				if err != nil {
					t.Fatal(err)
				}
			} else if _, err := r.Store.Pool.Exec(ctx, "UPDATE river_job SET state=$1,finalized_at=clock_timestamp() WHERE kind='knotra_cleanup_v1'", state); err != nil {
				t.Fatal(err)
			}
			if err := r.reconcileResources(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.reconcileResources(ctx); err != nil {
			t.Fatal(err)
		}
		var queued, live, attempts int
		if err := r.Store.Pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM river_job WHERE kind='knotra_cleanup_v1' AND state='available'),
			(SELECT count(*) FROM knotra_resources WHERE lifetime='run' AND state='active'),
			(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1)`, id).Scan(&queued, &live, &attempts); err != nil || queued != 1 || live != 65 || attempts != 1 {
			t.Fatalf("native state=%s queued=%d live=%d attempts=%d error=%v", state, queued, live, attempts, err)
		}
	}
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		resource, err := r.Store.ClaimResourceCleanupTx(ctx, tx, key, r.HostID, owner, claim.Generation, 2)
		if resource != nil {
			return fmt.Errorf("stale cleanup generation claimed %+v", resource)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCodeRegistersSandboxOwnerBeforeCreationAndClosesIt(t *testing.T) {
	helper := os.Getenv("KNOTRA_TEST_HELPER")
	if helper == "" {
		t.Skip("set KNOTRA_TEST_HELPER for real Docker resource ownership check")
	}
	r := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	plan := resourceCodePlan(t, r, 5*time.Second)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	id := admitRuntimeRun(t, r, plan)
	var resource, worker, host string
	var generation int64
	await(t, 15*time.Second, func() (bool, error) {
		var count int
		err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_resources WHERE run_id=$1", id).Scan(&count)
		return count == 1, err
	})
	if err := r.Store.Pool.QueryRow(ctx, "SELECT id,worker_id,host_id,ownership_generation FROM knotra_resources WHERE run_id=$1", id).Scan(&resource, &worker, &host, &generation); err != nil {
		t.Fatal(err)
	}
	for _, cleanupHost := range []string{host, "foreign-host"} {
		if got, err := r.Store.ClaimResourceCleanup(ctx, resource, cleanupHost, worker, generation); err != nil || got != nil {
			t.Fatalf("live sandbox cleanup=%+v error=%v", got, err)
		}
	}
	var labels map[string]string
	await(t, 10*time.Second, func() (bool, error) {
		output, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .Config.Labels}}", "sandbox-"+resource).Output()
		if err != nil {
			return false, nil //nolint:nilerr // The container may not exist yet; await its creation within the test timeout.
		}
		err = json.Unmarshal(output, &labels)
		return err == nil, err
	})
	if labels["io.knotra.resource"] != resource || labels["io.knotra.worker"] != worker || labels["io.knotra.host"] != host || labels["io.knotra.engine"] != r.Store.EngineID || labels["io.knotra.run"] != id || labels["io.knotra.generation"] != "1" || labels["io.knotra.attempt"] != "1" {
		t.Fatalf("incomplete sandbox identity: %+v", labels)
	}
	await(t, 20*time.Second, func() (bool, error) {
		status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
		return protocol.Terminal(status), err
	})
	run, err := r.Store.Run(ctx, id)
	if err != nil || run.Status != "succeeded" || string(run.Outputs["result"]) != `"confirmed"` {
		t.Fatalf("run=%+v error=%v", run, err)
	}
	var state string
	var budget int
	if err := r.Store.Pool.QueryRow(ctx, "SELECT state,(SELECT used FROM knotra_budgets WHERE run_id=$1 AND kind='tool' AND scope=$1) FROM knotra_resources WHERE run_id=$1", id).Scan(&state, &budget); err != nil || state != "closed" || budget != 1 {
		t.Fatalf("resource state=%s budget=%d error=%v", state, budget, err)
	}
	if output, err := exec.CommandContext(ctx, "docker", "inspect", "sandbox-"+resource).CombinedOutput(); err == nil {
		t.Fatalf("closed resource retained container: %s", output)
	}
}

func resourceCodePlan(t *testing.T, r *Runtime, delay time.Duration) contract.Plan {
	t.Helper()
	helper, err := filepath.Abs(os.Getenv("KNOTRA_TEST_HELPER"))
	if err != nil {
		t.Fatal(err)
	}
	r.Host.Runner.EngineID, r.Host.Runner.HostID = r.Store.EngineID, r.HostID
	r.Host.Runner.HelperPath = helper
	if r.Host.Runner.WorkDir == "" {
		r.Host.Runner.WorkDir = t.TempDir()
	}
	plan := deliveryPlan("")
	spec := &plan.Pipelines[plan.Root].Spec
	spec.Models = nil
	plan.Profile.Spec.Models = nil
	spec.Sandboxes = map[string]contract.SandboxResource{"box": {Profile: "local"}}
	plan.Profile.Spec.Sandboxes = map[string]contract.SandboxProfile{"local": {Image: "python:3.13-alpine", Resources: contract.Resources{CPU: 1, MemoryMiB: 256, DiskMiB: 64, Pids: 64}, Network: contract.Network{Mode: "none"}, AllowedTools: []string{"process.exec", "files.read", "files.write"}}}
	node := spec.Nodes["a"]
	node.Type, node.LLM, node.Sandbox = "code", nil, "box"
	node.Execution.Timeout = "50s"
	node.Code = &contract.CodeNode{Command: []string{"python", "-c", fmt.Sprintf(`import json,os,time;time.sleep(%g);json.dump({"answer":"confirmed"},open(os.environ["KNOTRA_OUTPUT_JSON"],"w"))`, delay.Seconds())}}
	spec.Nodes["a"] = node
	if err := r.Host.Runner.Prepare(t.Context(), &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}
