package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store/db"
)

func TestLargeInstanceCollectionPagesWithoutLosingData(t *testing.T) {
	// Both contexts fit the legal 32 KiB preview limit; together 5000 instances
	// previously exceeded both the desktop and CLI response limits.
	value, err := contract.JSONValue(json.RawMessage(`"` + strings.Repeat("x", maxContextBytes-256) + `"`))
	if err != nil {
		t.Fatal(err)
	}
	preview, truncated := observationContext(contract.Values{"value": value})
	if truncated {
		t.Fatal("fixture exceeds legal context preview")
	}
	const count = 5000
	seen, totalBytes := 0, 0
	for seen < count {
		documents := [][]byte{}
		documentBytes := 0
		for i := seen; i < count && i < seen+101; i++ {
			b, err := json.Marshal(protocol.Instance{ID: fmt.Sprintf("%05d", i), NodeID: "work", Scope: "pipeline.yaml", Status: "succeeded", Inputs: preview, Outputs: preview})
			if err != nil {
				t.Fatal(err)
			}
			documents = append(documents, b)
			documentBytes += len(b)
			if documentBytes > MaxInstancePageBytes {
				break
			}
		}
		page, err := instancePage(documents)
		if err != nil || len(page.Items) == 0 {
			t.Fatalf("page after %d: %v", seen, err)
		}
		for _, instance := range page.Items {
			if instance.ID != fmt.Sprintf("%05d", seen) || instance.DataTruncated || !reflect.DeepEqual(instance.Inputs, preview) || !reflect.DeepEqual(instance.Outputs, preview) {
				t.Fatalf("lost instance or context at %d: %s", seen, instance.ID)
			}
			b, err := json.Marshal(instance)
			if err != nil || len(b) < 64000 {
				t.Fatalf("instance context lost: %d bytes, %v", len(b), err)
			}
			totalBytes += len(b)
			seen++
		}
		b, err := json.Marshal(page)
		if err != nil || len(b) > MaxInstancePageBytes+1024 || len(b) >= 96<<20 {
			t.Fatalf("oversized page: %d bytes, %v", len(b), err)
		}
		if seen < count {
			if page.NextCursor == nil || *page.NextCursor != fmt.Sprintf("%05d", seen-1) {
				t.Fatalf("missing continuation after %d", seen)
			}
		} else if page.NextCursor != nil {
			t.Fatal("unexpected continuation on last page")
		}
	}
	if totalBytes <= 256<<20 {
		t.Fatalf("fixture does not reproduce the original response limit: %d", totalBytes)
	}
}

func TestRunInstancePages(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _ := setupRun(t, s)
	preview := map[string]any{"values": map[string]any{"value": strings.Repeat("x", 32000)}, "artifacts": map[string]any{}}
	if err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for i := range 205 {
			instanceID := fmt.Sprintf("%05d", i)
			b, err := json.Marshal(protocol.Instance{ID: instanceID, NodeID: "work", Scope: "pipeline.yaml", Status: "succeeded", Inputs: preview, Outputs: preview})
			if err != nil {
				return err
			}
			if _, err := db.New(tx).UpsertInstanceProjection(ctx, db.UpsertInstanceProjectionParams{RunID: id, ID: instanceID, Sequence: 1, Document: b}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	run, err := s.Run(ctx, id)
	if err != nil || len(run.Instances) == 0 || len(run.Instances) >= 100 || run.InstancesNextCursor == nil {
		t.Fatalf("run did not expose partial instances: %d, %v", len(run.Instances), err)
	}
	seen := len(run.Instances)
	cursor := run.InstancesNextCursor
	for cursor != nil {
		page, err := s.Instances(ctx, id, *cursor)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(page)
		if err != nil || len(encoded) > MaxInstancePageBytes+1024 {
			t.Fatalf("SQL instance page exceeds budget: %d, %v", len(encoded), err)
		}
		for _, instance := range page.Items {
			n, err := strconv.Atoi(instance.ID)
			if err != nil || n != seen {
				t.Fatalf("lost instance %d: %q", seen, instance.ID)
			}
			seen++
		}
		cursor = page.NextCursor
	}
	if seen != 205 {
		t.Fatalf("only %d instances accessible", seen)
	}
}

func TestInstancePageCountAndOversizedRecord(t *testing.T) {
	documents := [][]byte{}
	for i := range 101 {
		b, err := json.Marshal(protocol.Instance{ID: fmt.Sprintf("%05d", i), NodeID: "work", Scope: "pipeline.yaml", Status: "succeeded"})
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, b)
	}
	complete, err := instancePage(documents[:100])
	if err != nil || len(complete.Items) != 100 || complete.NextCursor != nil {
		t.Fatalf("small run was changed: %d, %v", len(complete.Items), err)
	}
	partial, err := instancePage(documents)
	if err != nil || len(partial.Items) != 100 || partial.NextCursor == nil || *partial.NextCursor != "00099" {
		t.Fatalf("count bound lost its cursor: %d, %v", len(partial.Items), err)
	}
	large, err := json.Marshal(protocol.Instance{ID: "large", NodeID: "work", Scope: "pipeline.yaml", Status: "failed", Reason: strings.Repeat("x", MaxInstancePageBytes+1)})
	if err != nil {
		t.Fatal(err)
	}
	page, err := instancePage([][]byte{large, documents[0]})
	if err != nil || len(page.Items) != 1 || page.NextCursor == nil || *page.NextCursor != "large" || len(page.Items[0].Reason) != MaxInstancePageBytes+1 {
		t.Fatalf("oversized record lost data or cursor: %v", err)
	}
}

func TestInstancePagesKeepCursorWhenSQLJSONHasMoreWhitespace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _ := setupRun(t, s)
	value, err := contract.JSONValue(make([]int, 15000))
	if err != nil {
		t.Fatal(err)
	}
	preview, truncated := observationContext(contract.Values{"dense": value})
	if truncated {
		t.Fatal("fixture exceeds legal context preview")
	}
	if err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for i := range 40 {
			instanceID := fmt.Sprintf("%05d", i)
			b, err := json.Marshal(protocol.Instance{ID: instanceID, NodeID: "work", Scope: "pipeline.yaml", Status: "succeeded", Inputs: preview, Outputs: preview})
			if err != nil {
				return err
			}
			if _, err := db.New(tx).UpsertInstanceProjection(ctx, db.UpsertInstanceProjectionParams{RunID: id, ID: instanceID, Sequence: 1, Document: b}); err != nil {
				return err
			}
		}
		// JSONB normalization inserts spaces while retaining the same legal projection.
		_, err := tx.Exec(ctx, "UPDATE knotra_instances SET document=document::jsonb::json WHERE run_id=$1", id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	seen, cursor := 0, ""
	for {
		page, err := s.Instances(ctx, id, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, instance := range page.Items {
			if instance.ID != fmt.Sprintf("%05d", seen) {
				t.Fatalf("lost instance %d: %q", seen, instance.ID)
			}
			seen++
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if seen != 40 {
		t.Fatalf("raw JSON spacing hid the continuation after %d instances", seen)
	}
}
