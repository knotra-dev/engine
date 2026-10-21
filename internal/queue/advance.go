package queue

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store"
	"github.com/knotra-dev/engine/internal/store/db"
)

const stepTransitions = 64

// Wake ensures a validated pending scheduler delivery in the caller's transaction.
func (r *Runtime) Wake(ctx context.Context, tx pgx.Tx, runID string, generation int64) error {
	args := AdvanceArgs{RunID: runID, WakeGeneration: generation, RoutingVersion: RoutingVersion}
	if err := args.Validate(); err != nil {
		return err
	}
	run, err := store.LockExecutionRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	pending, err := r.Client.hasPendingAdvance(ctx, tx, run)
	if err != nil || pending {
		return err
	}
	return r.Client.InsertAdvance(ctx, tx, args)
}

// CommandWake consumes a validated request/stop command in its receipt
// transaction, then inserts its durable delivery. No leaf work runs here.
func (r *Runtime) CommandWake(ctx context.Context, tx pgx.Tx, runID string, generation int64) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	plan, err := store.ReadPlan(ctx, tx, runID)
	if err != nil {
		return err
	}
	if err := r.advanceTx(ctx, tx, plan, runID, ""); err != nil {
		return err
	}
	return r.Wake(ctx, tx, runID, generation)
}

func (r *Runtime) advance(ctx context.Context, job *river.Job[AdvanceArgs]) error {
	plan, err := r.Store.Plan(ctx, job.Args.RunID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		applied, err := store.ApplyWake(ctx, tx, job.Args.RunID, job.Args.WakeGeneration)
		if err != nil {
			return err
		}
		if applied {
			if err := r.advanceTx(ctx, tx, plan, job.Args.RunID, ""); err != nil {
				return err
			}
		}
		_, err = river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job)
		return err
	})
}

func (r *Runtime) advanceTx(ctx context.Context, tx pgx.Tx, plan contract.Plan, runID, focusInstanceID string) error {
	ctx, span := otel.Tracer("knotra/scheduler").Start(ctx, "scheduler.advance", trace.WithAttributes(
		attribute.String("knotra.run.id", runID), attribute.String("knotra.instance.id", focusInstanceID),
		attribute.String("knotra.engine.id", r.Store.EngineID), attribute.String("knotra.host.id", r.HostID), attribute.String("knotra.actor.id", r.WorkerID)))
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	run, err := store.LockExecutionRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	if run.Backend != execution.BackendRiver || run.SchedulerVersion != execution.SchedulerVersion || run.StateFormatVersion != execution.StateFormatVersion {
		return store.ErrExecutionVersion
	}
	span.SetAttributes(attribute.Int64("knotra.wake.generation", run.WakeGeneration))
	if protocol.Terminal(run.Status) {
		return nil
	}
	graphs, err := db.New(tx).ListRunnableGraphs(ctx, db.ListRunnableGraphsParams{RunID: runID, GraphCursor: run.GraphCursor, FocusInstanceID: focusInstanceID})
	if err != nil {
		return err
	}
	// Publication visits its affected graph without moving the shared scan
	// cursor. A durable general wake propagates parent results and freed slots.
	remaining, more, progress, visited := stepTransitions, focusInstanceID != "", false, 0
	for _, graph := range graphs {
		if remaining == 0 {
			more = true
			break
		}
		changes, err := r.advanceGraphTx(ctx, tx, plan, runID, graph.ID, focusInstanceID, remaining)
		if err != nil {
			return err
		}
		remaining -= changes.Transitions
		visited++
		progress = progress || changes.Transitions > 0 || changes.Graph != nil
		more = more || changes.More || (changes.Graph != nil && changes.Graph.ParentInstanceID != "" && protocol.Terminal(changes.Graph.State))
		if focusInstanceID == "" {
			if _, err := db.New(tx).SetGraphCursor(ctx, db.SetGraphCursorParams{ID: runID, GraphCursor: graph.Address}); err != nil {
				return err
			}
		}
	}
	if progress {
		if _, err := db.New(tx).MarkGraphScanProgress(ctx, runID); err != nil {
			return err
		}
	}
	if focusInstanceID == "" && visited == len(graphs) {
		if len(graphs) == stepTransitions {
			more = true
		} else {
			run, err = store.LockExecutionRun(ctx, tx, runID)
			if err != nil {
				return err
			}
			more = more || run.GraphScanAgain
			if _, err := db.New(tx).FinishGraphScan(ctx, runID); err != nil {
				return err
			}
		}
	}
	root, err := store.ReadExecutionGraphMetadata(ctx, tx, runID, execution.StableID("g", runID+"/root"))
	if err != nil {
		return err
	}
	if protocol.Terminal(root.State) {
		active, err := db.New(tx).HasActiveExecution(ctx, runID)
		if err != nil {
			return err
		}
		if !active {
			run, err = store.LockExecutionRun(ctx, tx, runID)
			if err != nil {
				return err
			}
			return r.Store.ProjectExecution(ctx, tx, execution.Projection{RunID: runID, Kind: "run", Status: root.State,
				Outputs: root.Outputs, Failure: root.Failure, Time: run.Now})
		}
	}
	if more {
		_, err = store.WakeExecution(ctx, tx, runID, r.Wake)
	}
	return err
}
