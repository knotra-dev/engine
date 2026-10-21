package store

import (
	"context"
	"errors"
	"testing"

	"github.com/knotra-dev/engine/internal/execution"
)

func TestSessionAffinityFencesOtherWorkersAndSurvivesDrain(t *testing.T) {
	for _, state := range []string{"live", "draining", "expired"} {
		t.Run(state, func(t *testing.T) {
			s, ids, workers, _ := attemptFixture(t)
			ctx := context.Background()
			if _, err := s.Pool.Exec(ctx, "UPDATE knotra_runs SET session_affinity=true WHERE id=$1", ids[0].RunID); err != nil {
				t.Fatal(err)
			}
			tx, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if claim, err := s.ClaimAttempt(ctx, tx, ids[0], 1, workers[1]); err != nil || claim == nil {
				t.Fatalf("rolled-back claim=%+v error=%v", claim, err)
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			first, err := claimFixtureAttempt(ctx, s, ids[0], 1, workers[0])
			if err != nil || first == nil || first.SessionWorkerLost {
				t.Fatalf("first claim=%+v error=%v", first, err)
			}
			if state == "draining" {
				if err := s.DrainWorker(ctx, workers[0]); err != nil {
					t.Fatal(err)
				}
			}
			if state == "expired" {
				if _, err := s.Pool.Exec(
					ctx,
					"UPDATE knotra_workers SET heartbeat_at=clock_timestamp()-interval '21 seconds' WHERE id=$1",
					workers[0],
				); err != nil {
					t.Fatal(err)
				}
			}
			claim, err := claimFixtureAttempt(ctx, s, ids[3], 1, workers[1])
			if state == "expired" {
				if err != nil || claim == nil || !claim.SessionWorkerLost {
					t.Fatalf("lost-owner claim=%+v error=%v", claim, err)
				}
				if err := s.HeartbeatWorker(ctx, workers[0]); !errors.Is(err, ErrExecutionOwnership) {
					t.Fatalf("lost owner resurrected: %v", err)
				}
			} else {
				if !errors.Is(err, ErrExecutionAffinity) || claim != nil {
					t.Fatalf("foreign claim=%+v error=%v", claim, err)
				}
				claim, err = claimFixtureAttempt(ctx, s, ids[3], 1, workers[0])
				if err != nil || claim == nil || claim.SessionWorkerLost {
					t.Fatalf("original-owner claim=%+v error=%v", claim, err)
				}
			}
			var owner string
			if err := s.Pool.QueryRow(ctx, "SELECT session_worker_id FROM knotra_runs WHERE id=$1", ids[0].RunID).Scan(&owner); err != nil || owner != workers[0] {
				t.Fatalf("session affinity changed: owner=%s error=%v", owner, err)
			}
			if claim.Validate() != nil ||
				claim.Request.Ownership == nil ||
				*claim.Request.Ownership != (execution.Ownership{AttemptID: ids[3], WorkerID: claim.WorkerID, Generation: claim.Generation}) {
				t.Fatal("claim lost the publication ownership fence")
			}
		})
	}
}
