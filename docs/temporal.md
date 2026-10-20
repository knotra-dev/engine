# Legacy Temporal backend

River is the default. Temporal is used by `serve --backend temporal`, compatibility tests, or
retained live Temporal runs and unsent legacy commands. Existing runs keep their admitted backend;
completed legacy projections alone do not require a Temporal connection.

Start the optional development service with `docker compose --profile temporal up -d --wait`. The
bundled address is `127.0.0.1:27233`; otherwise the client defaults to `127.0.0.1:7233`. Use
`--temporal-address` / `KNOTRA_TEMPORAL_ADDRESS` and `KNOTRA_TEST_TEMPORAL` for overrides.

## Execution and storage

One Knotra run uses one Workflow ID and a Continue-As-New chain. Deterministic workflow code owns
graph progress, CEL, loops, limits, deadlines and waits. Activities perform PostgreSQL writes and
model/MCP/Docker calls. Nested pipelines are graph scopes within the same workflow.

One agent attempt is one activity; individual model/tool calls have PostgreSQL journal identities.
Automatic activity retry is disabled for execution: Knotra applies the v1 retry and unknown-outcome
rules. Metadata publication can retry after a database outage without repeating confirmed external
work. Temporal `COMPLETED` means orchestration completed; inspect Knotra's result for pipeline
success.

The payload codec offloads serialized payloads of at least 8 KiB into content-addressed `payloads/`
under `--data-dir`. Writes are synced and reads check SHA-256. These files are durable state.
Preserve them with Temporal history, PostgreSQL and artifact/outcome storage for as long as retained
runs or replay need them. Unused payloads are not automatically collected. Large port values still
cost serialization time and memory; use artifacts for substantial byte volumes.

## History and compatibility

Continue-As-New is requested at 8000 events, 16 MiB history or an earlier SDK suggestion. New work
pauses until active attempts and metadata publication finish; counters, graph positions, original
deadlines and completed outputs carry forward. Knotra bounds concurrent leaf attempts, metadata
activities and pending human requests to 64 each, within any stricter profile limits. Deployment
must also respect [Temporal service limits](https://docs.temporal.io/evaluate/cloud/limits).

Workflow code must remain replay-compatible with retained histories. Changing command order needs
replay checks and an explicit deployment strategy. Automatic conversion between arbitrary workflow
versions is not provided. Every leaf call reads the immutable plan from PostgreSQL; there is no plan
cache.

## Checks

Default tests use a virtual workflow environment. With a real Temporal service, enable the large
plan/Continue-As-New and payload checks explicitly:

```sh
KNOTRA_TEST_TEMPORAL_STRESS=1 go test ./internal/engine \
  -run TestTemporalLargePlanAndHistoryContinuation -count=1 -v
KNOTRA_TEST_LARGE_PAYLOADS=1 go test ./internal/engine \
  -run TestPayloadCodecLargeAggregateHasNoHidden256MiBCap -count=1 -v
```

For retained-history replay, preserve the payload directory and set `KNOTRA_TEST_REPLAY_WORKFLOW`
plus the first `KNOTRA_TEST_REPLAY_RUN_ID`, then run
`go test ./internal/engine -run TestTemporalReplayRetainedHistory -count=1 -v`. Replay does not
invoke activities. See [verification](verification.md) for test scope and
[running](running.md#river-backup-and-restore) for the database/file backup procedure.
