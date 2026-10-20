//go:build unix

package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/knotra-dev/engine/internal/blob"
	"github.com/knotra-dev/engine/internal/client"
	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/protocol"
)

// This test mutates only the named disposable namespace. Infrastructure and
// forwarding are supplied by the caller; no cluster is created or removed here.
func TestKubernetesWorkersShareS3AndRecoverPodLoss(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_KUBERNETES") != "1" {
		t.Skip("set KNOTRA_TEST_KUBERNETES=1 for disposable Kubernetes acceptance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	kube := kubernetesAcceptance{t: t, ctx: ctx, context: requiredKubeEnv(t, "KNOTRA_TEST_KUBE_CONTEXT"),
		namespace: env("KNOTRA_TEST_KUBE_NAMESPACE", "knotra-acceptance")}
	cert, err := os.ReadFile(requiredKubeEnv(t, "KNOTRA_TEST_KUBE_CA_CERT"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cert) {
		t.Fatal("Kubernetes fixture CA contains no certificates")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	api := &client.Client{BaseURL: requiredKubeEnv(t, "KNOTRA_TEST_KUBE_API"), Token: requiredKubeEnv(t, "KNOTRA_TEST_KUBE_TOKEN"),
		StateDir: filepath.Join(t.TempDir(), "client"), HTTP: &http.Client{Transport: transport, Timeout: 10 * time.Second}}
	model := requiredKubeEnv(t, "KNOTRA_TEST_KUBE_MODEL")
	control := func(hold *bool) int {
		t.Helper()
		method := http.MethodGet
		var body []byte
		if hold != nil {
			method = http.MethodPost
			body, _ = json.Marshal(map[string]bool{"hold": *hold})
		}
		req, err := http.NewRequestWithContext(ctx, method, model+"/control", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		var status struct{ Calls int }
		if err := json.NewDecoder(response.Body).Decode(&status); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("model fixture control: HTTP %d error=%v", response.StatusCode, err)
		}
		return status.Calls
	}
	control(new(false))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		request, _ := http.NewRequestWithContext(cleanup, http.MethodPost, model+"/control", bytes.NewBufferString(`{"hold":false}`))
		response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
	})
	admin, err := pgx.Connect(ctx, requiredKubeEnv(t, "KNOTRA_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	var disconnectChecks bool
	if err := admin.QueryRow(ctx, "SELECT current_setting('client_connection_check_interval')::interval>interval '0'").Scan(&disconnectChecks); err != nil {
		t.Fatal(err)
	}
	if !disconnectChecks {
		t.Fatal("configure disposable PostgreSQL with client_connection_check_interval=1s before starting engine queries; the crash barrier requires dead clients to be detected while queries wait")
	}
	identity := engineIdentity(t, ctx, api)
	pathStyle, err := strconv.ParseBool(env("KNOTRA_TEST_S3_PATH_STYLE", "false"))
	if err != nil {
		t.Fatal(err)
	}
	outcomes := multiworkerOutcomes(t, ctx, blob.S3Config{Bucket: requiredKubeEnv(t, "KNOTRA_TEST_S3_BUCKET"),
		Endpoint: requiredKubeEnv(t, "KNOTRA_TEST_S3_ENDPOINT"), Region: env("KNOTRA_TEST_S3_REGION", "us-east-1"),
		Prefix: os.Getenv("KNOTRA_TEST_S3_PREFIX"), PathStyle: pathStyle}, identity)
	var definition struct{ Definition protocol.Definition }
	source := strings.Replace(multiworkerPipeline, "spec:\n", "spec:\n  limits: {maxConcurrentNodes: 1}\n", 1)
	pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source,
		Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
	if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
		t.Fatal(err)
	}
	var allRuns []string
	admit := func(count int) []string {
		t.Helper()
		runs := make([]string, count)
		for i := range runs {
			var accepted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID,
				"profile": env("KNOTRA_TEST_KUBE_PROFILE", "kubernetes")}, uuid.NewString(), &accepted); err != nil {
				t.Fatal(err)
			}
			runs[i] = accepted.Run.ID
		}
		allRuns = append(allRuns, runs...)
		return runs
	}
	checkRun := func(id string) {
		t.Helper()
		final := awaitTerminal(t, ctx, api, id)
		if final.Status != "succeeded" || len(final.Instances) != 3 || string(final.Outputs["result"]) != `"shared remote result"` {
			t.Fatalf("Kubernetes run=%+v", final)
		}
	}
	initialCalls := control(nil)
	for phase, count := range []int{6, 4} {
		workers := 3 - phase
		kube.command("scale", "deployment/executor", fmt.Sprintf("--replicas=%d", workers))
		kube.command("rollout", "status", "deployment/executor", "--timeout=90s")
		var pods []kubernetesPod
		kube.await(func() (bool, error) {
			pods = kube.pods("executor")
			return len(pods) == workers, nil
		})
		control(new(true))
		runs := admit(count)
		kube.await(func() (bool, error) {
			var owners int
			err := admin.QueryRow(ctx, `SELECT count(DISTINCT owner) FROM knotra_execution_attempts
				WHERE state='claimed' AND run_id=ANY($1)`, runs).Scan(&owners)
			return owners == workers, err
		})
		rows, err := admin.Query(ctx, `SELECT DISTINCT w.host_id FROM knotra_execution_attempts a
			JOIN knotra_workers w ON w.id=a.owner WHERE a.run_id=ANY($1) AND a.state='claimed'`, runs)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var host string
			if err := rows.Scan(&host); err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(pods, func(pod kubernetesPod) bool { return pod.Metadata.Name == host }) {
				t.Fatalf("attempt owner %q is not a live executor pod", host)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if phase == 0 {
			kube.command("scale", "deployment/executor", "--replicas=2")
			kube.await(func() (bool, error) {
				var draining bool
				err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knotra_workers w
					JOIN knotra_execution_attempts a ON a.owner=w.id
					WHERE w.draining AND a.state='claimed' AND a.run_id=ANY($1))`, runs).Scan(&draining)
				return draining, err
			})
			t.Log("scaling down marked an executor draining while its model call was still active")
		}
		control(new(false))
		for _, id := range runs {
			checkRun(id)
		}
		for _, pod := range pods {
			t.Logf("phase=%d executor pod=%s node=%s", phase+1, pod.Metadata.Name, pod.Spec.NodeName)
		}
	}
	content := []byte("Kubernetes artifact survives API pod replacement\x00\xff")
	var uploaded struct{ Artifact contract.Artifact }
	if err := api.Command(ctx, "/artifacts", map[string]any{"name": "pod-evidence.bin", "mediaType": "application/octet-stream",
		"content": base64.StdEncoding.EncodeToString(content)}, uuid.NewString(), &uploaded); err != nil {
		t.Fatal(err)
	}
	oldAPI := kube.pods("api")
	if len(oldAPI) != 1 {
		t.Fatalf("expected one API pod, got %d", len(oldAPI))
	}
	kube.command("delete", "pod", oldAPI[0].Metadata.Name, "--wait=true", "--timeout=90s")
	kube.command("rollout", "status", "deployment/api", "--timeout=90s")
	kube.await(func() (bool, error) {
		var info struct{ EngineID string }
		err := api.Get(ctx, "/info", &info)
		return err == nil && info.EngineID == identity, nil
	})
	if data, err := api.Bytes(ctx, "/artifacts/"+uploaded.Artifact.ID+"/content"); err != nil || !bytes.Equal(data, content) {
		t.Fatalf("replacement API artifact: %v", err)
	}

	// One held provider reply lets the test install a publication barrier without
	// altering engine code. Only the worker holding that attempt is then killed.
	control(new(true))
	run := admit(1)[0]
	kube.await(func() (bool, error) {
		var claimed int
		err := admin.QueryRow(ctx, "SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND state='claimed'", run).Scan(&claimed)
		return claimed == 1, err
	})
	barrier, err := pgx.Connect(ctx, requiredKubeEnv(t, "KNOTRA_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = barrier.Close(context.Background()) }()
	if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273659)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `CREATE FUNCTION kubernetes_outcome_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock(918273659); RETURN NEW; END $$;
		CREATE TRIGGER kubernetes_outcome_barrier BEFORE UPDATE ON knotra_execution_attempts FOR EACH ROW
		WHEN (NEW.outcome IS NOT NULL AND OLD.outcome IS NULL) EXECUTE FUNCTION kubernetes_outcome_barrier()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		connection, err := pgx.Connect(cleanup, os.Getenv("KNOTRA_TEST_DATABASE_URL"))
		if err == nil {
			_, _ = connection.Exec(cleanup, "DROP TRIGGER IF EXISTS kubernetes_outcome_barrier ON knotra_execution_attempts; DROP FUNCTION IF EXISTS kubernetes_outcome_barrier()")
			_ = connection.Close(cleanup)
		}
	})
	control(new(false))
	kube.await(func() (bool, error) {
		var blocked bool
		err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event='advisory'
			AND $1::integer=ANY(pg_blocking_pids(pid)))`, int64(barrier.PgConn().PID())).Scan(&blocked)
		return blocked, err
	})
	var owner, host, key string
	if err := admin.QueryRow(ctx, `SELECT a.owner,w.host_id,a.outcome_key FROM knotra_execution_attempts a
		JOIN knotra_workers w ON w.id=a.owner WHERE a.run_id=$1 AND a.state='claimed' AND a.outcome IS NULL`, run).Scan(&owner, &host, &key); err != nil {
		t.Fatal(err)
	}
	if outcome, err := outcomes.GetContext(ctx, key); err != nil || outcome.Failure != nil || string(outcome.Outputs["answer"].JSON) != `"shared remote result"` {
		t.Fatalf("pod result was not saved in S3 before database publication: %+v %v", outcome, err)
	}
	var publisher int32
	if err := admin.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE wait_event='advisory'
		AND $1::integer=ANY(pg_blocking_pids(pid)) ORDER BY query_start LIMIT 1`, int64(barrier.PgConn().PID())).Scan(&publisher); err != nil {
		t.Fatal(err)
	}
	kube.command("delete", "pod", host, "--grace-period=0", "--force", "--wait=false")
	kube.await(func() (bool, error) {
		var gone bool
		err := admin.QueryRow(ctx, "SELECT NOT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)", publisher).Scan(&gone)
		return gone, err
	})
	if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273659)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "DROP TRIGGER kubernetes_outcome_barrier ON knotra_execution_attempts; DROP FUNCTION kubernetes_outcome_barrier()"); err != nil {
		t.Fatal(err)
	}
	checkRun(run)
	var recovered int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM knotra_execution_attempts
		WHERE run_id=$1 AND owner=$2 AND state='completed' AND number=1`, run, owner).Scan(&recovered); err != nil || recovered != 1 {
		t.Fatalf("crashed pod's original outcome was not recovered: recovered=%d error=%v", recovered, err)
	}
	var attempts, invalid, used, slots int
	if err := admin.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE state<>'completed' OR number<>1),
		(SELECT sum(used) FROM knotra_budgets WHERE run_id=ANY($1) AND scope=run_id AND kind='model'),
		(SELECT count(*) FROM knotra_execution_slots WHERE run_id=ANY($1) AND released_at IS NULL)
		FROM knotra_execution_attempts WHERE run_id=ANY($1)`, allRuns).Scan(&attempts, &invalid, &used, &slots); err != nil {
		t.Fatal(err)
	}
	if attempts != 33 || invalid != 0 || used != 33 || slots != 0 || control(nil)-initialCalls != 33 {
		t.Fatalf("Kubernetes totals: attempts=%d invalid=%d budget=%d slots=%d calls=%d", attempts, invalid, used, slots, control(nil)-initialCalls)
	}
	kube.command("rollout", "status", "deployment/executor", "--timeout=90s")
	t.Logf("11 workflows, 33 calls/debits: three executor pods, scale to two, API replacement artifact and recovery after forced deletion of %s", host)
}

type kubernetesPod struct {
	Metadata struct {
		Name              string
		DeletionTimestamp *string
	}
	Spec   struct{ NodeName string }
	Status struct {
		Phase      string
		Conditions []struct{ Type, Status string }
	}
}

type kubernetesAcceptance struct {
	t         *testing.T
	ctx       context.Context
	context   string
	namespace string
}

func (k kubernetesAcceptance) command(args ...string) []byte {
	k.t.Helper()
	arguments := append([]string{"--context", k.context, "--namespace", k.namespace}, args...)
	output, err := exec.CommandContext(k.ctx, "kubectl", arguments...).CombinedOutput()
	if err != nil {
		k.t.Fatalf("kubectl %v: %v\n%s", args, err, output)
	}
	return output
}

func (k kubernetesAcceptance) pods(app string) []kubernetesPod {
	k.t.Helper()
	var result struct{ Items []kubernetesPod }
	if err := json.Unmarshal(k.command("get", "pods", "-l", "app="+app, "-o", "json"), &result); err != nil {
		k.t.Fatal(err)
	}
	return slices.DeleteFunc(result.Items, func(pod kubernetesPod) bool {
		return pod.Metadata.DeletionTimestamp != nil || pod.Status.Phase != "Running" ||
			!slices.ContainsFunc(pod.Status.Conditions, func(condition struct{ Type, Status string }) bool {
				return condition.Type == "Ready" && condition.Status == "True"
			})
	})
}

func (k kubernetesAcceptance) await(ready func() (bool, error)) {
	k.t.Helper()
	ctx, cancel := context.WithTimeout(k.ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := ready()
		if err != nil {
			k.t.Fatal(err)
		}
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			k.t.Fatalf("Kubernetes acceptance boundary: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func requiredKubeEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required for Kubernetes acceptance", name)
	}
	return value
}
