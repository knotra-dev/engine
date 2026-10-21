package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store/db"
)

var ErrExecutionVersion = errors.New("unsupported scheduler or state format version")

// EnqueueWake composes queue insertion with a domain mutation without making
// storage depend on River. The bridge must insert the given generation in tx.
type EnqueueWake func(context.Context, pgx.Tx, string, int64) error

// PutRiverRun admits a new run and its first delivery in the caller's command
// transaction. A savepoint also prevents a failed queue insertion leaving a
// partial admission if a caller subsequently commits its outer transaction.
func PutRiverRun(ctx context.Context, tx pgx.Tx, run protocol.Run, plan contract.Plan, inputs contract.Values, enqueue EnqueueWake) (protocol.Run, error) {
	if enqueue == nil {
		return run, errors.New("river admission requires transactional enqueue")
	}
	if run.Status != "pending" {
		return run, errors.New("new scheduler admission must be pending")
	}
	if plan.Version != "knotra/v1" || plan.CompilerVersion != contract.CompilerVersion || plan.CELVersion != contract.CELVersion {
		return run, ErrExecutionVersion
	}

	root := plan.Pipelines[plan.Root]
	if root == nil || plan.Digest == "" {
		return run, errors.New("river admission requires a frozen root plan and digest")
	}

	inputs, err := contract.ValidatePorts(root.Spec.Inputs, inputs, true)
	if err != nil {
		return run, err
	}

	limits := execution.RestrictLimits(plan.Profile.Spec.Limits, root.Spec.Limits)
	duration, err := contract.Duration(limits.Timeout)
	if err != nil || duration <= 0 {
		return run, errors.New("invalid admitted execution timeout")
	}

	inner, err := tx.Begin(ctx)
	if err != nil {
		return run, err
	}

	defer func() { _ = inner.Rollback(ctx) }()
	var admitted time.Time
	storedDatabaseTime, err := db.New(inner).DatabaseTime(ctx)
	if err != nil {
		return run, err
	}

	admitted = storedDatabaseTime

	run.CreatedAt = admitted.UTC()
	run.UpdatedAt = run.CreatedAt
	deadline := admitted.Add(duration)
	if err := insertRun(ctx, inner, run, plan, inputs, admitted, deadline); err != nil {
		return run, err
	}

	b, err := json.Marshal(limits)
	if err != nil {
		return run, err
	}
	if _, err := db.New(inner).InsertRootScope(ctx, db.InsertRootScopeParams{RunID: run.ID, Limits: b}); err != nil {
		return run, err
	}
	if _, err := db.New(inner).InsertRootDeadline(ctx, db.InsertRootDeadlineParams{RunID: run.ID, DueAt: deadline}); err != nil {
		return run, err
	}
	if err := InsertExecutionGraph(ctx, inner, execution.RootGraph(run.ID, plan.Root, inputs, limits, deadline)); err != nil {
		return run, err
	}
	if err := enqueue(ctx, inner, run.ID, 1); err != nil {
		return run, err
	}

	return run, inner.Commit(ctx)
}

// LockExecutionRun establishes the mandatory mutation lock order: run, graph,
// nodes in lexical identity order, attempts, workers, operations, scopes in
// lexical order, then reservations and timers. Different runs do not block each other. Database
// time is captured after the lock so waiting cannot extend a deadline.
func LockExecutionRun(ctx context.Context, tx pgx.Tx, id string) (execution.RunState, error) {
	r := execution.RunState{RunID: id}

	storedExecutionRun, err := db.New(tx).LockExecutionRun(ctx, id)
	if err == nil {
		r.Backend = storedExecutionRun.Backend
		r.SchedulerVersion = storedExecutionRun.SchedulerVersion
		r.StateFormatVersion = storedExecutionRun.StateFormatVersion
		r.AdmittedAt = storedExecutionRun.AdmittedAt

		r.Revision = storedExecutionRun.StateRevision
		r.WakeGeneration = storedExecutionRun.WakeGeneration
		r.AppliedGeneration = storedExecutionRun.AppliedGeneration
		r.Cancelled = storedExecutionRun.CancelRequested
		r.Paused = storedExecutionRun.AdmissionPaused

		r.Status = storedExecutionRun.Status
		r.GraphCursor = storedExecutionRun.GraphCursor
		r.GraphScanAgain = storedExecutionRun.GraphScanAgain
		r.DeliveryCursor = storedExecutionRun.DeliveryCursor
		r.SessionAffinity = storedExecutionRun.SessionAffinity
		r.SessionWorkerID = storedExecutionRun.SessionWorkerID
		r.Now = storedExecutionRun.Now
	}
	if err != nil {
		return r, classify(err)
	}

	r.Deadline = storedExecutionRun.ExecutionDeadline
	if len(storedExecutionRun.StopCause) > 0 {
		if err := json.Unmarshal(storedExecutionRun.StopCause, &r.StopCause); err != nil {
			return r, err
		}
	}
	return r, nil
}

func checkExecutionVersion(run execution.RunState) error {
	if run.Backend != execution.BackendRiver {
		return ErrConflict
	}
	if run.SchedulerVersion != execution.SchedulerVersion || run.StateFormatVersion != execution.StateFormatVersion {
		return ErrExecutionVersion
	}
	return nil
}

// WakeExecution increments an explicit generation and enqueues it atomically.
// A completed River job can never suppress a later domain wakeup generation.
func WakeExecution(ctx context.Context, tx pgx.Tx, id string, enqueue EnqueueWake) (int64, error) {
	if enqueue == nil {
		return 0, errors.New("wake requires transactional enqueue")
	}

	inner, err := tx.Begin(ctx)
	if err != nil {
		return 0, err
	}

	defer func() { _ = inner.Rollback(ctx) }()
	run, err := LockExecutionRun(ctx, inner, id)
	if err != nil {
		return 0, err
	}
	if err := checkExecutionVersion(run); err != nil {
		return 0, err
	}
	if protocol.Terminal(run.Status) {
		return run.WakeGeneration, nil
	}

	var generation int64
	storedWakeExecution, err := db.New(inner).WakeExecution(ctx, id)
	if err != nil {
		return 0, err
	}

	generation = storedWakeExecution

	if err := enqueue(ctx, inner, id, generation); err != nil {
		return 0, err
	}

	return generation, inner.Commit(ctx)
}

// ApplyWake records all changes consumed by a bounded scheduler transaction.
// The bridge must complete its delivery in the same transaction. If bounded
// work remains it calls WakeExecution before commit to create the next step.
func ApplyWake(ctx context.Context, tx pgx.Tx, id string, generation int64) (bool, error) {
	run, err := LockExecutionRun(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if err := checkExecutionVersion(run); err != nil {
		return false, err
	}
	if generation < 1 || generation > run.WakeGeneration {
		return false, ErrConflict
	}
	if generation <= run.AppliedGeneration {
		return false, nil
	}
	_, err = db.New(tx).ApplyWake(ctx, id)
	return err == nil, err
}

// ProjectExecution allocates an event key and writes the public view alongside
// authoritative state in the same transaction. Complete graph/node values are
// persisted independently, before calling this bounded observation projection.
func (s *Store) ProjectExecution(ctx context.Context, tx pgx.Tx, p execution.Projection) error {
	return s.ProjectExecutionBatch(ctx, tx, []execution.Projection{p})
}

// ProjectExecutionBatch allocates fresh sequences and folds a scheduler step's
// ordered projections under one run lock, writing its public run document once.
func (s *Store) ProjectExecutionBatch(ctx context.Context, tx pgx.Tx, projections []execution.Projection) error {
	if len(projections) == 0 {
		return nil
	}
	id := projections[0].RunID
	for _, p := range projections {
		if p.RunID != id || p.Sequence != 0 {
			return fmt.Errorf("scheduler projections require one run and transaction-allocated sequences")
		}
	}
	run, err := LockExecutionRun(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := checkExecutionVersion(run); err != nil {
		return err
	}
	last, err := db.New(tx).NextExecutionEventSequence(ctx, db.NextExecutionEventSequenceParams{ID: id, Count: int64(len(projections))})
	if err != nil {
		return err
	}
	stored, err := db.New(tx).LockRunProjection(ctx, id)
	if err != nil {
		return err
	}
	var public protocol.Run
	if err := json.Unmarshal(stored.Document, &public); err != nil {
		return err
	}
	sequence := stored.Sequence
	for i, p := range projections {
		p.Sequence = last - int64(len(projections)-1-i)
		if p.Time.IsZero() {
			p.Time = run.Now
		}
		if err := projectEvent(ctx, tx, p, &public, &sequence); err != nil {
			return err
		}
	}
	b, err := raw(public)
	if err != nil {
		return err
	}
	_, err = db.New(tx).UpdateRunProjection(ctx, db.UpdateRunProjectionParams{ID: id, Document: b, Sequence: sequence})
	return err
}
