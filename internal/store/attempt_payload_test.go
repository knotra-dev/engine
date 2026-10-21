package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/store/db"
)

func TestReadExecutionAttemptsOmitsDuplicateOutcome(t *testing.T) {
	s, ids, workers, _ := attemptFixture(t)
	ctx := context.Background()
	claim, err := claimFixtureAttempt(ctx, s, ids[0], 1, workers[0])
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	result := execution.ExecuteResult{Outputs: contract.Values{
		"result": {JSON: json.RawMessage(`"` + strings.Repeat("x", 256<<10) + `"`)},
	}}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	outcomeJSON, err := json.Marshal(execution.Outcome{FormatVersion: execution.StateFormatVersion,
		Ownership: claim.Ownership, PlanID: claim.PlanID, CompletedAt: time.Now().UTC(), Outputs: result.Outputs})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE knotra_execution_attempts SET state='completed',
		result=$4,outcome=$5,evidence_key=outcome_key WHERE run_id=$1 AND instance_id=$2 AND number=$3`,
		claim.RunID, claim.InstanceID, claim.Number, resultJSON, outcomeJSON); err != nil {
		t.Fatal(err)
	}
	documents, err := db.New(tx).ReadExecutionAttempts(ctx, db.ReadExecutionAttemptsParams{
		RunID: claim.RunID, GraphID: "a", InstanceIds: []string{claim.InstanceID},
	})
	if err != nil || len(documents) != 1 {
		t.Fatalf("attempt documents=%d: %v", len(documents), err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(documents[0], &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"outcome", "evidence_key", "failure"} {
		if _, ok := fields[field]; ok {
			t.Errorf("scheduler payload contains unused %s", field)
		}
	}
	if len(documents[0]) > len(resultJSON)+1024 {
		t.Errorf("scheduler payload duplicates result: document=%d result=%d", len(documents[0]), len(resultJSON))
	}
	attempts, err := ReadExecutionAttempts(ctx, tx, claim.RunID, "a", []string{claim.InstanceID})
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempt records=%d: %v", len(attempts), err)
	}
	got := attempts[0]
	if got.AttemptID != claim.AttemptID ||
		got.State != "completed" ||
		got.DispatchGeneration != 1 ||
		got.DispatchPending ||
		got.Ownership == nil ||
		*got.Ownership != claim.Ownership ||
		got.OutcomeKey != claim.OutcomeKey ||
		!got.LeaseExpiresAt.Equal(claim.LeaseExpiresAt) {
		t.Fatal("scheduler payload lost attempt identity or scheduling metadata")
	}
	if !reflect.DeepEqual(got.Result, &result) {
		t.Fatal("scheduler payload lost complete attempt result")
	}
}
