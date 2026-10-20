# Verification

Default checks need no model weights or paid credentials. Integration checks use explicitly enabled
services and disposable test databases. Passing examples demonstrate their tested paths; they do not
establish throughput or universal model quality.

## Reproduce

From the repository root:

```sh
make check
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s examples/showcase/release-gate/tests -v
```

[CONTRIBUTING](../CONTRIBUTING.md#integration-tests) describes PostgreSQL, Docker, MCP and recovery
test setup. [Desktop](https://github.com/knotra-dev/desktop#checks-and-build) covers frontend,
native and real-engine client checks. Contract fixtures are documented
[separately](../contracts/v1/fixtures/README.md).

| Suite                  | Scope                                                                                                    |
| ---------------------- | -------------------------------------------------------------------------------------------------------- |
| Go unit/race checks    | Contract validation, scheduler decisions, adapters, clients and recovery rules                           |
| PostgreSQL integration | Transactions, ownership, budgets, receipts, requests and publication                                     |
| Distributed roles      | Public service processes, independent local directories, S3 protocol, worker drain and remote outcome recovery |
| Kubernetes acceptance  | Executor pod distribution, active scale-down, API replacement, forced pod deletion and Docker sandbox artifact transfers |
| Docker/MCP integration | Isolation, tool execution, cancellation and resource cleanup                                             |
| Process recovery       | SIGKILL boundaries, saved outcomes, stable requests, original deadlines and no repeated confirmed calls  |
| Backup/restore         | Stopped River engine, real pg_dump/pg_restore, complete files and retained SSE/artifact bytes            |
| Client fixtures        | HTTP/SSE, mutation reconciliation, review and binary integrity; fixture providers return fixed responses |
| Live-model acceptance  | Selected Ollama or explicitly enabled paid-provider scenarios                                            |

River uses PostgreSQL; start the development database with `docker compose up -d --wait`. Tests
create their own runs/artifacts; use a development engine. To inspect scheduler resource costs, use
`python3 scripts/benchmark_scheduler.py --help`; record the binary and environment with new
measurements.

## Recorded examples

On 21 October 2026, Kubernetes acceptance passed against OrbStack Kubernetes `v1.35.6+orb1`,
PostgreSQL 18.6 and MinIO `RELEASE.2025-09-07T16-13-09Z`. The host test harness used `-race`;
pods ran a Linux arm64 build of engine commit `4ac73bf`. API, scheduler and three executors used
independent local directories and shared PostgreSQL/S3, with no shared filesystem between pods.
`TestKubernetesWorkersShareS3AndRecoverPodLoss` completed 11 workflows with exactly 33 model-fixture
calls and matching budget debits. Scaling three executors to two while calls were active preserved
all workflows. A replacement API pod served the original binary artifact. Force-deleting an
executor after its S3 write and before database publication recovered its saved result without
another model call.

`TestKubernetesCodeTransfersArtifactsAcrossPods` completed five real Python executions through
local Docker sidecars. Three parallel consumers ran on three distinct executor pods, read the
producer's artifact from S3 through read-only mounts, and produced reports for an aggregation step.
Artifact bytes/hashes, five tool debits, released execution slots and five closed sandbox resources
were verified. The sidecars used Docker 29.4.0 and its DinD setup wrapper for cgroup nesting.
Initial fixture runs exposed missing PostgreSQL disconnected-client checks, an omitted DinD
wrapper and stale port forwarding after API rollout; those setup issues were corrected before
the passing runs. No engine runtime change was needed. See the
[Kubernetes fixture setup](../internal/integration/testdata/kubernetes.md) to reproduce.
All pods used one Kubernetes node; physical node loss and cross-machine network faults remain
untested. These checks exercise the existing Docker sandbox backend inside pods.

On 21 October 2026, `TestMultiworkerServeWithS3AndIndependentDataDirectories` and
`TestMultiworkerRecoversS3OutcomeAfterExecutorCrash` passed with the race detector, PostgreSQL 18.6
and River 0.48.0. Public `app.Serve` ran in independent API, scheduler and executor OS processes,
each with a separate local data directory and a shared S3 HTTP fixture. Three workers completed
work, one drained, and the remaining two completed more: 30 physical model calls, matching budget
debits and remote outcomes. An API replacement read the original binary artifact from remote
storage. A separate SIGKILL check stopped an executor after its S3 write but before PostgreSQL
publication; the scheduler recovered that result and a new executor finished the graph without
repeating the call. Run these checks with:

```sh
KNOTRA_TEST_DATABASE_URL='postgres://USER:PASSWORD@127.0.0.1:5432/TEST_DATABASE?sslmode=disable' \
  go test -race ./internal/integration -run '^TestMultiworker' -count=1 -v
```

The default fixture enforces conditional writes and serves stored bytes. The same two checks and
`TestMultiworkerS3StorageContract` also passed with the race detector against real MinIO
`RELEASE.2025-09-07T16-13-09Z` in an isolated, loopback-only Docker container, using the official
image digest `minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e`.
The storage contract checks concurrent identical creation, preservation after conflicting writes,
ETag-based replacement and stale-write rejection, metadata reads, and missing-object classification.
The model provider remained a fixed HTTP fixture. These checks do not cover other S3 providers,
cross-host network faults, Kubernetes sandbox configuration, or communication between separate machines.

`TestAttemptLeaseCoversBlockedOutcomeUpload` also passed with the race detector and PostgreSQL:
attempt ownership renewed while a remote upload stayed blocked beyond an injected shorter lease,
then published once with one model call and budget debit. S3 fault fixtures passed under the race
detector for late timed-out mutable writes, retries after a lost successful upload response,
ambiguous HTTP 404s, unavailable duplicate verification, and missing ETags or unsupported conditional
replacement. These controlled faults do not establish availability during real provider outages.

The Docker-enabled queue SIGKILL suite passed with the race detector (720.613 seconds), as did
the Docker adapter suite (59.739 seconds) and stopped backup/restore acceptance. The broader
integration run exposed an outdated run-session operation-count expectation: affinity now fences
the second tool before creating its intent. That assertion was updated and the case passed on
rerun; the other integration cases passed. The final upload-lease change was checked separately
with the blocked-upload regression, runtime/affinity race tests and another real-MinIO run.

To use a live S3-compatible provider, supply a disposable existing bucket and credentials through
the normal AWS credential chain, then run the same test command above with these additional values:

```sh
export KNOTRA_TEST_S3_ENDPOINT='http://127.0.0.1:9000'
export KNOTRA_TEST_S3_BUCKET='knotra-acceptance'
export KNOTRA_TEST_S3_REGION='us-east-1'
export KNOTRA_TEST_S3_PREFIX='acceptance'
export KNOTRA_TEST_S3_PATH_STYLE='true'
```

Both endpoint and bucket are required for live tests. With neither set, tests use the local fixture.
The provider must implement atomic conditional creation (`If-None-Match: *`) and replacement
(`If-Match` with ETags), plus consistent reads. Tests leave objects under their isolated engine/test
prefixes; remove the disposable bucket or prefixes after testing.

On 20 October 2026, `TestThreeExecutorProcessesShareWorkAndSurviveDrain` passed with the race
detector, real PostgreSQL 18.6 and River 0.48.0. Three executor OS processes shared local storage:
12 three-node workflows completed before one worker drained, then six completed on the remaining
two. All exports matched; exactly 54 physical fixture-model calls and root debits were recorded.
This checks local work sharing and graceful removal, without measuring inference throughput or
covering separate machines, network storage or an in-flight crash.

The same day, `TestOllamaRealStructuredWithoutDocker` and `TestOllamaRealStructuredAndAgent` passed
against actual Ollama `qwen3.5:9b`: structured output, replay without another model call, and agent
completion in Docker. These adapter checks do not exercise River/PostgreSQL restart recovery.

## Boundaries

- Recommendations remain advice; substantive quality requires the workflow's explicit checks.
- Approval stores typed reviewer/ticket fields without authenticated identity or digital signatures.
- Detailed observations can be truncated or incomplete; authoritative results remain separate.
- Unknown external effects require explicit resolution under
  [execution semantics](notation/execution.md).
- The recorded backup check covers a stopped engine; online snapshots, network filesystem faults,
  and recovery across separate machines need separate acceptance.
