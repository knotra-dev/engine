# Kubernetes acceptance fixtures

`TestKubernetesWorkersShareS3AndRecoverPodLoss` uses a disposable namespace supplied by the caller.
It scales the `executor` Deployment from three pods to two during active model calls, replaces an
API pod, and force-deletes an executor after its S3 outcome is saved but before database publication.
It checks 33 physical calls, matching budgets, unchanged artifact bytes and recovery without replay.
The harness creates no cluster or infrastructure and does not remove the namespace.

The full `-run '^TestKubernetes'` command also runs
`TestKubernetesCodeTransfersArtifactsAcrossPods`. Load the `kubernetes-sandbox` profile on every
engine role, using the exact `kubernetesSandboxProfile` fixture in
[kubernetes_sandbox_test.go](../kubernetes_sandbox_test.go). It requires a `python_box` sandbox with
`python:3.13-alpine`, network `none`, CPU 1, memory 256 MiB, disk 64 MiB and 64 PIDs, with
`process.exec`, `files.read` and `files.write` allowed. The profile needs at least three concurrent
nodes and five tool calls. Every Docker daemon must have the same sandbox image ID.

API and executor pods need local Docker for sandbox admission/execution. In the tested setup, each
has its own Docker sidecar sharing matching absolute data and helper paths (`/var/lib/knotra` and
`/shared`) with its engine container. Pods exchange artifacts through S3 and have no shared
filesystem between them. This exercises the existing Docker backend; no Kubernetes sandbox backend
or production deployment manifests are supplied by these fixtures.
The tested sidecars were privileged and ran the Docker image's `/usr/local/bin/dind` setup wrapper
before starting `dockerd`, enabling the nested cgroup controllers needed for sandbox limits.

Prepare Deployments named `api`, `scheduler`, `executor`, `postgres`, `minio` and `model-fixture`,
with matching `app` labels. Executors need `--execution-workers=1` and their pod name as `--host-id`.
All engine roles share PostgreSQL, S3 configuration and a profile named `kubernetes` containing an
Ollama connection `local`, model `fixture`, pointing to the fixture's port 11434. The Python fixture
is `kubernetes_model_fixture.py`; its `/control` endpoint holds/releases calls and reports counts.

Set `client_connection_check_interval=1s` on the disposable PostgreSQL server before starting engine
queries. The test deliberately blocks publication with an advisory-lock trigger and waits for the
deleted pod's database backend to disappear before releasing it. PostgreSQL's default `0` cannot
detect that dead client while the query waits; reloading the setting after a query has started does
not arm its timer. This is test-fixture configuration, not a production engine change.
See PostgreSQL's [connection settings](https://www.postgresql.org/docs/18/runtime-config-connection.html#GUC-CLIENT-CONNECTION-CHECK-INTERVAL).

Forward API, PostgreSQL, MinIO and model-fixture services to local ports. API forwarding must restart
after the API pod is replaced. Export:

```sh
export KNOTRA_TEST_KUBERNETES=1
export KNOTRA_TEST_KUBE_CONTEXT=YOUR_DISPOSABLE_CONTEXT
export KNOTRA_TEST_KUBE_NAMESPACE=knotra-acceptance
export KNOTRA_TEST_KUBE_API=https://localhost:18487
export KNOTRA_TEST_KUBE_TOKEN=YOUR_FIXTURE_TOKEN
export KNOTRA_TEST_KUBE_CA_CERT=/absolute/path/to/fixture-ca.crt
export KNOTRA_TEST_KUBE_MODEL=http://127.0.0.1:11487
export KNOTRA_TEST_DATABASE_URL=YOUR_DISPOSABLE_POSTGRES_URL
export KNOTRA_TEST_S3_ENDPOINT=http://127.0.0.1:19087
export KNOTRA_TEST_S3_BUCKET=knotra
export KNOTRA_TEST_S3_REGION=us-east-1
export KNOTRA_TEST_S3_PREFIX=YOUR_ENGINE_PREFIX
export KNOTRA_TEST_S3_PATH_STYLE=true
# Set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY for the disposable MinIO service.
go test -race ./internal/integration -run '^TestKubernetes' -count=1 -v
```

Standard `KUBECONFIG` is honored. S3 bucket, prefix, region and credentials must match the deployed
engine. `KNOTRA_TEST_KUBE_PROFILE` optionally selects another profile name. Use this fixture without
other model callers; the test verifies exact call counts. Pods may share one Kubernetes node, so
passing this test alone does not establish recovery across physical machines or node loss.
