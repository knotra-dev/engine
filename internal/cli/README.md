# CLI

`cmd/knotra` provides signal handling and exit codes. `internal/cli` owns parsing and presentation;
network behavior and mutation reconciliation belong to `internal/client`. CLI commands never run
pipeline workloads locally.

The client shares the [versioned HTTP/SSE contract](../../docs/api/desktop-v1.md) with desktop.
[Running](../../docs/running.md) covers infrastructure and engine configuration;
[verification](../../docs/verification.md) lists real acceptance checks. The CLI remains usable
without installing the desktop toolchain.

`doctor` checks local prerequisites without changing infrastructure. `quickstart` prepares
persistent Compose services, extracts editable examples, runs the greeting and serves until Ctrl-C.
Both print human-readable progress and reject `--json`. Use `--dir` to select saved state,
`--provider` and `--model` for cloud models, and `--no-run` to omit greeting submission. The cloud
path reads `OPENAI_API_KEY` or `ANTHROPIC_API_KEY`; see [running](../../docs/running.md).

Global options:

- `--endpoint` / `KNOTRA_ENDPOINT`, default `http://127.0.0.1:8787`.
- `KNOTRA_TOKEN` for engine bearer authentication. Tokens are not command flags or persisted
  receipts.
- `--state-dir` / `KNOTRA_STATE_DIR` for durable mutation receipts and event cursors; defaults to
  the OS configuration directory's `knotra` subdirectory.
- `--json` for JSON objects; event streams emit one JSON event per line.

```sh
knotra validate pipeline.yaml
knotra validate pipeline.yaml --profile engine-profile.yaml
knotra validate pipeline.yaml --remote --profile local --inputs inputs.json
knotra run pipeline.yaml --profile local --input 'topic="AI"' --wait
knotra runs list --all
knotra runs get RUN_ID --json
knotra runs watch RUN_ID
knotra runs cancel RUN_ID
knotra requests list --run RUN_ID --all --json
knotra requests respond REQUEST_ID --output 'approved=true'
knotra artifacts upload input.txt --media-type text/plain
knotra artifacts download ARTIFACT_ID --output report.txt
knotra operations list
knotra operations retry OPERATION_ID
```

`run` publishes a package version, then admits a run. The two commands have distinct stable
idempotency identities derived from `--idempotency-key` when provided. `--definition ID` starts an
existing immutable definition instead of loading a file. JSON inputs use `--inputs FILE` or repeated
`--input NAME=JSON`; artifact references use `--artifacts FILE` or repeated `--artifact NAME=ID` /
`--artifact 'NAME=["ID"]'`. The literal `-` reads a JSON object or uploaded bytes from stdin.

`--wait` prints final state. `--watch` prints events and final state; with `--json` this is a JSON
Lines stream followed by a final `{run,operationId}` object. Interrupting either stops observation
only. `runs wait --timeout 5m` similarly does not alter engine state. Exit codes: 0 success, 1
command/transport error, 2 invalid pipeline, 3 observed failed/cancelled run, 130 interruption.

Event journals are scoped by endpoint, engine, principal and run identity. Each event and cursor is
atomically persisted before display. Up to 1000 recent events (bounded to 8 MiB) are cached; the
engine retains authoritative history. Use `runs watch --from-start` or `--cursor ID` to select a
different replay position.

`runs resolve RUN_ID INSTANCE_ID` requires `--outcome succeeded|not_started|failed` and
`--evidence TEXT`; verified JSON outputs use `--outputs FILE` or `--output`. The engine decides
whether the evidence permits continuation. `resume` is a separate request and cannot bypass an
uncertain external operation.

Artifact downloads verify both size and SHA-256 before publishing any bytes. Files are created
atomically and are never overwritten unless `--force` is explicit. `--output -` emits exact binary
bytes and cannot be combined with `--json`.

Run `knotra serve --help` for infrastructure options. The service requires at least one trusted
`--profile FILE` and a PostgreSQL URL through `KNOTRA_DATABASE_URL` or `--database-url`. River is
the execution backend. Engine logs go to stderr.

`knotra serve` starts the embedded River queues. For an isolated test database, start PostgreSQL
with `docker compose up -d --wait postgres`. `--role` / `KNOTRA_ROLE` defaults to `all`; `api` serves HTTP/SSE,
`scheduler` advances runs and `executor` performs work. Scheduler and executor roles are headless.
Split roles require S3 storage or an explicit shared data directory. `--execution-workers` bounds
simultaneous River leaf deliveries per executor (default 16); existing profile concurrency and
budgets still apply. The queue timeout covers current
profile limits and the original durations of active admitted runs, with a one-minute margin.

`--host-id` (`KNOTRA_HOST_ID`, default `local`) identifies the execution host across restarts. Use a
stable ASCII name containing letters, digits, dots, underscores or hyphens, up to 128 characters,
starting with a letter or digit. Explicit host names have separate sandbox staging directories.
`--shared-data-dir` (`KNOTRA_SHARED_DATA_DIR`) places `artifacts/` and `outcomes/` in a separate
directory while `--data-dir` retains local staging. Without that flag, the existing storage layout
is unchanged. Back up the shared directory together with PostgreSQL. The filesystem must support
hard links, atomic same-directory rename, file/directory syncing and consistent reads between
clients. See
[deployment and recovery](../../docs/running.md#data-recovery-and-deployment-boundaries).

S3-compatible storage uses `--s3-bucket`, `--s3-prefix`, `--s3-region`, `--s3-endpoint` and
`--s3-path-style`, with corresponding `KNOTRA_S3_BUCKET`, `KNOTRA_S3_PREFIX`, `KNOTRA_S3_REGION`,
`KNOTRA_S3_ENDPOINT` and `KNOTRA_S3_PATH_STYLE` environment variables. Omit the endpoint for AWS S3;
set it for another provider. Use path-style addressing when required by the provider. Credentials
use the standard AWS credential chain, including environment credentials and workload identity.
Every process must use the same database, bucket and prefix. S3 and `--shared-data-dir` are mutually
exclusive. With S3, the data directory contains local staging; durable files are stored remotely.
See [multiple pods or servers](../../docs/running.md#multiple-pods-or-servers).
