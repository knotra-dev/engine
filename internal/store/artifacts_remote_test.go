package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/knotra-dev/engine/internal/blob"
)

type artifactBlobs map[string][]byte

func (s artifactBlobs) Get(_ context.Context, key string, limit int64) ([]byte, error) {
	data, found := s[key]
	if !found {
		return nil, os.ErrNotExist
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, blob.ErrTooLarge
	}
	return bytes.Clone(data), nil
}

func (s artifactBlobs) Put(_ context.Context, key string, data []byte, replace bool) error {
	if previous, found := s[key]; found && !replace && !bytes.Equal(previous, data) {
		return blob.ErrConflict
	}
	s[key] = bytes.Clone(data)
	return nil
}

func (s artifactBlobs) SavedAt(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}

func TestRemoteArtifactsRequireImmutableVerifiedBytes(t *testing.T) {
	objects := artifactBlobs{}
	files := Artifacts{Blobs: objects}
	data := []byte("durable artifact")
	artifact, err := files.Write("first.txt", "text/plain", data, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := files.Write("second.txt", "text/plain", data, nil)
	if err != nil || artifact.SHA256 != again.SHA256 || artifact.ID == again.ID || len(objects) != 1 {
		t.Fatalf("content deduplication failed: %+v %v", again, err)
	}
	reader := Artifacts{Blobs: objects}
	got, err := reader.read(t.Context(), artifact)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("fresh reader lost bytes: %q %v", got, err)
	}
	storageKey := "artifacts/" + artifact.SHA256
	objects[storageKey] = []byte(strings.Repeat("x", len(data)))
	if _, err := reader.read(t.Context(), artifact); err == nil {
		t.Fatal("accepted corrupt artifact")
	}
	if _, err := files.Write("again.txt", "text/plain", data, nil); !errors.Is(err, blob.ErrConflict) {
		t.Fatalf("replaced corrupt existing object: %v", err)
	}
	if _, err := files.Write("large.txt", "text/plain", make([]byte, MaxArtifactBytes+1), nil); err == nil {
		t.Fatal("accepted oversized artifact")
	}
	delete(objects, storageKey)
	if _, err := reader.read(t.Context(), artifact); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing bytes changed classification: %v", err)
	}
}
