package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/protocol"
)

func managedCall(handler http.Handler, route, token, key string, payload []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1"+route, bytes.NewReader(payload))
	r.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestManagedControlRequiresSeparateToken(t *testing.T) {
	for _, route := range []string{"/managed/prepare-restart", "/managed/cancel-restart"} {
		for _, token := range []string{"", "api-token", "wrong"} {
			s := &Server{Token: "api-token", ManagedToken: "control-token"}
			if w := managedCall(s.Handler(), route, token, "", []byte(`{}`)); w.Code != 401 {
				t.Fatalf("%s accepted %q: %d", route, token, w.Code)
			}
		}
		if w := managedCall((&Server{Token: "api-token"}).Handler(), route, "api-token", "", []byte(`{}`)); w.Code != 404 {
			t.Fatalf("unmanaged service exposes %s: %d", route, w.Code)
		}
	}
	s := &Server{Token: "api-token", ManagedToken: "control-token"}
	s.HoldManagedAdmission()
	if w := managedCall(s.Handler(), "/managed/cancel-restart", "control-token", "", []byte(`{"unexpected":true}`)); w.Code != 400 || !s.restartPrepared {
		t.Fatalf("invalid request released admission: %d", w.Code)
	}
	if w := managedCall(s.Handler(), "/managed/cancel-restart", "control-token", "", []byte(`{}`)); w.Code != 200 || s.restartPrepared {
		t.Fatalf("owner could not release held admission: %d", w.Code)
	}
}

func TestCancelledManagedControlCannotChangeAdmission(t *testing.T) {
	for _, action := range []string{"prepare-restart", "cancel-restart"} {
		t.Run(action, func(t *testing.T) {
			s := &Server{ManagedToken: "control-token", restartPrepared: action == "cancel-restart"}
			held := s.restartPrepared
			ctx, cancel := context.WithCancel(t.Context())
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/managed/"+action, strings.NewReader(`{}`))
			r.Header.Set("Authorization", "Bearer control-token")
			s.admissionMu.RLock()
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(s.admissionMu.RUnlock) }
			t.Cleanup(func() { cancel(); release() })
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				done <- w
			}()
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for s.admissionMu.TryRLock() {
				s.admissionMu.RUnlock()
				select {
				case <-deadline.C:
					t.Fatal("control request did not wait on the admission gate")
				case <-time.After(time.Millisecond):
				}
			}
			cancel()
			release()
			w := <-done
			if w.Code != 503 || s.restartPrepared != held {
				t.Fatalf("abandoned %s changed admission: status=%d held=%v", action, w.Code, s.restartPrepared)
			}
		})
	}
}

func managedFixture(t *testing.T) (*Server, http.Handler, []byte) {
	t.Helper()
	database := apiStore(t)
	profile, diagnostics := contract.ParseProfile([]byte(`apiVersion: knotra/v1
kind: EngineProfile
metadata: {name: local}
spec:
  models:
    source:
      provider: openai
      model: original-model
      baseUrl: https://original.example/v1
      auth: {key: {secretRef: source_key}}
  secrets: {source_key: {env: ORIGINAL_MODEL_KEY}}
  limits: {timeout: 10m, maxConcurrentNodes: 4, maxNodeInstances: 100, maxModelCalls: 10, maxToolCalls: 20}
`))
	if contract.HasErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	s := &Server{Store: database, Profiles: map[string]contract.Profile{"local": profile}, ManagedToken: "control-token",
		Wake: func(context.Context, pgx.Tx, string, int64) error { return nil }}
	handler := s.Handler()
	source := `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: approve}
spec:
  nodes:
    approval:
      type: human
      human: {prompt: {text: Approve}}
      outputs:
        ok: {schema: {type: boolean}}
  outputs:
    ok:
      schema: {type: boolean}
      bind: {from: nodes.approval.outputs.ok}
`
	pkg := contract.Package{Entrypoint: "pipeline.yaml", Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
	request, err := json.Marshal(map[string]any{"package": pkg})
	if err != nil {
		t.Fatal(err)
	}
	w := managedCall(handler, "/definitions", "", "definition", request)
	var definition struct {
		Definition protocol.Definition `json:"definition"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &definition) != nil {
		t.Fatalf("define: %d %s", w.Code, w.Body)
	}
	payload, err := json.Marshal(map[string]any{
		"definitionId": definition.Definition.ID,
		"profile":      "local",
		"inputs":       map[string]any{},
		"artifacts":    map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, handler, payload
}

func managedRun(t *testing.T, w *httptest.ResponseRecorder) protocol.Run {
	t.Helper()
	var result struct {
		Run protocol.Run `json:"run"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	return result.Run
}

func managedReady(t *testing.T, handler http.Handler) (bool, int64) {
	t.Helper()
	w := managedCall(handler, "/managed/prepare-restart", "control-token", "", []byte(`{}`))
	var result struct {
		Ready      bool  `json:"ready"`
		ActiveRuns int64 `json:"activeRuns"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("prepare: %d %s", w.Code, w.Body)
	}
	return result.Ready, result.ActiveRuns
}

func TestManagedRestartWaitsForEveryUnfinishedRunAndPreservesReceipts(t *testing.T) {
	s, handler, payload := managedFixture(t)
	first := managedCall(handler, "/runs", "", "initial", payload)
	run := managedRun(t, first)
	for _, status := range []string{
		"pending",
		"running",
		"waiting_human",
		"waiting_resolution",
		"retry_wait",
		"queued",
		"unknown",
		"future_status",
		"",
	} {
		if _, err := s.Store.Pool.Exec(
			t.Context(),
			"UPDATE knotra_runs SET document=jsonb_set(document::jsonb,'{status}',to_jsonb($2::text)) WHERE id=$1",
			run.ID,
			status,
		); err != nil {
			t.Fatal(err)
		}
		if status == "" {
			if _, err := s.Store.Pool.Exec(t.Context(), "UPDATE knotra_runs SET document=document::jsonb-'status' WHERE id=$1", run.ID); err != nil {
				t.Fatal(err)
			}
		}
		if ready, active := managedReady(t, handler); ready || active != 1 {
			t.Fatalf("status %q: ready=%v active=%d", status, ready, active)
		}
	}
	second := managedRun(t, managedCall(handler, "/runs", "", "busy-stays-open", payload))
	if _, err := s.Store.Pool.Exec(
		t.Context(),
		"UPDATE knotra_runs SET document=jsonb_set(document::jsonb,'{status}',to_jsonb('succeeded'::text)) WHERE id=$1",
		second.ID,
	); err != nil {
		t.Fatal(err)
	}

	// Replacing the startup profile cannot alter the already serialized plan.
	profile := s.Profiles["local"]
	model := profile.Spec.Models["source"]
	model.Model, model.BaseURL = "replacement-model", "https://replacement.example/v1"
	profile.Spec.Models["source"] = model
	profile.Spec.Secrets["source_key"] = contract.SecretSource{Env: "REPLACEMENT_MODEL_KEY"}
	plan, err := s.Store.Plan(t.Context(), run.ID)
	if err != nil ||
		plan.Profile.Spec.Models["source"].Model != "original-model" ||
		plan.Profile.Spec.Models["source"].BaseURL != "https://original.example/v1" ||
		plan.Profile.Spec.Secrets["source_key"].Env != "ORIGINAL_MODEL_KEY" {
		t.Fatalf("admitted bindings changed: %v %v", plan.Profile, err)
	}

	for _, status := range []string{"succeeded", "failed", "cancelled", "skipped"} {
		if _, err := s.Store.Pool.Exec(
			t.Context(),
			"UPDATE knotra_runs SET document=jsonb_set(document::jsonb,'{status}',to_jsonb($2::text)) WHERE id=$1",
			run.ID,
			status,
		); err != nil {
			t.Fatal(err)
		}
		if ready, active := managedReady(t, handler); !ready || active != 0 {
			t.Fatalf("terminal %q: ready=%v active=%d", status, ready, active)
		}
		if w := managedCall(handler, "/runs", "", "retry-after-update", payload); w.Code != 503 {
			t.Fatalf("admitted after idle accepted: %d %s", w.Code, w.Body)
		}
		managedCall(handler, "/managed/cancel-restart", "control-token", "", []byte(`{}`))
	}
	if ready, _ := managedReady(t, handler); !ready {
		t.Fatal("idle engine did not freeze")
	}
	if replay := managedCall(handler, "/runs", "", "initial", payload); replay.Code != 201 || !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatalf("held engine lost receipt: %d %s", replay.Code, replay.Body)
	}
	var receipts int
	if err := s.Store.Pool.QueryRow(t.Context(), "SELECT count(*) FROM knotra_commands WHERE id='retry-after-update'").Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("temporary admission rejection persisted a receipt: %d %v", receipts, err)
	}
	managedCall(handler, "/managed/cancel-restart", "control-token", "", []byte(`{}`))
	managedRun(t, managedCall(handler, "/runs", "", "retry-after-update", payload))
}

func TestManagedRestartWaitsThroughRunCommit(t *testing.T) {
	s, handler, payload := managedFixture(t)
	for _, statement := range []string{
		`CREATE FUNCTION managed_commit_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(713674129); RETURN NEW; END $$`,
		`CREATE CONSTRAINT TRIGGER managed_commit_barrier AFTER INSERT ON knotra_runs DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION managed_commit_barrier()`,
	} {
		if _, err := s.Store.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	barrier, err := s.Store.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := barrier.Exec(t.Context(), "SELECT pg_advisory_lock(713674129)"); err != nil {
		barrier.Release()
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	releaseAdmission := func() {
		releaseOnce.Do(func() {
			if _, err := barrier.Exec(context.Background(), "SELECT pg_advisory_unlock(713674129)"); err != nil {
				t.Error(err)
			}
			barrier.Release()
		})
	}
	t.Cleanup(releaseAdmission)
	inserted := make(chan struct{})
	s.Wake = func(context.Context, pgx.Tx, string, int64) error {
		close(inserted)
		return nil
	}
	started := make(chan *httptest.ResponseRecorder, 1)
	go func() { started <- managedCall(handler, "/runs", "", "in-flight", payload) }()
	select {
	case <-inserted:
	case <-time.After(5 * time.Second):
		t.Fatal("admission did not reach its uncommitted wake")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		var commitBlocked bool
		if err := s.Store.Pool.QueryRow(
			t.Context(),
			"SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=713674129 AND NOT granted)",
		).Scan(&commitBlocked); err != nil {
			t.Fatal(err)
		}
		if commitBlocked {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("admission did not reach its blocked commit")
		case <-time.After(time.Millisecond):
		}
	}
	prepared := make(chan *httptest.ResponseRecorder, 1)
	prepareStarted := make(chan struct{})
	go func() {
		close(prepareStarted)
		prepared <- managedCall(handler, "/managed/prepare-restart", "control-token", "", []byte(`{}`))
	}()
	<-prepareStarted
	select {
	case w := <-prepared:
		releaseAdmission()
		<-started
		t.Fatalf("idle decision raced uncommitted run: %d %s", w.Code, w.Body)
	case <-time.After(50 * time.Millisecond):
	}
	releaseAdmission()
	managedRun(t, <-started)
	w := <-prepared
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ready":false`) || !strings.Contains(w.Body.String(), `"activeRuns":1`) {
		t.Fatalf("committed run was missed: %d %s", w.Code, w.Body)
	}
}

func TestManagedReplacementStartsHeldAndReleasesWithoutPoisoningCommand(t *testing.T) {
	s, handler, payload := managedFixture(t)
	s.HoldManagedAdmission()
	if w := managedCall(handler, "/runs", "", "after-source-commit", payload); w.Code != 503 {
		t.Fatalf("held replacement admitted a run: %d %s", w.Code, w.Body)
	}
	managedCall(handler, "/managed/cancel-restart", "control-token", "", []byte(`{}`))
	managedRun(t, managedCall(handler, "/runs", "", "after-source-commit", payload))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/managed/prepare-restart", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer control-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 503 || s.restartPrepared {
		t.Fatalf("failed idle check changed admission: %d %s", w.Code, w.Body)
	}
}
