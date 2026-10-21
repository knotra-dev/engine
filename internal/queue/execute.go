package queue

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/store"
	"github.com/knotra-dev/engine/internal/store/db"
	"github.com/knotra-dev/engine/internal/telemetry"
)

func (r *Runtime) execute(ctx context.Context, job *river.Job[ExecuteArgs]) error {
	// Both execution queues share the process's physical execution limit.
	select {
	case r.Client.executionSlots <- struct{}{}:
		defer func() { <-r.Client.executionSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	var claim *execution.Claim
	claimCtx, stopClaim := context.WithTimeout(ctx, 5*time.Second)
	err := pgx.BeginFunc(claimCtx, r.Store.Pool, func(tx pgx.Tx) error {
		var err error
		claim, err = r.Store.ClaimAttempt(claimCtx, tx, job.Args.Attempt, job.Args.DispatchGeneration, r.WorkerID)
		if errors.Is(err, store.ErrExecutionAffinity) {
			if _, err := store.LockExecutionRun(claimCtx, tx, job.Args.Attempt.RunID); err != nil {
				return err
			}
			if _, err := db.New(tx).MarkPausedAttemptForDispatch(claimCtx, db.MarkPausedAttemptForDispatchParams{
				RunID: job.Args.Attempt.RunID, InstanceID: job.Args.Attempt.InstanceID, Number: job.Args.Attempt.Number, DispatchGeneration: job.Args.DispatchGeneration,
			}); err != nil {
				return err
			}
			if _, err := store.WakeExecution(claimCtx, tx, job.Args.Attempt.RunID, r.Wake); err != nil {
				return err
			}
			_, err = river.JobCompleteTx[*riverpgxv5.Driver](claimCtx, tx, job)
			return err
		}
		if err != nil {
			return err
		}
		if claim == nil {
			_, err = river.JobCompleteTx[*riverpgxv5.Driver](claimCtx, tx, job)
		}
		return err
	})
	stopClaim()
	if err != nil || claim == nil {
		return err
	}
	trace.SpanFromContext(ctx).SetAttributes(telemetry.OwnershipAttributes(claim.Ownership)...)
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("knotra.actor.id", r.WorkerID))
	work, cancel := context.WithCancelCause(ctx)
	leaseDone := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(leaseDone)
		ticker := time.NewTicker(execution.WorkerHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-finished:
				return
			case <-work.Done():
				return
			case <-ticker.C:
			}
			renewCtx, stop := context.WithTimeout(work, execution.WorkerHeartbeat)
			err := pgx.BeginFunc(renewCtx, r.Store.Pool, func(tx pgx.Tx) error {
				_, err := r.Store.RenewAttempt(renewCtx, tx, claim.Ownership)
				return err
			})
			stop()
			if err != nil {
				cancel(err)
				return
			}
		}
	}()
	// Durable upload and publication still need the attempt's live ownership.
	defer func() {
		close(finished)
		<-leaseDone
		cancel(context.Canceled)
	}()
	var result execution.ExecuteResult
	if claim.SessionWorkerLost {
		result.Failure = &execution.Failure{Code: "OUTCOME_UNKNOWN", Message: "run-scoped MCP worker was lost; session state cannot be reconstructed", Unknown: true,
			OperationID: claim.RunID + "/session"}
	} else {
		result = r.Host.Execute(work, claim.Request)
	}
	outcome := execution.Outcome{FormatVersion: execution.StateFormatVersion, Ownership: claim.Ownership, PlanID: claim.PlanID,
		CompletedAt: time.Now().UTC(), Outputs: result.Outputs, Failure: result.Failure, Artifacts: outcomeArtifacts(result.Outputs)}
	key, err := r.Outcomes.Put(outcome)
	if err != nil {
		return err
	}
	// Publication has its own bounded context. A cancelled execution context
	// must not discard the durable envelope or prevent a finalization handoff.
	publication, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	err = r.publish(publication, outcome, false, func(tx pgx.Tx) error {
		_, err := river.JobCompleteTx[*riverpgxv5.Driver](publication, tx, job)
		return err
	})
	if err == nil {
		return nil
	}
	handoff, stopHandoff := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stopHandoff()
	queueErr := pgx.BeginFunc(handoff, r.Store.Pool, func(tx pgx.Tx) error {
		return r.Client.InsertFinalize(handoff, tx, FinalizeArgs{Attempt: claim.AttemptID, OutcomeKey: key, RoutingVersion: RoutingVersion})
	})
	return errors.Join(err, queueErr)
}
