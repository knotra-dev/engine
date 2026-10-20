package queue

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/store"
)

func overdueTimerRuns(t *testing.T, r *Runtime, count int) []string {
	t.Helper()
	ids := make([]string, count)
	for i := range ids {
		ids[i] = admitRuntimeRun(t, r, runtimePlan(""))
	}
	slices.Sort(ids)
	if _, err := r.Store.Pool.Exec(context.Background(), `INSERT INTO knotra_execution_timers(run_id,id,generation,kind,due_at)
		SELECT run_id,'overdue-'||n,1,'run_deadline',clock_timestamp()-interval '1 second'
		FROM unnest($1::text[]) run_id CROSS JOIN generate_series(1,129) n`, ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

func checkTimerProgress(t *testing.T, r *Runtime, id string, wantConsumed int, wantGeneration int64) {
	t.Helper()
	var consumed int
	var generation int64
	if err := r.Store.Pool.QueryRow(context.Background(), `SELECT wake_generation,
		(SELECT count(*) FROM knotra_execution_timers WHERE run_id=$1 AND consumed_at IS NOT NULL)
		FROM knotra_runs WHERE id=$1`, id).Scan(&generation, &consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != wantConsumed || generation != wantGeneration {
		t.Fatalf("run %s: consumed=%d want=%d generation=%d want=%d", id, consumed, wantConsumed, generation, wantGeneration)
	}
}

func TestRuntimeTimerPumpRotatesPastTerminalRunBacklogs(t *testing.T) {
	r := newTestRuntime(t)
	ids := overdueTimerRuns(t, r, 65)
	ctx := context.Background()
	for i, status := range []string{"succeeded", "failed", "cancelled", "skipped"} {
		if _, err := r.Store.Pool.Exec(ctx, `UPDATE knotra_runs SET document=jsonb_set(document::jsonb,'{status}',to_jsonb($2::text)) WHERE id=ANY($1::text[])`, ids[i*16:(i+1)*16], status); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.pumpTimers(ctx); err != nil {
		t.Fatal(err)
	}
	checkTimerProgress(t, r, ids[64], 0, 1)
	if err := r.pumpTimers(ctx); err != nil {
		t.Fatal(err)
	}
	checkTimerProgress(t, r, ids[64], 64, 2)
	for _, id := range ids[:64] {
		wantConsumed := 128
		if id == ids[63] {
			wantConsumed = 64
		}
		checkTimerProgress(t, r, id, wantConsumed, 1)
	}
}

func TestRuntimeTimerPumpRotatesAcrossLiveRunBacklogs(t *testing.T) {
	r := newTestRuntime(t)
	ids := overdueTimerRuns(t, r, 65)
	ctx := context.Background()
	if err := r.pumpTimers(ctx); err != nil {
		t.Fatal(err)
	}
	checkTimerProgress(t, r, ids[0], 64, 2)
	checkTimerProgress(t, r, ids[64], 0, 1)
	if err := r.pumpTimers(ctx); err != nil {
		t.Fatal(err)
	}
	checkTimerProgress(t, r, ids[64], 64, 2)
	for range 3 {
		if err := r.pumpTimers(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		checkTimerProgress(t, r, id, 129, 4)
	}
	var jobs int
	if err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM river_job").Scan(&jobs); err != nil || jobs != len(ids) {
		t.Fatalf("pending scheduler deliveries=%d want=%d error=%v", jobs, len(ids), err)
	}
}

func TestRuntimeTimerPumpSkipsLockedRunAndRetriesItsTimers(t *testing.T) {
	r := newTestRuntime(t)
	ids := overdueTimerRuns(t, r, 2)
	ctx := context.Background()
	locked, err := r.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locked.Rollback(ctx) }()
	if _, err := store.LockExecutionRun(ctx, locked, ids[0]); err != nil {
		t.Fatal(err)
	}
	pollCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := r.pumpTimers(pollCtx); err != nil {
		t.Fatal(err)
	}
	checkTimerProgress(t, r, ids[0], 0, 1)
	checkTimerProgress(t, r, ids[1], 64, 2)
	if err := locked.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.pumpTimers(ctx); err != nil {
		t.Fatal(err)
	}
	checkTimerProgress(t, r, ids[0], 64, 2)
}

func TestRuntimeTimerPumpPreservesTimersOnVersionFailureAndContinues(t *testing.T) {
	r := newTestRuntime(t)
	ids := overdueTimerRuns(t, r, 2)
	ctx := context.Background()
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_runs SET scheduler_version=$2 WHERE id=$1", ids[0], execution.SchedulerVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := r.pumpTimers(ctx); !errors.Is(err, store.ErrExecutionVersion) {
		t.Fatalf("timer pump error=%v want=%v", err, store.ErrExecutionVersion)
	}
	checkTimerProgress(t, r, ids[0], 0, 1)
	checkTimerProgress(t, r, ids[1], 64, 2)
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_runs SET scheduler_version=$2 WHERE id=$1", ids[0], execution.SchedulerVersion); err != nil {
		t.Fatal(err)
	}
	if err := r.pumpTimers(ctx); err != nil {
		t.Fatal(err)
	}
	checkTimerProgress(t, r, ids[0], 64, 2)
}
