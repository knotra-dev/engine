package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/knotra-dev/engine/internal/blob"
)

type outcomeBlobs map[string][]byte

func (s outcomeBlobs) Get(ctx context.Context, key string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, found := s[key]
	if !found {
		return nil, os.ErrNotExist
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, blob.ErrTooLarge
	}
	return bytes.Clone(data), nil
}

func (s outcomeBlobs) Put(_ context.Context, key string, data []byte, replace bool) error {
	if previous, found := s[key]; found && !replace && !bytes.Equal(previous, data) {
		return blob.ErrConflict
	}
	s[key] = bytes.Clone(data)
	return nil
}

func (s outcomeBlobs) SavedAt(ctx context.Context, key string) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if _, found := s[key]; !found {
		return time.Time{}, os.ErrNotExist
	}
	return time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), nil
}

func TestRemoteOutcomeRecoveryVerifiesImmutableEvidence(t *testing.T) {
	objects := outcomeBlobs{}
	files, err := OpenRemoteOutcomeFiles(objects)
	if err != nil {
		t.Fatal(err)
	}
	outcome := fixtureOutcome()
	key, err := files.Put(outcome)
	if err != nil {
		t.Fatal(err)
	}
	if same, err := files.Put(outcome); err != nil || same != key {
		t.Fatalf("identical delivery failed: %s %v", same, err)
	}
	changed := outcome
	changed.CompletedAt = changed.CompletedAt.Add(time.Second)
	if _, err := files.Put(changed); !errors.Is(err, ErrOutcomeConflict) {
		t.Fatalf("replaced original evidence: %v", err)
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenRemoteOutcomeFiles(objects)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Get(key)
	if err != nil || !reflect.DeepEqual(got, outcome) {
		t.Fatalf("fresh reader lost evidence: %+v %v", got, err)
	}
	if saved, err := reader.SavedAt(key); err != nil || saved.IsZero() {
		t.Fatalf("missing saved time: %s %v", saved, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.GetContext(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("read ignored its caller's cancellation: %v", err)
	}
	if _, err := reader.SavedAtContext(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("metadata read ignored its caller's cancellation: %v", err)
	}
	if _, err := reader.Get(strings.Repeat("0", 64)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing outcome changed classification: %v", err)
	}
	var envelope outcomeFile
	storageKey := "outcomes/" + key + ".json"
	if err := json.Unmarshal(objects[storageKey], &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.SHA256 = strings.Repeat("0", 64)
	objects[storageKey], _ = json.Marshal(envelope)
	if _, err := reader.Get(key); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("accepted corrupt remote evidence: %v", err)
	}
	if _, err := OpenRemoteOutcomeFiles(nil); err == nil {
		t.Fatal("accepted missing remote storage")
	}
}

func TestRemoteMCPRecoveryPreservesSessionAndCurrentCall(t *testing.T) {
	objects := outcomeBlobs{}
	files, err := OpenRemoteOutcomeFiles(objects)
	if err != nil {
		t.Fatal(err)
	}
	resource := ResourceRecord{
		ID:                 uuid.NewString(),
		EngineID:           "engine",
		HostID:             "host",
		Kind:               "mcp_http",
		Lifetime:           "run",
		State:              "active",
		DispatchGeneration: 1,
		Ownership:          fixtureOutcome().Ownership,
		MCP:                &MCPSessionRecord{Connection: "tools", SessionID: "session", ProtocolVersion: "2025-11-25"},
	}
	if err := files.PutMCPSession(resource); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "2"} {
		if err := files.PutMCPCall(resource, []byte(id), false); err != nil {
			t.Fatal(err)
		}
	}
	expected := resource
	expected.MCP, expected.State, expected.DispatchGeneration = nil, "cleaning", 2
	if got, err := files.GetMCPSession(expected); err != nil || got == nil || *got != *resource.MCP {
		t.Fatalf("lost session identity: %+v %v", got, err)
	}
	if id, err := files.GetMCPCall(expected); err != nil || string(id) != "2" {
		t.Fatalf("lost latest unconfirmed call: %s %v", id, err)
	}
	if err := files.PutMCPCall(resource, []byte("2"), true); err != nil {
		t.Fatal(err)
	}
	if id, err := files.GetMCPCall(expected); err != nil || len(id) != 0 {
		t.Fatalf("confirmed call remained active: %s %v", id, err)
	}
	if len(objects) != 2 {
		t.Fatalf("current-call updates grew storage: %d", len(objects))
	}
	foreign := expected
	foreign.HostID = "other-host"
	if _, err := files.GetMCPSession(foreign); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("accepted another host's evidence: %v", err)
	}
	key, _ := mcpEvidenceKey(resource)
	objects["outcomes/"+key+".json"] = bytes.Repeat([]byte("x"), maxMCPEvidenceBytes+1)
	if _, err := files.GetMCPSession(expected); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("accepted oversized evidence: %v", err)
	}
}
