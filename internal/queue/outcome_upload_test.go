package queue

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knotra-dev/engine/internal/blob"
	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/store"
)

type blockedOutcomeUpload struct {
	mu      sync.Mutex
	objects map[string][]byte
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockedOutcomeUpload) Get(_ context.Context, key string, limit int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, found := s.objects[key]
	if !found {
		return nil, os.ErrNotExist
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, blob.ErrTooLarge
	}
	return bytes.Clone(data), nil
}

func (s *blockedOutcomeUpload) Put(_ context.Context, key string, data []byte, replace bool) error {
	s.once.Do(func() {
		close(s.started)
		<-s.release
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, found := s.objects[key]; found && !replace && !bytes.Equal(previous, data) {
		return blob.ErrConflict
	}
	s.objects[key] = bytes.Clone(data)
	return nil
}

func (s *blockedOutcomeUpload) SavedAt(_ context.Context, key string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.objects[key]; !found {
		return time.Time{}, os.ErrNotExist
	}
	return time.Now(), nil
}

func TestAttemptLeaseCoversBlockedOutcomeUpload(t *testing.T) {
	r := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	objects := &blockedOutcomeUpload{objects: map[string][]byte{}, started: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(objects.release) }) }
	t.Cleanup(unblock)
	files, err := execution.OpenRemoteOutcomeFiles(objects)
	if err != nil {
		t.Fatal(err)
	}
	r.Outcomes = files
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"message":{"role":"assistant","content":"{\"answer\":\"once\"}"},"done":true,"done_reason":"stop"}`)
	}))
	defer provider.Close()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	id := admitRuntimeRun(t, r, deliveryPlan(provider.URL))
	select {
	case <-objects.started:
	case <-ctx.Done():
		t.Fatal("completed execution did not start its outcome upload")
	}
	var shortened time.Time
	if err := r.Store.Pool.QueryRow(ctx, `UPDATE knotra_execution_attempts SET lease_expires_at=clock_timestamp()+interval '8 seconds'
		WHERE run_id=$1 RETURNING lease_expires_at`, id).Scan(&shortened); err != nil {
		t.Fatal(err)
	}
	await(t, 7*time.Second, func() (bool, error) {
		var renewed bool
		err := r.Store.Pool.QueryRow(ctx, "SELECT lease_expires_at>$2 FROM knotra_execution_attempts WHERE run_id=$1", id, shortened).Scan(&renewed)
		return renewed, err
	})
	// Keep the upload blocked beyond the injected lease, while scheduler recovery runs.
	await(t, 5*time.Second, func() (bool, error) {
		var elapsed bool
		err := r.Store.Pool.QueryRow(ctx, "SELECT clock_timestamp()>$1", shortened).Scan(&elapsed)
		return elapsed, err
	})
	var state string
	var live bool
	if err := r.Store.Pool.QueryRow(
		ctx,
		`SELECT a.state,a.lease_expires_at>clock_timestamp() AND w.heartbeat_at>clock_timestamp()-interval '20 seconds'
		FROM knotra_execution_attempts a JOIN knotra_workers w ON w.id=a.owner WHERE a.run_id=$1`,
		id,
	).Scan(&state, &live); err != nil || state != "claimed" || !live {
		t.Fatalf("blocked upload lost its live owner: state=%s live=%t error=%v", state, live, err)
	}
	unblock()
	await(t, 5*time.Second, func() (bool, error) {
		status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
		return status == "succeeded", err
	})
	run, err := r.Store.Run(ctx, id)
	if err != nil || string(run.Outputs["result"]) != `"once"` || calls.Load() != 1 {
		t.Fatalf("slow upload lost or repeated result: run=%+v calls=%d error=%v", run, calls.Load(), err)
	}
	var attempts, published, used int
	if err := r.Store.Pool.QueryRow(
		ctx,
		`SELECT count(*),count(*) FILTER (WHERE state='completed' AND outcome IS NOT NULL),
		(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model') FROM knotra_execution_attempts WHERE run_id=$1`,
		id,
	).Scan(&attempts, &published, &used); err != nil || attempts != 1 || published != 1 || used != 1 {
		t.Fatalf("slow upload repeated publication or spending: attempts=%d published=%d budget=%d error=%v", attempts, published, used, err)
	}
}
