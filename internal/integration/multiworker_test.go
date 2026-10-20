//go:build unix

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/knotra-dev/engine/internal/app"
	"github.com/knotra-dev/engine/internal/blob"
	"github.com/knotra-dev/engine/internal/client"
	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store"
)

const multiworkerPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: multiworker}
spec:
  models:
    model: {connection: local, requires: [structuredOutput]}
  nodes:
    a:
      type: llm
      llm: {model: model, prompt: {text: Return answer.}}
      outputs:
        answer: {schema: {type: string}}
    b:
      type: llm
      llm: {model: model, prompt: {text: Return answer.}}
      outputs:
        answer: {schema: {type: string}}
    c:
      type: llm
      inputs:
        previous: {schema: {type: string}, bind: {from: nodes.a.outputs.answer}}
      llm: {model: model, prompt: {text: Return answer.}}
      outputs:
        answer: {schema: {type: string}}
  outputs:
    result: {schema: {type: string}, bind: {from: nodes.c.outputs.answer}}
`

// The model is a fixture; S3 uses a wire fixture unless an explicit test endpoint
// is configured. Every role runs public app.Serve in an independent process.
func TestMultiworkerServeWithS3AndIndependentDataDirectories(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_DATABASE_URL for public multiworker service acceptance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	storage := multiworkerStorage(t, "acceptance")
	var calls atomic.Int32
	var phase atomic.Int32
	releases := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var released [2]sync.Once
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/chat" {
			_, _ = io.WriteString(w, `{"models":[{"name":"fixture","digest":"fixture-digest"}],"capabilities":["completion"]}`)
			return
		}
		calls.Add(1)
		select {
		case <-releases[phase.Load()]:
		case <-req.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"role": "assistant", "content": `{"answer":"shared remote result"}`},
			"done":    true, "done_reason": "stop",
		})
	}))
	t.Cleanup(provider.Close)
	t.Cleanup(func() {
		for i := range releases {
			released[i].Do(func() { close(releases[i]) })
		}
	})
	work := t.TempDir()
	profile := recoveryProfile()
	profile.Metadata.Name = "multiworker"
	profile.Spec.Sandboxes = nil
	profile.Spec.Models = map[string]contract.ModelConnection{"local": {Provider: "ollama", Model: "fixture", BaseURL: provider.URL}}
	profile.Spec.Limits.MaxConcurrentNodes = 1
	profile.Spec.Limits.MaxModelCalls = 3
	profilePath := filepath.Join(work, "profile.json")
	writeJSON(t, profilePath, profile)
	base := app.Options{DatabaseURL: databaseSchema(t, ctx), Profiles: []string{profilePath}, ExecutionWorkers: 1,
		S3: storage}
	var headless []string
	start := func(name, host string, role execution.Role) (*engineProcess, *client.Client) {
		t.Helper()
		options := base
		options.HostID, options.Role, options.DataDir = host, role, filepath.Join(work, name)
		options.Listen = unusedAddress(t)
		path, log := filepath.Join(work, name+".json"), filepath.Join(work, name+".log")
		writeJSON(t, path, options)
		t.Cleanup(func() {
			if t.Failed() {
				data, _ := os.ReadFile(log)
				t.Logf("%s: %s", name, data)
			}
		})
		var api *client.Client
		if role == execution.RoleAPI {
			api = &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, name+"-client")}
		}
		process := startEngineProcess(t, ctx, path, log, api)
		if api == nil {
			headless = append(headless, options.Listen)
		}
		return process, api
	}
	apiProcess, api := start("api", "control", execution.RoleAPI)
	identity := engineIdentity(t, ctx, api)
	outcomes := multiworkerOutcomes(t, ctx, storage, identity)
	admin, err := store.Open(ctx, base.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	start("scheduler", "control", execution.RoleScheduler)
	workers := make([]*engineProcess, 3)
	for i := range workers {
		name := fmt.Sprintf("worker-%d", i+1)
		workers[i], _ = start(name, name, execution.RoleExecutor)
	}
	awaitMultiworker(t, ctx, func() (bool, error) {
		var ready int
		err := admin.Pool.QueryRow(ctx, `SELECT count(*) FROM knotra_workers
			WHERE capabilities->>'role'='executor' AND delivery_client_id IS NOT NULL AND NOT draining`).Scan(&ready)
		return ready == 3, err
	})
	for _, address := range headless {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/v1/info", nil)
		if err != nil {
			t.Fatal(err)
		}
		if response, err := http.DefaultClient.Do(request); err == nil {
			_ = response.Body.Close()
			t.Fatal("headless role opened an HTTP listener")
		}
	}
	var uploaded struct{ Artifact contract.Artifact }
	content := []byte("artifact survives API replacement\x00\xff")
	if err := api.Command(ctx, "/artifacts", map[string]any{"name": "evidence.bin", "mediaType": "application/octet-stream", "content": base64.StdEncoding.EncodeToString(content)}, uuid.NewString(), &uploaded); err != nil {
		t.Fatal(err)
	}
	var definition struct{ Definition protocol.Definition }
	pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: multiworkerPipeline, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(multiworkerPipeline)}}}
	if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
		t.Fatal(err)
	}
	for round, count := range []int{6, 4} {
		phase.Store(int32(round))
		before := calls.Load()
		runs := make([]string, count)
		for i := range runs {
			var accepted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &accepted); err != nil {
				t.Fatal(err)
			}
			runs[i] = accepted.Run.ID
		}
		awaitMultiworker(t, ctx, func() (bool, error) {
			var owners int
			err := admin.Pool.QueryRow(ctx, `SELECT count(DISTINCT owner) FROM knotra_execution_attempts
				WHERE state='claimed' AND run_id=ANY($1)`, runs).Scan(&owners)
			return owners == 3-round && calls.Load() == before+int32(3-round), err
		})
		released[round].Do(func() { close(releases[round]) })
		for _, id := range runs {
			final := awaitTerminal(t, ctx, api, id)
			if final.Status != "succeeded" || len(final.Instances) != 3 || string(final.Outputs["result"]) != `"shared remote result"` {
				t.Fatalf("multiworker run=%+v", final)
			}
		}
		if round == 0 {
			if err := workers[0].command.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			select {
			case <-workers[0].finished:
				if !workers[0].command.ProcessState.Success() {
					t.Fatal("executor did not drain successfully")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			apiProcess.kill(t)
			_, api = start("replacement-api", "control", execution.RoleAPI)
			if engineIdentity(t, ctx, api) != identity {
				t.Fatal("API replacement changed engine identity")
			}
			data, err := api.Bytes(ctx, "/artifacts/"+uploaded.Artifact.ID+"/content")
			if err != nil || !bytes.Equal(data, content) {
				t.Fatalf("fresh API could not read shared artifact: %v", err)
			}
		}
	}
	var attempts, invalid, debits, slots, wrongRole int
	if err := admin.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE a.state<>'completed' OR a.number<>1),
		(SELECT sum(used) FROM knotra_budgets WHERE kind='model' AND scope=run_id),
		(SELECT count(*) FROM knotra_execution_slots WHERE released_at IS NULL),
		count(*) FILTER (WHERE w.capabilities->>'role'<>'executor')
		FROM knotra_execution_attempts a JOIN knotra_workers w ON w.id=a.owner`).Scan(&attempts, &invalid, &debits, &slots, &wrongRole); err != nil {
		t.Fatal(err)
	}
	if attempts != 30 || calls.Load() != 30 || debits != 30 || invalid != 0 || slots != 0 || wrongRole != 0 {
		t.Fatalf("attempts/calls/debits=%d/%d/%d invalid/slots/wrongRole=%d/%d/%d", attempts, calls.Load(), debits, invalid, slots, wrongRole)
	}
	rows, err := admin.Pool.Query(ctx, "SELECT outcome_key FROM knotra_execution_attempts")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var saved int
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		outcome, err := outcomes.GetContext(ctx, key)
		if err != nil || outcome.Failure != nil || string(outcome.Outputs["answer"].JSON) != `"shared remote result"` {
			t.Fatalf("remote outcome=%+v error=%v", outcome, err)
		}
		saved++
	}
	if err := rows.Err(); err != nil || saved != 30 {
		t.Fatalf("remote outcomes=%d error=%v", saved, err)
	}
	t.Log("public API, scheduler and three executor processes: 30 calls, matching debits and remote outcomes; fresh API read artifact after one executor drained")
}

func TestMultiworkerRecoversS3OutcomeAfterExecutorCrash(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_DATABASE_URL for remote outcome crash recovery")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	storage := multiworkerStorage(t, "recovery")
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/chat" {
			_, _ = io.WriteString(w, `{"models":[{"name":"fixture","digest":"fixture-digest"}],"capabilities":["completion"]}`)
			return
		}
		calls.Add(1)
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"{\"answer\":\"shared remote result\"}"},"done":true,"done_reason":"stop"}`)
	}))
	t.Cleanup(provider.Close)
	work := t.TempDir()
	profile := recoveryProfile()
	profile.Spec.Sandboxes = nil
	profile.Spec.Models = map[string]contract.ModelConnection{"local": {Provider: "ollama", Model: "fixture", BaseURL: provider.URL}}
	profile.Spec.Limits.MaxConcurrentNodes = 1
	profile.Spec.Limits.MaxModelCalls = 3
	profilePath := filepath.Join(work, "profile.json")
	writeJSON(t, profilePath, profile)
	base := app.Options{DatabaseURL: databaseSchema(t, ctx), Profiles: []string{profilePath}, ExecutionWorkers: 1,
		S3: storage}
	apiOptions := base
	apiOptions.HostID, apiOptions.Role, apiOptions.DataDir = "control", execution.RoleAPI, filepath.Join(work, "api")
	apiOptions.Listen = unusedAddress(t)
	apiPath := filepath.Join(work, "api.json")
	writeJSON(t, apiPath, apiOptions)
	api := &client.Client{BaseURL: "http://" + apiOptions.Listen, StateDir: filepath.Join(work, "client")}
	startEngineProcess(t, ctx, apiPath, filepath.Join(work, "api.log"), api)
	outcomes := multiworkerOutcomes(t, ctx, storage, engineIdentity(t, ctx, api))
	start := func(name string, role execution.Role) *engineProcess {
		t.Helper()
		options := base
		options.HostID, options.Role, options.DataDir = name, role, filepath.Join(work, name)
		path, log := filepath.Join(work, name+".json"), filepath.Join(work, name+".log")
		writeJSON(t, path, options)
		t.Cleanup(func() {
			if t.Failed() {
				data, _ := os.ReadFile(log)
				t.Logf("%s: %s", name, data)
			}
		})
		return startEngineProcess(t, ctx, path, log, nil)
	}
	start("scheduler", execution.RoleScheduler)
	first := start("crashed-worker", execution.RoleExecutor)
	admin, err := store.Open(ctx, base.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	barrier, err := pgx.Connect(ctx, base.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = barrier.Close(context.Background()) }()
	if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273658)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Pool.Exec(ctx, `CREATE FUNCTION remote_outcome_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock(918273658); RETURN NEW; END $$;
		CREATE TRIGGER remote_outcome_barrier BEFORE UPDATE ON knotra_execution_attempts FOR EACH ROW
		WHEN (NEW.outcome IS NOT NULL AND OLD.outcome IS NULL) EXECUTE FUNCTION remote_outcome_barrier()`); err != nil {
		t.Fatal(err)
	}
	var definition struct{ Definition protocol.Definition }
	pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: multiworkerPipeline, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(multiworkerPipeline)}}}
	if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
		t.Fatal(err)
	}
	var accepted struct{ Run protocol.Run }
	if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &accepted); err != nil {
		t.Fatal(err)
	}
	awaitMultiworker(t, ctx, func() (bool, error) {
		var blocked bool
		err := admin.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE wait_event='advisory' AND $1::integer=ANY(pg_blocking_pids(pid)))`, int64(barrier.PgConn().PID())).Scan(&blocked)
		return blocked, err
	})
	var owner string
	var key string
	var unpublished bool
	if err := admin.Pool.QueryRow(ctx, `SELECT owner,outcome_key,outcome IS NULL FROM knotra_execution_attempts
		WHERE run_id=$1 AND state='claimed'`, accepted.Run.ID).Scan(&owner, &key, &unpublished); err != nil || !unpublished || calls.Load() != 1 {
		t.Fatalf("crash boundary unpublished=%v calls=%d error=%v", unpublished, calls.Load(), err)
	}
	if outcome, err := outcomes.GetContext(ctx, key); err != nil || outcome.Failure != nil || string(outcome.Outputs["answer"].JSON) != `"shared remote result"` {
		t.Fatalf("outcome was not saved remotely before publication: %+v %v", outcome, err)
	}
	first.kill(t)
	if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273658)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Pool.Exec(ctx, "DROP TRIGGER remote_outcome_barrier ON knotra_execution_attempts"); err != nil {
		t.Fatal(err)
	}
	start("replacement-worker", execution.RoleExecutor)
	final := awaitTerminal(t, ctx, api, accepted.Run.ID)
	if final.Status != "succeeded" || string(final.Outputs["result"]) != `"shared remote result"` || calls.Load() != 3 {
		t.Fatalf("remote recovery run=%+v calls=%d", final, calls.Load())
	}
	var attempts, invalid, recovered, debits int
	if err := admin.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE state<>'completed' OR number<>1),
		count(*) FILTER (WHERE owner=$2),
		(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model')
		FROM knotra_execution_attempts WHERE run_id=$1`, accepted.Run.ID, owner).Scan(&attempts, &invalid, &recovered, &debits); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || invalid != 0 || recovered != 1 || debits != 3 {
		t.Fatalf("remote recovery attempts=%d invalid=%d recovered=%d debits=%d", attempts, invalid, recovered, debits)
	}
	t.Log("executor killed after conditional S3 save and before PostgreSQL publication; scheduler recovered original result, replacement completed remaining work without another call")
}

func TestMultiworkerS3StorageContract(t *testing.T) {
	settings := multiworkerStorage(t, "storage-contract/"+uuid.NewString())
	objects, err := blob.NewS3(t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	original := []byte("immutable original bytes")
	var writers sync.WaitGroup
	for range 12 {
		writers.Go(func() {
			if err := objects.Put(t.Context(), "immutable", original, false); err != nil {
				t.Errorf("concurrent immutable publication: %v", err)
			}
		})
	}
	writers.Wait()
	if err := objects.Put(t.Context(), "immutable", []byte("conflicting bytes"), false); !errors.Is(err, blob.ErrConflict) {
		t.Fatalf("conflicting publication did not fail: %v", err)
	}
	data, err := objects.Get(t.Context(), "immutable", int64(len(original)))
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("concurrent/conflicting writes replaced immutable bytes: %q %v", data, err)
	}
	if saved, err := objects.SavedAt(t.Context(), "immutable"); err != nil || saved.IsZero() {
		t.Fatalf("saved object metadata: %v %v", saved, err)
	}
	for _, value := range []string{"started", "completed"} {
		if err := objects.Put(t.Context(), "current-call", []byte(value), true); err != nil {
			t.Fatal(err)
		}
	}
	if data, err := objects.Get(t.Context(), "current-call", 100); err != nil || string(data) != "completed" {
		t.Fatalf("mutable replacement: %q %v", data, err)
	}
	awsConfig, err := config.LoadDefaultConfig(t.Context(), config.WithRegion(settings.Region))
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint, options.UsePathStyle = aws.String(settings.Endpoint), settings.PathStyle
		options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		options.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	key := path.Join(settings.Prefix, "current-call")
	metadata, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(settings.Bucket), Key: aws.String(key)})
	if err != nil || metadata.ETag == nil {
		t.Fatalf("provider omitted conditional replacement identity: %v", err)
	}
	for i, value := range []string{"next-call", "stale-completion"} {
		_, err := client.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String(settings.Bucket), Key: aws.String(key),
			Body: bytes.NewReader([]byte(value)), IfMatch: metadata.ETag})
		if i == 0 && err != nil {
			t.Fatalf("conditional replacement failed: %v", err)
		}
		if i == 1 {
			var response interface{ HTTPStatusCode() int }
			if !errors.As(err, &response) || response.HTTPStatusCode() != http.StatusPreconditionFailed {
				t.Fatalf("provider accepted stale conditional replacement: %v", err)
			}
		}
	}
	if data, err := objects.Get(t.Context(), "current-call", 100); err != nil || string(data) != "next-call" {
		t.Fatalf("stale completion overwrote the current call: %q %v", data, err)
	}
	if _, err := objects.Get(t.Context(), "missing", 100); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing object was not classified: %v", err)
	}
	if _, err := objects.SavedAt(t.Context(), "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing object metadata was not classified: %v", err)
	}
	t.Log("conditional creation, concurrent identical writers, conflicting bytes, ETag replacement/stale rejection, metadata and missing-key reads passed")
}

func awaitMultiworker(t *testing.T, ctx context.Context, ready func() (bool, error)) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(20 * time.Second)
	defer timeout.Stop()
	for {
		ok, err := ready()
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timeout.C:
			t.Fatal("multiworker boundary not reached")
		case <-ticker.C:
		}
	}
}

func multiworkerStorage(t *testing.T, prefix string) blob.S3Config {
	t.Helper()
	endpoint, bucket := os.Getenv("KNOTRA_TEST_S3_ENDPOINT"), os.Getenv("KNOTRA_TEST_S3_BUCKET")
	if endpoint != "" || bucket != "" {
		if endpoint == "" || bucket == "" {
			t.Fatal("live S3 acceptance requires both KNOTRA_TEST_S3_ENDPOINT and KNOTRA_TEST_S3_BUCKET")
		}
		pathStyle, err := strconv.ParseBool(env("KNOTRA_TEST_S3_PATH_STYLE", "false"))
		if err != nil {
			t.Fatal("KNOTRA_TEST_S3_PATH_STYLE must be a boolean")
		}
		t.Log("using configured live S3 provider")
		return blob.S3Config{Bucket: bucket, Endpoint: endpoint, Region: env("KNOTRA_TEST_S3_REGION", "us-east-1"),
			Prefix: path.Join(os.Getenv("KNOTRA_TEST_S3_PREFIX"), prefix), PathStyle: pathStyle}
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	return blob.S3Config{Bucket: "multiworker", Prefix: prefix, Endpoint: multiworkerS3(t).URL, Region: "us-east-1", PathStyle: true}
}

func multiworkerOutcomes(t *testing.T, ctx context.Context, settings blob.S3Config, engineID string) *execution.OutcomeFiles {
	t.Helper()
	settings.Prefix = path.Join(settings.Prefix, engineID)
	objects, err := blob.NewS3(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := execution.OpenRemoteOutcomeFiles(objects)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outcomes.Close() })
	return outcomes
}

func multiworkerS3(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	objects := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		data, exists := objects[req.URL.Path]
		switch req.Method {
		case http.MethodPut:
			etag := fmt.Sprintf(`"%x"`, sha256.Sum256(data))
			if (exists && req.Header.Get("If-None-Match") == "*") || (req.Header.Get("If-Match") != "" && (!exists || req.Header.Get("If-Match") != etag)) {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
				return
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			objects[req.URL.Path] = body
			w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(body)))
			w.WriteHeader(http.StatusOK)
		case http.MethodGet, http.MethodHead:
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(data)))
			if req.Method == http.MethodGet {
				_, _ = w.Write(data)
			}
		default:
			t.Errorf("unexpected object storage method: %s", req.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
