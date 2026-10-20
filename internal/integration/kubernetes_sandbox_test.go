//go:build unix

package integration

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/knotra-dev/engine/internal/client"
	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/protocol"
)

func TestKubernetesSandboxFixture(t *testing.T) {
	profile, diagnostics := contract.ParseProfile([]byte(kubernetesSandboxProfile))
	if contract.HasErrors(diagnostics) {
		t.Fatalf("sandbox profile: %+v", diagnostics)
	}
	if _, diagnostics := contract.Compile(kubernetesSandboxPackage(t), &profile); contract.HasErrors(diagnostics) {
		t.Fatalf("sandbox package: %+v", diagnostics)
	}
}

func TestKubernetesCodeTransfersArtifactsAcrossPods(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_KUBERNETES") != "1" {
		t.Skip("set KNOTRA_TEST_KUBERNETES=1 for disposable Kubernetes acceptance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
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
	admin, err := pgx.Connect(ctx, requiredKubeEnv(t, "KNOTRA_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	kube.command("scale", "deployment/executor", "--replicas=3")
	kube.command("rollout", "status", "deployment/executor", "--timeout=90s")
	var hosts []string
	kube.await(func() (bool, error) {
		hosts = nil
		for _, pod := range kube.pods("executor") {
			hosts = append(hosts, pod.Metadata.Name)
		}
		if len(hosts) != 3 {
			return false, nil
		}
		var workers int
		err := admin.QueryRow(ctx, `SELECT count(DISTINCT host_id) FROM knotra_workers
			WHERE host_id=ANY($1) AND NOT draining AND heartbeat_at>clock_timestamp()-interval '10 seconds'`, hosts).Scan(&workers)
		return workers == 3, err
	})
	var definition struct{ Definition protocol.Definition }
	if err := api.Command(ctx, "/definitions", map[string]any{"package": kubernetesSandboxPackage(t)}, uuid.NewString(), &definition); err != nil {
		t.Fatal(err)
	}
	var accepted struct{ Run protocol.Run }
	if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID,
		"profile": env("KNOTRA_TEST_KUBE_SANDBOX_PROFILE", "kubernetes-sandbox")}, uuid.NewString(), &accepted); err != nil {
		t.Fatal(err)
	}
	final := awaitTerminal(t, ctx, api, accepted.Run.ID)
	if final.Status != "succeeded" || len(final.Instances) != 5 {
		t.Fatalf("sandbox run=%s status=%s diagnostics=%+v", final.ID, final.Status, final.Diagnostics)
	}
	var summary struct {
		Nonce            string `json:"nonce"`
		SHA256           string `json:"sha256"`
		VerifiedBranches int    `json:"verifiedBranches"`
	}
	if err := json.Unmarshal(final.Outputs["summary"], &summary); err != nil || summary.Nonce == "" || summary.VerifiedBranches != 3 {
		t.Fatalf("sandbox summary=%+v error=%v", summary, err)
	}
	for _, name := range []string{"source.txt", "accepted.json"} {
		index := slices.IndexFunc(final.Artifacts, func(artifact contract.Artifact) bool { return artifact.Name == name })
		if index < 0 {
			t.Fatalf("missing artifact %q: %+v", name, final.Artifacts)
		}
		artifact := final.Artifacts[index]
		data, err := api.Bytes(ctx, "/artifacts/"+artifact.ID+"/content")
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		if int64(len(data)) != artifact.Size || hex.EncodeToString(digest[:]) != artifact.SHA256 {
			t.Fatalf("artifact %q failed content digest/size verification", name)
		}
		if name == "source.txt" && (string(data) != "Kubernetes sandbox artifact "+summary.Nonce+"\n" || artifact.SHA256 != summary.SHA256) {
			t.Fatalf("source artifact does not match sandbox summary: %q %+v", data, summary)
		}
		if name == "accepted.json" {
			var result any
			var output any
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(final.Outputs["summary"], &output); err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(result)
			want, _ := json.Marshal(output)
			if string(got) != string(want) {
				t.Fatalf("accepted artifact does not match published JSON: %s != %s", got, want)
			}
		}
		t.Logf("run=%s artifact=%s id=%s bytes=%d sha256=%s", final.ID, name, artifact.ID, artifact.Size, artifact.SHA256)
	}
	rows, err := admin.Query(ctx, `SELECT n.node_id,w.host_id FROM knotra_execution_attempts a
		JOIN knotra_execution_nodes n ON n.run_id=a.run_id AND n.id=a.instance_id
		JOIN knotra_workers w ON w.id=a.owner WHERE a.run_id=$1 AND n.node_id=ANY($2) ORDER BY n.node_id`,
		final.ID, []string{"left", "center", "right"})
	if err != nil {
		t.Fatal(err)
	}
	consumerHosts := map[string]bool{}
	for rows.Next() {
		var node, host string
		if err := rows.Scan(&node, &host); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(hosts, host) {
			t.Fatalf("sandbox node %s ran outside live executor pods: %s", node, host)
		}
		consumerHosts[host] = true
		t.Logf("sandbox node=%s executor pod=%s", node, host)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(consumerHosts) != 3 {
		t.Fatalf("parallel artifact consumers used %d executor pods, want 3", len(consumerHosts))
	}
	var attempts, invalid, toolBudget, modelBudget, slots, resources, open int
	kube.await(func() (bool, error) {
		err := admin.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE state<>'completed' OR number<>1),
			COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool'),0),
			COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),0),
			(SELECT count(*) FROM knotra_execution_slots WHERE run_id=$1 AND released_at IS NULL),
			(SELECT count(*) FROM knotra_resources WHERE run_id=$1 AND kind='sandbox'),
			(SELECT count(*) FROM knotra_resources WHERE run_id=$1 AND state<>'closed')
			FROM knotra_execution_attempts WHERE run_id=$1`, final.ID).Scan(&attempts, &invalid, &toolBudget, &modelBudget, &slots, &resources, &open)
		return open == 0, err
	})
	if attempts != 5 || invalid != 0 || toolBudget != 5 || modelBudget != 0 || slots != 0 || resources != 5 {
		t.Fatalf("sandbox totals: attempts=%d invalid=%d toolBudget=%d modelBudget=%d slots=%d resources=%d open=%d",
			attempts, invalid, toolBudget, modelBudget, slots, resources, open)
	}
	t.Logf("five sandbox attempts/debits, three executor pods, verified S3 artifact transfers and five closed sandbox resources; run=%s", final.ID)
}

func kubernetesSandboxPackage(t *testing.T) contract.Package {
	t.Helper()
	scalar := func(kind, from string) contract.Port {
		port := contract.Port{Schema: json.RawMessage(`{"type":"` + kind + `"}`)}
		if from != "" {
			port.Bind = &contract.Binding{From: from}
		}
		return port
	}
	file := func(mediaType, collect string) contract.Port {
		return contract.Port{Artifact: &contract.ArtifactPort{MediaTypes: []string{mediaType}},
			Collect: &contract.Collect{Path: collect, MediaType: mediaType}}
	}
	nodes := map[string]contract.Node{
		"produce": {Type: "code", Sandbox: "work", Code: &contract.CodeNode{Command: []string{"python3", "/package/produce.py"}},
			Outputs: map[string]contract.Port{"nonce": scalar("string", ""), "sha256": scalar("string", ""), "file": file("text/plain", "source.txt")}},
	}
	verify := contract.Node{Type: "code", Sandbox: "work", Code: &contract.CodeNode{Command: []string{"python3", "/package/verify.py"}},
		Inputs:  map[string]contract.Port{"nonce": scalar("string", "nodes.produce.outputs.nonce"), "sha256": scalar("string", "nodes.produce.outputs.sha256")},
		Outputs: map[string]contract.Port{"summary": scalar("object", ""), "file": file("application/json", "accepted.json")}}
	for _, branch := range []string{"left", "center", "right"} {
		branchPort := scalar("string", "")
		branchPort.Bind = &contract.Binding{Value: json.RawMessage(`"` + branch + `"`)}
		nodes[branch] = contract.Node{Type: "code", Sandbox: "work", Code: &contract.CodeNode{Command: []string{"python3", "/package/consume.py"}},
			Inputs: map[string]contract.Port{
				"nonce": scalar("string", "nodes.produce.outputs.nonce"), "sha256": scalar("string", "nodes.produce.outputs.sha256"), "branch": branchPort,
				"source": {Artifact: &contract.ArtifactPort{MediaTypes: []string{"text/plain"}}, Mount: "source.txt", Bind: &contract.Binding{From: "nodes.produce.outputs.file"}}},
			Outputs: map[string]contract.Port{"file": file("application/json", "report.json")}}
		verify.Inputs[branch] = contract.Port{Artifact: &contract.ArtifactPort{MediaTypes: []string{"application/json"}},
			Mount: "report_" + branch + ".json", Bind: &contract.Binding{From: "nodes." + branch + ".outputs.file"}}
	}
	nodes["verify"] = verify
	pipeline := contract.Pipeline{APIVersion: "knotra/v1", Kind: "Pipeline", Metadata: contract.Metadata{Name: "kubernetes-sandbox-artifacts"},
		Spec: contract.Spec{Files: []string{"produce.py", "consume.py", "verify.py"},
			Sandboxes: map[string]contract.SandboxResource{"work": {Profile: "python_box"}},
			Defaults:  contract.Defaults{Execution: contract.Execution{Timeout: "1m"}},
			Graph: contract.Graph{Nodes: nodes, Outputs: map[string]contract.Port{
				"summary":  scalar("object", "nodes.verify.outputs.summary"),
				"source":   {Artifact: &contract.ArtifactPort{MediaTypes: []string{"text/plain"}}, Bind: &contract.Binding{From: "nodes.produce.outputs.file"}},
				"accepted": {Artifact: &contract.ArtifactPort{MediaTypes: []string{"application/json"}}, Bind: &contract.Binding{From: "nodes.verify.outputs.file"}}}}}}
	source, err := json.Marshal(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	return contract.Package{Entrypoint: "pipeline.json", Source: string(source), Files: []contract.File{
		{Path: "pipeline.json", Content: source}, {Path: "produce.py", Content: []byte(kubernetesSandboxProduce)},
		{Path: "consume.py", Content: []byte(kubernetesSandboxConsume)}, {Path: "verify.py", Content: []byte(kubernetesSandboxVerify)}}}
}

const kubernetesSandboxProfile = `{"apiVersion":"knotra/v1","kind":"EngineProfile","metadata":{"name":"kubernetes-sandbox"},"spec":{"sandboxes":{"python_box":{"image":"python:3.13-alpine","resources":{"cpu":1,"memoryMiB":256,"diskMiB":64,"pids":64},"network":{"mode":"none"},"allowedTools":["process.exec","files.read","files.write"]}},"limits":{"timeout":"5m","maxConcurrentNodes":3,"maxNodeInstances":100,"maxModelCalls":1,"maxToolCalls":20}}}`

const kubernetesSandboxProduce = `import hashlib, json, os, uuid
from pathlib import Path
nonce = uuid.uuid4().hex
data = ("Kubernetes sandbox artifact " + nonce + "\n").encode()
Path("source.txt").write_bytes(data)
Path(os.environ["KNOTRA_OUTPUT_JSON"]).write_text(json.dumps({"nonce": nonce, "sha256": hashlib.sha256(data).hexdigest()}))
`

const kubernetesSandboxConsume = `import hashlib, json, os, socket, time
from pathlib import Path
values = json.loads(Path(os.environ["KNOTRA_INPUT_JSON"]).read_text())["values"]
data = Path("source.txt").read_bytes()
assert data == ("Kubernetes sandbox artifact " + values["nonce"] + "\n").encode()
assert hashlib.sha256(data).hexdigest() == values["sha256"]
try:
    Path("source.txt").write_text("modified")
except OSError:
    pass
else:
    raise AssertionError("input artifact mount was writable")
time.sleep(10)
report = {"branch": values["branch"], "nonce": values["nonce"], "sha256": values["sha256"], "bytes": len(data), "sandbox": socket.gethostname()}
Path("report.json").write_text(json.dumps(report))
Path(os.environ["KNOTRA_OUTPUT_JSON"]).write_text("{}")
`

const kubernetesSandboxVerify = `import json, os
from pathlib import Path
values = json.loads(Path(os.environ["KNOTRA_INPUT_JSON"]).read_text())["values"]
reports = [json.loads(Path("report_" + branch + ".json").read_text()) for branch in ("left", "center", "right")]
assert {report["branch"] for report in reports} == {"left", "center", "right"}
assert len({report["sandbox"] for report in reports}) == 3
assert all(report["nonce"] == values["nonce"] and report["sha256"] == values["sha256"] for report in reports)
assert len({report["bytes"] for report in reports}) == 1
summary = {"nonce": values["nonce"], "sha256": values["sha256"], "verifiedBranches": len(reports), "reports": reports}
Path("accepted.json").write_text(json.dumps(summary))
Path(os.environ["KNOTRA_OUTPUT_JSON"]).write_text(json.dumps({"summary": summary}))
`
