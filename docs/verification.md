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
test setup. [Desktop](../app/README.md#checks-and-build) covers frontend, native and real-engine
client checks. Contract fixtures are documented [separately](../contracts/v1/fixtures/README.md).

| Suite                  | Scope                                                                                                    |
| ---------------------- | -------------------------------------------------------------------------------------------------------- |
| Go unit/race checks    | Contract validation, scheduler decisions, adapters, clients and recovery rules                           |
| PostgreSQL integration | Transactions, ownership, budgets, receipts, requests and publication                                     |
| Docker/MCP integration | Isolation, tool execution, cancellation and resource cleanup                                             |
| Process recovery       | SIGKILL boundaries, saved outcomes, stable requests, original deadlines and no repeated confirmed calls  |
| Backup/restore         | Stopped River engine, real pg_dump/pg_restore, complete files and retained SSE/artifact bytes            |
| Client fixtures        | HTTP/SSE, mutation reconciliation, review and binary integrity; fixture providers return fixed responses |
| Live-model acceptance  | Selected Ollama or explicitly enabled paid-provider scenarios                                            |

River is the default backend. Legacy compatibility and comparison tests additionally require
`docker compose --profile temporal up -d --wait` and `KNOTRA_TEST_TEMPORAL=127.0.0.1:27233`. Tests
create their own runs/artifacts; use a development engine. To inspect scheduler resource costs, use
`python3 scripts/benchmark_scheduler.py --help`; record the binary and environment with new
measurements.

## Recorded examples

On 5 October 2026, actual local `qwen3.5:9b`, Docker/Node, Temporal and PostgreSQL completed the
[game repair](../examples/showcase/game-repair/README.md) in two iterations. The first candidate
failed an executed test; the correction passed 3159 checks across 929 positions and 236 completed
games. An independent sandbox verified and packaged the source; the downloaded game worked offline
in a browser. Generation from scratch also passed. An earlier repair exhausted four attempts after
syntax errors and published no game.

The [release gate](../examples/showcase/release-gate/README.md) completed 30 nodes, 36 regression
tests and 18 gates for six sample services. Code verified agent citations, a human approved the
dossier, and exported bytes matched the reviewed snapshot. An incompatible API change failed before
agent review or authorization. These original Temporal executions were not repeated on River by
virtue of changing the default backend.

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
  multi-host recovery and mixed-backend backups need separate acceptance.
