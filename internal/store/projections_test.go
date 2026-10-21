package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/execution"
)

func TestExecutionProjectionBatchPreservesOrderedEventsAndWritesRunOnce(t *testing.T) {
	s := testStore(t)
	id, _ := setupRun(t, s)
	ctx := t.Context()
	_, err := s.Pool.Exec(ctx, `CREATE TABLE projection_writes (id integer);
		CREATE FUNCTION count_projection_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN INSERT INTO projection_writes VALUES (1); RETURN NEW; END $$;
		CREATE TRIGGER projection_write AFTER UPDATE OF document ON knotra_runs
		FOR EACH ROW EXECUTE FUNCTION count_projection_write()`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	projections := []execution.Projection{
		{RunID: id, Kind: "run", Status: "running", Time: now},
		{RunID: id, Kind: "node", InstanceID: "answer", NodeID: "answer", Status: "ready", Time: now},
		{RunID: id, Kind: "node", InstanceID: "answer", NodeID: "answer", Status: "succeeded", Time: now.Add(time.Second),
			Outputs: contract.Values{"answer": {JSON: json.RawMessage("42")}}},
		{RunID: id, Kind: "run", Status: "waiting_resolution", Time: now.Add(2 * time.Second)},
		{RunID: id, Kind: "run", Status: "running", Time: now.Add(3 * time.Second)},
		{RunID: id, Kind: "run", Status: "succeeded", Time: now.Add(4 * time.Second),
			Outputs: contract.Values{"answer": {JSON: json.RawMessage("42")}}},
		{RunID: id, Kind: "run", Status: "running", Time: now.Add(time.Second)},
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return s.ProjectExecutionBatch(ctx, tx, projections)
	})
	if err != nil {
		t.Fatal(err)
	}
	var writes, rootSequence, eventSequence int
	err = s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM projection_writes),sequence,execution_event_sequence
		FROM knotra_runs WHERE id=$1`, id).Scan(&writes, &rootSequence, &eventSequence)
	if err != nil {
		t.Fatal(err)
	}
	if writes != 1 || rootSequence != 7 || eventSequence != 7 {
		t.Fatalf("run writes=%d root sequence=%d event sequence=%d", writes, rootSequence, eventSequence)
	}
	run, err := s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "succeeded" || string(run.Outputs["answer"]) != "42" || !run.UpdatedAt.Equal(now.Add(4*time.Second)) || len(run.AvailableActions) != 0 {
		t.Fatalf("final run=%+v", run)
	}
	if len(run.Instances) != 1 || run.Instances[0].ID != "answer" || run.Instances[0].Status != "succeeded" {
		t.Fatalf("instances=%+v", run.Instances)
	}
	events, err := s.Events(ctx, id, "")
	if err != nil || len(events) != 7 {
		t.Fatalf("events=%+v error=%v", events, err)
	}
	for i, event := range events {
		data, ok := event.Data.(map[string]any)
		if !ok || event.Type != projections[i].Kind || data["status"] != projections[i].Status {
			t.Fatalf("event %d=%+v", i, event)
		}
	}
	// Explicit-sequence replays still deduplicate events allocated by a batch.
	duplicate := projections[2]
	duplicate.Sequence = 3
	if err := s.project(ctx, duplicate); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.Events(ctx, id, events[4].ID)
	if err != nil || len(replayed) != 2 {
		t.Fatalf("cursor replay=%+v error=%v", replayed, err)
	}
}

func TestExecutionProjectionBatchRollsBackAndRejectsInvalidIdentity(t *testing.T) {
	s := testStore(t)
	id, _ := setupRun(t, s)
	ctx := t.Context()
	injected := errors.New("rollback batch")
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := s.ProjectExecutionBatch(ctx, tx, []execution.Projection{
			{RunID: id, Kind: "run", Status: "running"},
			{RunID: id, Kind: "node", InstanceID: "answer", NodeID: "answer", Status: "succeeded"},
		}); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	for _, invalid := range []execution.Projection{
		{RunID: "other", Kind: "node", Status: "running"},
		{RunID: id, Kind: "run", Status: "running", Sequence: 9},
	} {
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			return s.ProjectExecutionBatch(ctx, tx, []execution.Projection{
				{RunID: id, Kind: "run", Status: "running"}, invalid,
			})
		})
		if err == nil {
			t.Fatalf("accepted invalid projection %+v", invalid)
		}
	}
	run, err := s.Run(ctx, id)
	if err != nil || run.Status != "pending" || len(run.Instances) != 0 {
		t.Fatalf("rolled back run=%+v error=%v", run, err)
	}
	events, err := s.Events(ctx, id, "")
	if err != nil || len(events) != 0 {
		t.Fatalf("rolled back events=%+v error=%v", events, err)
	}
	var sequence, revision int64
	err = s.Pool.QueryRow(ctx, "SELECT execution_event_sequence,state_revision FROM knotra_runs WHERE id=$1", id).Scan(&sequence, &revision)
	if err != nil || sequence != 0 || revision != 0 {
		t.Fatalf("rolled back event sequence=%d revision=%d error=%v", sequence, revision, err)
	}
}
