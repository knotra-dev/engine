# Verification

Knotra uses deterministic contract/workflow tests, opt-in service integration tests and complete
CLI/browser executions. Passing examples establish the paths below, not throughput or universal
model quality. Default checks need no paid API credentials.

## Current runtime

River is the default execution backend. Its queues and authoritative execution records use the same
PostgreSQL database; recovery also requires complete artifact and outcome directories. Normal
Compose and quickstart do not start Temporal. Current River checks and recorded results are in the
[implementation report](scheduler-river-implementation.md).

The examples and measurements below retain their original verification dates and backends. Legacy
Temporal results are historical evidence, not a claim that those runs were repeated on River.
Compatibility and comparison tests still require the optional Temporal service.

## Three executor processes: 20 October 2026

`TestThreeExecutorProcessesShareWorkAndSurviveDrain` passed with the race detector against real
PostgreSQL 18.6 and River 0.48.0. One scheduler runtime admits work directly through the existing
store boundary; three separate executor OS processes use independent database pools, River clients,
worker identities and heartbeats, with shared local outcome/artifact directories.

- All three executors hold a physical call concurrently before the test releases their responses.
- Twelve three-node workflows complete on the three executors. One executor then drains and exits;
  six more workflows complete on the remaining two, with both participating concurrently.
- All 18 workflows have the expected complete export. There are exactly 54 physical HTTP calls,
  completed first attempts and root model debits, with no unreleased slots or Temporal outbox rows.

The model endpoint returns fixed responses so calls can be counted exactly. This checks local
process separation and graceful removal of a worker; it does not measure model throughput, test
separate machines or network storage, or cover an in-flight executor crash. The public `serve`
command still uses the combined role and exclusive engine lease; split-process CLI deployment is not
exposed.

With a disposable PostgreSQL database, run from the repository root:

```sh
KNOTRA_TEST_DATABASE_URL='postgres://localhost/knotra_dev?sslmode=disable' \
  go test -mod=readonly -race -count=1 -timeout=4m ./internal/queue \
  -run '^TestThreeExecutorProcessesShareWorkAndSurviveDrain$' -v
```

## Local Qwen smoke checks: 20 October 2026

Two existing adapter tests passed against actual Ollama `qwen3.5:9b`:

- `TestOllamaRealStructuredWithoutDocker`: the output is JSON `answer: 42`; replay returns the
  recorded response without another model call.
- `TestOllamaRealStructuredAndAgent`: structured generation succeeds, then an agent completes
  through `knotra_finish` in a Docker sandbox using a helper built from the current sources.

These checks exercise the model adapter and agent runtime, not the River/PostgreSQL service or
restart recovery. With Ollama and Docker running, reproduce from the repository root:

```sh
make helper
docker pull python:3.13-alpine
KNOTRA_TEST_OLLAMA=http://127.0.0.1:11434 \
  KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper" \
  KNOTRA_TEST_WORKDIR="$PWD/.knotra/test-work" \
  go test -mod=readonly -count=1 -timeout=7m ./internal/adapters \
  -run '^TestOllamaRealStructured(WithoutDocker|AndAgent)$' -v
```

## Recorded examples: 5 October 2026

### Game generation and correction

The recorded game checks used actual local `qwen3.5:9b`, Docker/Node, Temporal and PostgreSQL.
Repair run `a4cbb411-34dd-44f9-8563-c754b283e97b` completed in two loop iterations with no
diagnostics:

1. The supplied, deliberately incorrect opponent lost against a legal sequence of moves.
2. The model received that exact failure and source; its correction passed all 3159 checks across
   929 positions and 236 completed games.

A separate sandbox verified the final source before producing the playable HTML and test report.
Source SHA-256: `f46794bbb02cd5d1248ac671eab2aa6504cdf20a3b9c91d1679cbb71029ffcfb`. The downloaded
game ran in a browser with networking denied; a human move, computer reply and restart worked
without page errors. Generation from scratch also passed in one iteration, run
`fe7f4980-70b1-41ea-bf7c-6fb09b9cb380`.

An earlier repair run reached the four-iteration limit after repeated model syntax errors. It
published no playable game. The prompt now distinguishes the opponent helper from local variables. A
default-input smoke run also exposed validation of an empty input before a skipped branch; the seed
input now permits empty text, and the successful default run above verifies that correction.
Failures remain in execution history.

Reproduce [generation](../examples/starter/tic-tac-toe/pipeline.yaml) and the
[seeded repair exercise](../examples/showcase/game-repair/README.md). Fixed tests cover all winning
lines, terminal states, move legality, immutability, repeatable choices and every legal opponent
continuation against the tested computer implementation as both X and O.

### Service fleet release gate

Run `920bc2ea-76a1-4c72-8d02-7f90809edeed` completed all 30 nodes with no diagnostics or browser
errors. Six executable sample services passed 36 regression tests and 18 release gates. Three agents
read scoped evidence and the release policy; code verified their citations. A human response with a
change ticket authorized the evidence packet.

The exported ZIP was checked against the actual reviewed bytes and input snapshot:

| File                | SHA-256                                                            |
| ------------------- | ------------------------------------------------------------------ |
| Repository snapshot | `bdd4fe2f79ac52d61005d21241b363f4008e9a125a15386502272f9f48e55333` |
| Executed checks     | `b7ac344c6adf6221e30a2d592c34690b761793156c7490903f1a76d489b9b257` |
| Reviewed dossier    | `64ce4cac59609f74241382e1f8860328b5670fda1f7f9e297bd3b0d6f6edf31e` |

A separate snapshot widened an existing API response enum. Run
`fd7f0b74-5b63-40c0-bc45-30f3c890b816` failed at the deterministic gate: no agent started, no human
request appeared and no authorized packet was published.

The [package](../examples/showcase/release-gate/README.md) includes 11 deterministic tests. They
execute the sample regressions and exercise incompatible/unsupported API schemas, unsafe or
ambiguous configuration, archive confinement, incomplete service coverage, mixed snapshot hashes,
invented citations and byte-preserving publication.

The [screenshots](../README.md#workflow-in-pictures) show these real executions in light theme. The
graph was captured during work; the agent inspector shows two model calls and actual evidence reads.
Inbox's prompt, deadline, files and details now share the header/form padding. Browser measurements
confirmed aligned left edges; four affected review/artifact checks passed, including long IDs at
1024/1440 px and preservation of a response draft after failed export.

## Providers and recovery

Provider checks cover structured arguments, tool IDs, private conversation context,
malformed/incomplete replies, credentials, HTTP failures, cancellation, budgets and durable replay.

Process-recovery tests cover River/PostgreSQL and legacy Temporal/PostgreSQL with Docker tools. The
earlier Temporal verification killed the engine while waiting for a human and restarted it; request
identity remained unchanged and each provider received exactly one generation request. Reviewed
research runs additionally checked SIGKILL recovery, SSE replay, valid/late human responses,
cancellation and byte-identical publication. These paths are covered by the reproducible integration
tests below.

The broader real-service suite exercised three concurrent pipelines with all nine node types, actual
Ollama, Docker, MCP, Temporal and PostgreSQL. A separate Temporal stress test completed 1,004 nodes
through three Continue-As-New transitions; retained histories replayed. Its external activities were
controlled fixtures, so this is history evidence rather than a model throughput benchmark.
[Temporal details](temporal.md) record the measurements.

The v0.1.1 release passed Go checks, desktop/docs checks and four CLI archive builds. Archives
contain helpers for both Docker architectures. A standalone quickstart produced its greeting and
retained the same run on restart. A release-test scheduling assumption was corrected without
changing production scheduling; 200 focused race repetitions and `make check` passed.

## Reproduce the checks

From the repository root:

```sh
make check
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s examples/showcase/release-gate/tests -v
```

Frontend and native commands are in [CONTRIBUTING](../CONTRIBUTING.md). Optional browser acceptance
uses `KNOTRA_E2E_ENDPOINT` and submits packages through the same API as desktop.

For real services, use a disposable database and the addresses/helper/work directory described in
[Running Knotra](running.md) and [integration setup](../examples/integration/README.md). Then enable
only the checks needed. The commands below include legacy compatibility and stress checks, so start
the optional service with `docker compose --profile temporal up -d --wait` and set
`KNOTRA_TEST_TEMPORAL=127.0.0.1:27233` when using bundled Compose:

```sh
KNOTRA_TEST_REAL=1 go test -mod=readonly -race -count=1 -timeout 30m \
  ./internal/integration -run '^TestReal(ConcurrentPipelines|ProcessRecovery)$' -v
KNOTRA_TEST_RECOVERY=1 go test -mod=readonly -race -count=1 -timeout 10m \
  ./internal/integration -run '^Test(CloudProtocolsWithRealEngineRecovery|RealProcessRecovery)$' -v
KNOTRA_TEST_TEMPORAL_STRESS=1 go test -mod=readonly ./internal/engine \
  -run '^TestTemporalLargePlanAndHistoryContinuation$' -count=1 -v
```

## Boundaries

Agent recommendations remain advice. Approval records typed reviewer/ticket fields rather than
authenticated identities or digital signatures.

Quickstart runs on one controlled host. Shared storage, multi-user access, retention and workflow
upgrade policy remain separate work. [Execution semantics](notation/execution.md) define retries and
unknown outcomes.
