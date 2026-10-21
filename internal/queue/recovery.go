package queue

import (
	"context"
	"errors"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store"
	"github.com/knotra-dev/engine/internal/store/db"
)

func (r *Runtime) pumpTimers(ctx context.Context) error {
	ids, err := db.New(r.Store.Pool).ListDueTimerRuns(ctx, r.timerCursor)
	if err != nil {
		return err
	}
	var pollErr error
	for _, id := range ids {
		r.timerCursor = id
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			if _, err := db.New(tx).TryLockTimerRun(ctx, id); errors.Is(err, pgx.ErrNoRows) {
				return nil
			} else if err != nil {
				return err
			}
			run, err := store.LockExecutionRun(ctx, tx, id)
			if err != nil {
				return err
			}
			tag, err := db.New(tx).ConsumeDueTimers(ctx, db.ConsumeDueTimersParams{RunID: id, ConsumedAt: new(run.Now)})
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 || protocol.Terminal(run.Status) {
				return nil
			}
			_, err = store.WakeExecution(ctx, tx, id, r.Wake)
			return err
		}); err != nil {
			pollErr = errors.Join(pollErr, err)
		}
	}
	return pollErr
}

func (r *Runtime) reconcileOutcomes(ctx context.Context) error {
	rows, err := db.New(r.Store.Pool).ListExpiredOutcomeAttempts(ctx, new(r.outcomeCursor))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		r.outcomeCursor = ""
		return nil
	}
	for _, record := range rows {
		if record.OutcomeKey == nil || record.Owner == nil {
			return errors.New("claimed attempt is missing ownership/outcome identity")
		}
		r.outcomeCursor = *record.OutcomeKey
		token := execution.Ownership{AttemptID: execution.AttemptID{RunID: record.RunID, InstanceID: record.InstanceID, Number: record.Number}, WorkerID: *record.Owner, Generation: record.OwnershipGeneration}
		if _, err := r.Outcomes.GetContext(ctx, *record.OutcomeKey); err != nil {
			if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, execution.ErrInvalidOutcome) {
				return err
			}
			// ponytail: missing fenced envelopes stay in the bounded scan until
			// retention; storage notifications can replace polling at high volume.
			if record.State != "claimed" {
				continue
			}
			plan, err := r.Store.Plan(ctx, token.RunID)
			if err != nil {
				return err
			}
			if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
				staged, err := r.Store.StageLostAttempt(ctx, tx, token)
				if err != nil || !staged {
					return err
				}
				if err := r.advanceTx(ctx, tx, plan, token.RunID, token.InstanceID); err != nil {
					return err
				}
				_, err = store.ReleaseAttemptSlots(ctx, tx, token, r.Wake)
				return err
			}); err != nil {
				return err
			}
			continue
		}
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			return r.Client.InsertFinalize(ctx, tx, FinalizeArgs{Attempt: token.AttemptID, OutcomeKey: *record.OutcomeKey, RoutingVersion: RoutingVersion})
		}); err != nil {
			return err
		}
	}
	return nil
}

// reconcileDeliveries repairs infrastructure identities, never business attempts.
func (r *Runtime) reconcileDeliveries(ctx context.Context) error {
	runs, err := db.New(r.Store.Pool).ListDeliveryRepairRuns(ctx, r.deliveryCursor)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		r.deliveryCursor = ""
		return nil
	}
	for _, id := range runs {
		r.deliveryCursor = id
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			run, err := store.LockExecutionRun(ctx, tx, id)
			if err != nil {
				return err
			}
			if run.Backend != execution.BackendRiver || run.SchedulerVersion != execution.SchedulerVersion || run.StateFormatVersion != execution.StateFormatVersion {
				return store.ErrExecutionVersion
			}
			if protocol.Terminal(run.Status) {
				return nil
			}
			if run.WakeGeneration > run.AppliedGeneration {
				viable, err := r.Client.hasPendingAdvance(ctx, tx, run)
				if err != nil {
					return err
				}
				if !viable {
					_, err = store.WakeExecution(ctx, tx, id, r.Wake)
				}
				return err
			}
			if run.Cancelled || run.StopCause != nil || run.Paused || !run.Deadline.After(run.Now) {
				return nil //nolint:nilerr // These states deliberately prohibit redispatch; no delivery repair failed.
			}
			ready, err := db.New(tx).ReadReadyDeliveries(ctx, db.ReadReadyDeliveriesParams{RunID: id, Cursor: run.DeliveryCursor, Now: run.Now})
			if err != nil {
				return err
			}
			repaired, cursor := false, run.DeliveryCursor
			for _, attempt := range ready {
				cursor = attempt.InstanceID
				args := ExecuteArgs{Attempt: execution.AttemptID{RunID: id, InstanceID: attempt.InstanceID, Number: attempt.Number}, DispatchGeneration: attempt.DispatchGeneration, RoutingVersion: RoutingVersion}
				viable, err := r.Client.hasDelivery(ctx, tx, args, run.Now)
				if err != nil {
					return err
				}
				if viable {
					continue
				}
				if _, err := db.New(tx).MarkAttemptForDispatch(ctx, db.MarkAttemptForDispatchParams{RunID: id, InstanceID: attempt.InstanceID, Number: attempt.Number}); err != nil {
					return err
				}
				repaired = true
			}
			if len(ready) < stepTransitions {
				cursor = ""
			}
			if cursor != run.DeliveryCursor {
				if _, err := db.New(tx).SetDeliveryCursor(ctx, db.SetDeliveryCursorParams{ID: id, DeliveryCursor: cursor}); err != nil {
					return err
				}
			}
			if repaired {
				_, err = store.WakeExecution(ctx, tx, id, r.Wake)
			}
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}
