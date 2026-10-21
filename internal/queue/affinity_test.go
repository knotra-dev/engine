package queue

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/riverqueue/river"

	"github.com/knotra-dev/engine/internal/adapters"
	"github.com/knotra-dev/engine/internal/api"
	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/executor"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store"
)

func TestRunMCPSessionStaysOnWorkerAcrossHandoffDrainAndLoss(t *testing.T) {
	for _, mode := range []string{"handoff", "drain", "lost"} {
		t.Run(mode, func(t *testing.T) {
			owner := newTestRuntime(t)
			ctx := context.Background()
			var calls atomic.Int32
			var session atomic.Value
			server := mcp.NewServer(&mcp.Implementation{Name: "affinity-test", Version: "1"}, nil)
			server.AddTool(
				&mcp.Tool{Name: "work", InputSchema: map[string]any{"type": "object"}},
				func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					if previous, ok := session.Load().(string); ok && previous != req.Session.ID() {
						t.Error("run-scoped MCP state moved to another session")
					}
					session.Store(req.Session.ID())
					return &mcp.CallToolResult{StructuredContent: map[string]int32{"calls": calls.Add(1)}}, nil
				},
			)
			remote := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
			t.Cleanup(remote.Close)
			plan := runtimePlan("")
			port := contract.Port{Schema: json.RawMessage(`{"type":"object"}`)}
			first := contract.Node{
				Type:    "tool",
				Tool:    &contract.ToolNode{Server: "tools", Name: "work", Arguments: contract.Binding{Value: json.RawMessage(`{}`)}},
				Outputs: map[string]contract.Port{"result": port},
			}
			second := first
			second.Dependencies = []string{"review"}
			review := contract.Node{
				Type:         "human",
				Dependencies: []string{"first"},
				Human:        &contract.HumanNode{Prompt: contract.TextSource{Text: "Continue?"}},
				Outputs:      map[string]contract.Port{"approved": {Schema: json.RawMessage(`{"type":"boolean"}`)}},
			}
			export := port
			export.Bind = &contract.Binding{From: "nodes.second.outputs.result"}
			spec := &plan.Pipelines[plan.Root].Spec
			spec.Models = nil
			spec.MCP = map[string]contract.MCPResource{"tools": {Connection: "local", Session: "run"}}
			spec.Graph = contract.Graph{
				Nodes:   map[string]contract.Node{"first": first, "review": review, "second": second},
				Outputs: map[string]contract.Port{"result": export},
			}
			plan.Profile.Spec.MCP = map[string]contract.MCPConnection{"local": {Transport: "streamable_http", URL: remote.URL, AllowRunSession: true,
				AllowedTools: []string{"work"}, ToolPolicies: map[string]contract.ToolPolicy{"work": {Effect: "read"}}}}
			if err := owner.Host.Runner.Prepare(ctx, &plan); err != nil {
				t.Fatal(err)
			}
			release := make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			if mode == "handoff" {
				handlers := owner.Handlers()
				handlers.Execute = func(ctx context.Context, job *river.Job[ExecuteArgs]) error {
					if job.Args.Attempt.InstanceID == execution.StableID("n", job.Args.Attempt.RunID+"/root/second") {
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return owner.execute(ctx, job)
				}
				var err error
				owner.Client, err = New(
					owner.Store.Pool,
					Config{
						EngineID:         owner.Store.EngineID,
						HostID:           owner.HostID,
						Schema:           owner.Store.Pool.Config().ConnConfig.RuntimeParams["search_path"],
						Logger:           owner.Client.logger,
						ExecutionWorkers: 2,
						ExecutionTimeout: time.Minute,
					},
					handlers,
				)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := owner.Start(ctx); err != nil {
				t.Fatal(err)
			}
			id := admitRuntimeRun(t, owner, plan)
			await(t, 5*time.Second, func() (bool, error) {
				var waiting bool
				err := owner.Store.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM knotra_requests WHERE run_id=$1 AND kind='human' AND status='open')", id).Scan(&waiting)
				return waiting && calls.Load() == 1, err
			})
			peer := &Runtime{Store: owner.Store, Outcomes: owner.Outcomes, WorkerID: uuid.NewString(), HostID: "affinity-peer",
				Host: &executor.Host{Store: owner.Store, Artifacts: owner.Host.Artifacts, Runner: &adapters.Runner{}}}
			var err error
			peer.Client, err = New(
				owner.Store.Pool,
				Config{
					EngineID:         owner.Store.EngineID,
					HostID:           peer.HostID,
					Schema:           owner.Store.Pool.Config().ConnConfig.RuntimeParams["search_path"],
					Logger:           owner.Client.logger,
					ExecutionWorkers: 2,
					ExecutionTimeout: time.Minute,
				},
				peer.Handlers(),
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := peer.Start(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := peer.Stop(stop); err != nil {
					t.Error(err)
				}
				_ = peer.Host.Runner.Close()
			})
			var drainDone chan error
			switch mode {
			case "drain":
				drainDone = make(chan error, 1)
				go func() {
					stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					drainDone <- owner.Stop(stop)
				}()
				await(t, 5*time.Second, func() (bool, error) {
					var draining bool
					err := owner.Store.Pool.QueryRow(ctx, "SELECT draining FROM knotra_workers WHERE id=$1", owner.WorkerID).Scan(&draining)
					return draining, err
				})
			case "lost":
				stop, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := owner.Client.Stop(stop); err != nil {
					t.Fatal(err)
				}
				cancel()
				owner.cancel(context.Canceled)
				<-owner.Done()
				if _, err := owner.Store.Pool.Exec(
					ctx,
					"UPDATE knotra_workers SET heartbeat_at=clock_timestamp()-interval '21 seconds' WHERE id=$1",
					owner.WorkerID,
				); err != nil {
					t.Fatal(err)
				}
			}
			reviewID := execution.StableID("h", execution.StableID("n", id+"/root/review")+"/human")
			handler := (&api.Server{Store: peer.Store, Artifacts: peer.Host.Artifacts, Wake: peer.CommandWake}).Handler()
			if response := runtimeCommand(t, handler, "/v1/requests/"+reviewID+"/response", "approve", `{"outputs":{"approved":true}}`); response.Code != 200 {
				t.Fatalf("human response HTTP %d: %s", response.Code, response.Body.String())
			}
			secondID := execution.StableID("n", id+"/root/second")
			if mode == "handoff" {
				await(t, 5*time.Second, func() (bool, error) {
					var ready bool
					err := owner.Store.Pool.QueryRow(
						ctx,
						"SELECT EXISTS(SELECT 1 FROM knotra_execution_attempts WHERE run_id=$1 AND instance_id=$2 AND state='ready')",
						id,
						secondID,
					).Scan(&ready)
					return ready, err
				})
				args := ExecuteArgs{
					Attempt:            execution.AttemptID{RunID: id, InstanceID: secondID, Number: 1},
					DispatchGeneration: 1,
					RoutingVersion:     RoutingVersion,
				}
				if err := pgx.BeginFunc(ctx, peer.Store.Pool, func(tx pgx.Tx) error {
					metadata, err := json.Marshal(args)
					if err != nil {
						return err
					}
					_, err = peer.Client.River.InsertTx(ctx, tx, args, &river.InsertOpts{Queue: peer.Client.affinityExecute, Metadata: metadata})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				await(t, 5*time.Second, func() (bool, error) {
					var generation int64
					err := owner.Store.Pool.QueryRow(
						ctx,
						"SELECT dispatch_generation FROM knotra_execution_attempts WHERE run_id=$1 AND instance_id=$2",
						id,
						secondID,
					).Scan(&generation)
					return generation > 1, err
				})
				once.Do(func() { close(release) })
			}
			await(t, 10*time.Second, func() (bool, error) {
				status, err := store.ReadRunStatus(ctx, peer.Store.Pool, id)
				return protocol.Terminal(status) || status == "waiting_resolution", err
			})
			var affinity, secondOwner string
			var failed *string
			if err := peer.Store.Pool.QueryRow(
				ctx,
				`SELECT r.session_worker_id,a.owner,a.result->'failure'->>'code'
				FROM knotra_runs r JOIN knotra_execution_attempts a ON a.run_id=r.id WHERE r.id=$1 AND a.instance_id=$2`,
				id,
				secondID,
			).Scan(&affinity, &secondOwner, &failed); err != nil {
				t.Fatal(err)
			}
			if affinity != owner.WorkerID {
				t.Fatal("worker loss or redispatch changed the run's session owner")
			}
			if mode == "lost" {
				if failed == nil || *failed != "OUTCOME_UNKNOWN" || calls.Load() != 1 || secondOwner != peer.WorkerID {
					t.Fatalf("lost worker repeated execution: failure=%v calls=%d secondOwner=%s", failed, calls.Load(), secondOwner)
				}
				if response := runtimeCommand(t, handler, "/v1/runs/"+id+"/cancel", "cancel", `{}`); response.Code != 202 {
					t.Fatalf("cancel HTTP %d: %s", response.Code, response.Body.String())
				}
			} else if failed != nil || calls.Load() != 2 || secondOwner != owner.WorkerID {
				t.Fatalf("session moved or duplicated: failure=%v calls=%d secondOwner=%s", failed, calls.Load(), secondOwner)
			}
			if drainDone != nil {
				if err := <-drainDone; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
