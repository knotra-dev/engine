# Running the engine and CLI

Knotra connects to PostgreSQL, model APIs and Docker, with S3-compatible storage for distributed
workers. River runs inside the engine. Quickstart and
the bundled Compose configuration prepare development infrastructure; production needs managed
storage, backups and access configuration.

For the engine and desktop together, use [knotra-dev/dev](https://github.com/knotra-dev/dev). Build
from source below until the first organization release is published.

## Quickstart from a CLI archive

Install Docker Engine, Docker Compose and Ollama. Unpack the
[CLI archive](https://github.com/knotra-dev/engine/releases), keeping both Linux helpers next to the
binary:

```sh
./knotra doctor
./knotra quickstart --dir "$PWD/knotra-data"
```

Doctor checks dependencies without starting infrastructure. Quickstart starts persistent PostgreSQL,
loads images/model, extracts editable examples, produces `greeting.txt` and serves until Ctrl-C. Use
the same directory, provider, model and ports on restart; existing settings and edits are kept.
Services and volumes survive Ctrl-C. The directory must be shared with Docker/Colima; keep it with
the PostgreSQL volume. Without `--dir`, the system configuration directory is used and printed.

On Windows use Docker Desktop with Linux containers and WSL 2, a local NTFS data directory, and
`.\knotra.exe` in PowerShell. Docker uses the current context's local named pipe; Ollama must be
reachable at `http://127.0.0.1:11434`.

Useful flags:

- `--no-run`: start without generating the greeting.
- `--cors-origin http://127.0.0.1:1420`: allow browser preview.
- `--postgres-port` and `--listen`: select free ports for another quickstart directory.
- `--provider openai --model YOUR_MODEL` or `--provider anthropic --model YOUR_MODEL`: use a cloud
  model. Set `OPENAI_API_KEY` or `ANTHROPIC_API_KEY` in the environment; keys are not written into
  profiles. In PowerShell use `$env:OPENAI_API_KEY = 'YOUR_KEY'`. Greeting generation is a paid
  request unless `--no-run` is set.

## Build and start from source

Use Go from `go.mod`, Make and Docker with Compose:

```sh
make install
make infra
make dev
```

`make infra` starts PostgreSQL, builds the sandbox helper and firewall image, and pulls the Python
and Node images used by the local profile. `make dev` builds the CLI and starts it with
`examples/local/profile.yaml`; it does not download or start a model. For local model examples,
start Ollama and download the configured model separately:

```sh
ollama pull qwen3.5:9b
```

`make helper` detects the Docker architecture; set `HELPER_ARCH=amd64` or `arm64` if detection is
unavailable. The helper is written to `.knotra/bin/sandbox-helper`. Pipeline executables must exist
in their configured images. Model/MCP credentials come from the engine environment through profile
secret references.

`make dev` defaults `KNOTRA_DATABASE_URL` to the bundled development database on port `25432`.
Set it explicitly for another database or custom Compose credentials/port. To use existing
PostgreSQL, skip `make infra` and prepare sandbox helpers and images separately if needed.
The engine creates its tables at startup. Docker uses the current context or
`DOCKER_HOST` / `--docker-host` with a local Unix socket or Windows named pipe; remote TCP Docker
endpoints are unsupported.

Compose starts PostgreSQL 18.6 on loopback port `25432`. `KNOTRA_PG_PORT` overrides the port;
`KNOTRA_DEV_DB_PASSWORD` changes the initial password. `make stop` preserves the database and local
`.knotra` data; `make infra` starts it again. `docker compose down` keeps named volumes, while
`docker compose down --volumes` deletes the database. Use `make help` for the full command list.

## OpenAI and Anthropic connections

Replace the `model_main` connection inside `EngineProfile.spec`, keeping sandboxes and limits:

```yaml
secrets:
  model_key: { env: OPENAI_API_KEY }
models:
  model_main:
    provider: openai
    model: YOUR_MODEL
    auth: { key: { secretRef: model_key } }
    parameters: { max_output_tokens: 4096 }
```

For Anthropic use `ANTHROPIC_API_KEY`, `provider: anthropic` and `max_tokens`. Default API bases are
`https://api.openai.com/v1` and `https://api.anthropic.com/v1`. Models must support function
calling; capabilities and accepted parameters depend on the provider/model. Cloud adapters reject
`imageInput`. See [adapter parameters and guarantees](../internal/adapters/README.md).

## First run

In another terminal from the repository root:

```sh
bin/knotra validate examples/starter/hello/pipeline.yaml --remote --profile local
bin/knotra run examples/starter/hello/pipeline.yaml --profile local --wait
bin/knotra runs list
bin/knotra artifacts list --run RUN_ID
bin/knotra artifacts download ARTIFACT_ID --output greeting.txt
```

Downloads verify size and SHA-256; replacing an existing file requires `--force`. Authoritative
resource checks happen with `--remote` and at admission. For human requests:

```sh
bin/knotra requests list --run RUN_ID --all --json
bin/knotra requests respond REQUEST_ID --outputs response.json
bin/knotra runs watch RUN_ID
```

After transport loss, inspect `operations list` and reconcile with `operations retry OPERATION_ID`,
which keeps the original command key. An uncertain external effect needs `runs resolve`; `resume`
cannot bypass it. Completed operations are restored from journals, and a lost unfinished agent
workspace is not automatically re-executed.

[First run](first-run.md) walks through the game and release review;
[starter workflows](../examples/starter/README.md) and [CLI reference](../internal/cli/README.md)
cover other commands.

## Connecting desktop and browser preview

The engine listens on `http://127.0.0.1:8787` by default. Start
[desktop](https://github.com/knotra-dev/desktop), save the address in Settings and connect. Browser
preview additionally needs `--cors-origin http://127.0.0.1:1420`, exactly matching its origin;
native desktop does not need CORS. Closing a client stops observation without cancelling execution.

Remote access requires `KNOTRA_TOKEN`, `--tls-cert` and `--tls-key`; remote CLI uses HTTPS and the
same token. One bearer token represents one trusted principal; independent-user RBAC is not
implemented. `--cors-origin` permits one browser origin. Keep credentials out of exported packages.

## Data, recovery, and deployment boundaries

Multiple hosts share PostgreSQL and durable artifact/outcome storage. Set a stable, distinct
`--host-id` on each execution host. Workers need compatible profiles, helpers and images.
[EngineProfile](notation/engine-profile.md) defines resources and limits.

### Multiple pods or servers

Use one PostgreSQL database and one S3-compatible bucket with a common prefix. The endpoint is
configurable, so operators can use AWS S3 or their own compatible service. Start one API, one
scheduler and several executors; roles can be scaled independently:

| Role | Work | HTTP listener |
| --- | --- | --- |
| `all` (default) | API, scheduling and execution | Yes |
| `api` | Admission, commands, inspection and artifact access | Yes |
| `scheduler` | Persisted graph decisions, timers and recovery | No |
| `executor` | Model/tool calls and sandbox execution | No |

Configure these values in every process, using the deployment's secret mechanism for credentials:

```sh
export KNOTRA_DATABASE_URL='postgres://USER:PASSWORD@DATABASE/knotra?sslmode=require'
export KNOTRA_S3_BUCKET='knotra'
export KNOTRA_S3_PREFIX='production'
export KNOTRA_S3_REGION='us-east-1'
export KNOTRA_S3_ENDPOINT='https://YOUR-S3-ENDPOINT'
export KNOTRA_S3_PATH_STYLE='true'
# AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN, or workload identity.
```

Omit `KNOTRA_S3_ENDPOINT` for AWS S3. Set `KNOTRA_S3_PATH_STYLE` only when the provider requires
path-style bucket addressing. The corresponding CLI flags are `--s3-bucket`, `--s3-prefix`,
`--s3-region`, `--s3-endpoint` and `--s3-path-style`. Credentials use the AWS SDK's standard
credential chain and are never flags or profile fields. Each role needs object read and write
permissions, including metadata reads, under the configured prefix. Knotra adds the database's
engine ID to isolate its objects; the bucket must already exist.

Run each command on its intended pod/server, with its own local directory:

```sh
bin/knotra serve --role api --data-dir /var/lib/knotra/api --profile /etc/knotra/profile.yaml
bin/knotra serve --role scheduler --data-dir /var/lib/knotra/scheduler --profile /etc/knotra/profile.yaml
bin/knotra serve --role executor --host-id worker-1 --data-dir /var/lib/knotra/worker --profile /etc/knotra/profile.yaml
```

Give every concurrent executor a distinct host ID, stable across restarts when preserving its
local resources. For Kubernetes, a StatefulSet pod name is one suitable identity. A second executor
cannot hold the same host ID concurrently. API and scheduler processes can share a host ID.
`--execution-workers` limits concurrent deliveries per executor; profile limits and budgets still
apply across the database. `--role all` can also run on multiple hosts when using shared storage.

All roles need the trusted profile files and the same database and S3 settings. The S3 service must
provide strongly consistent reads, atomic conditional creation with `If-None-Match: *`, and
conditional replacement with `If-Match` and object ETags. These preconditions prevent conflicting
immutable writes and delayed updates from overwriting newer MCP recovery evidence; compatibility
with basic uploads alone is insufficient. Knotra rejects conflicting immutable bytes.
Do not expire or rewrite objects through bucket lifecycle rules while their execution history is
retained. Protect object access and transport alongside database access: outputs and recovery
evidence can contain user data.

Executors use local Docker for sandbox nodes. API admission also needs Docker and the helper when
resolving sandbox images. Kubernetes worker pods therefore need a supported local Docker setup;
a native Kubernetes sandbox backend is not implemented. Model-only pipelines do not need Docker.
A workflow using any run-scoped MCP session pins all its leaves to the first owning worker process,
including unrelated leaves and human waits. Concurrency within that worker still applies, but the
run cannot spread across workers. Losing that process leaves unfinished work requiring explicit
resolution; restarting on the same host cannot recreate its session as a continuation. Graceful
shutdown waits for pinned runs within the shutdown deadline.

Send SIGINT or SIGTERM for graceful shutdown before removing a worker. A confirmed result is saved
in object storage before its database publication, allowing another process to recover it after a
crash. Arbitrary external side effects still require provider idempotency or reconciliation.

For a shared filesystem deployment, use `--shared-data-dir` instead of S3 configuration. It contains
`artifacts/` and `outcomes/`; `--data-dir` keeps local staging. The filesystem needs hard links,
atomic same-directory rename, durable file/directory sync and consistent reads. Split roles require
explicit shared storage. The default single-process layout remains entirely local.

Use a fresh database for a new deployment. Existing installations need a stopped, offline schema
and storage migration before adopting this execution format; automatic upgrade and file-to-S3
migration are not provided. Do not reset a database to bypass a format error. Storage binding
prevents accidentally starting an existing engine against a different durable backend or prefix.

Preserve PostgreSQL with the complete artifact/outcome objects, including unpublished envelopes
and MCP recovery evidence. A database dump alone is insufficient. Storage objects and Knotra history
have no automatic cleanup; operators manage free space and backups. River job retention does not
authorize deleting history or durable files. Individual artifacts are limited to 64 MiB.

On Windows, directory flushing is unavailable; directory-entry persistence after sudden power loss
depends on NTFS. File contents are flushed before publication on every OS.

Sandbox containers have independent deadlines and a watchdog that stops them about 45 seconds after
engine heartbeats stop. Docker `allowlist` filters pinned IPs without inspecting HTTP Host/SNI;
shared addresses/proxies share access. See [adapter guarantees](../internal/adapters/README.md).

### River backup and restore

The supported procedure uses a maintenance stop on all engine hosts. Stop admission and all
API/scheduler/executor processes, wait for process exit, and keep all writers stopped until the
database dump and storage backup finish. After a crash, confirm all writers have exited before
backing up the surviving database and objects. An HTTP outage alone does not prove that writers have
stopped. This procedure preserves pending evidence without needing a distributed snapshot protocol.

Record the exact Knotra release/binary, PostgreSQL major version, engine ID (`GET /v1/info` before
stopping), host IDs, data paths, S3 endpoint/bucket/prefix, profile/credential references, and helper
and image versions with the backup. Restore with that Knotra release and execution format; an older binary is not a rollback
procedure. Preserve credentials/TLS/configuration through the deployment's existing secure backup
mechanism. Use PostgreSQL client tools matching the server major version.

For S3, keep writers stopped while dumping PostgreSQL and backing up every object under the
configured prefix and engine ID, including unpublished outcomes and MCP evidence. Use the storage
provider's backup mechanism and verify both copies before marking the backup complete. Restore
the complete object set and database before restarting any role, using the original storage
configuration. Bucket versioning alone does not coordinate a database snapshot.

The following archive commands apply to filesystem storage. With writers stopped, set these paths
and libpq connection settings for the existing database:

```sh
set -eu
umask 077
backup_dir=/absolute/path/to/new-backup
local_dir=/absolute/path/to/engine-data
shared_dir=/absolute/path/to/shared-data
# Configure PGHOST, PGPORT, PGDATABASE, PGUSER and a protected PGPASSFILE.
mkdir -m 700 "$backup_dir"
pg_dump --format=custom --no-owner --no-acl > "$backup_dir/database.dump"
tar -cf "$backup_dir/local.tar" -C "$local_dir" .
if [ "$shared_dir" != "$local_dir" ]; then
  tar -cf "$backup_dir/shared.tar" -C "$shared_dir" .
fi
```

Archive the whole application database, including River tables, migration metadata, command receipts
and ownership records. The commands above intentionally omit database ownership and grants:
provision the same application role and database access separately. Mark/copy a backup as complete
only after every command succeeds, and retain the matching release/configuration inventory with it.
Archive each host's local data and the shared durable directories. Do not restart writers midway
through making the archives.

Restore into an empty database created from `template0` and fresh empty directories, with the engine
still stopped. Set libpq settings to the restoration target, provision its application role, and run
as that role:

```sh
set -eu
umask 077
pg_restore --no-owner --no-acl --exit-on-error --single-transaction \
  --dbname="$PGDATABASE" "$backup_dir/database.dump"
mkdir -m 700 "$local_dir"
tar -xpf "$backup_dir/local.tar" -C "$local_dir"
if [ "$shared_dir" != "$local_dir" ]; then
  mkdir -m 700 "$shared_dir"
  tar -xpf "$backup_dir/shared.tar" -C "$shared_dir"
fi
```

Keep the engine stopped if either database or file restoration fails. Do not merge an archive into
an active/newer engine. Preserve file contents, permissions and modification times. Start the
matching binary with the restored database and data directories, original host IDs, and compatible
profiles/helpers/images. Check the original engine ID, waiting request IDs/deadlines, downloaded
artifact bytes and SSE history. Lease reconciliation imports saved results idempotently; missing
external evidence requires explicit outcome resolution and never justifies automatic repetition.

The PostgreSQL commands follow its documented
[custom dump](https://www.postgresql.org/docs/18/backup-dump.html) and
[transactional restore](https://www.postgresql.org/docs/18/app-pgrestore.html) procedures. The
integration check `TestRiverBackupRestoresHumanAndUncommittedOutcome` exercises actual `pg_dump`,
`tar`, schema/data loss, `pg_restore`, restored publication, CLI human responses, exact SSE history
and a second engine restart. It covers a stopped single-host River engine; online snapshots, network
filesystem faults and multi-host recovery need separate acceptance.

## Observability and operational limits

JSON logs go to stderr; `--debug` includes successful execution boundaries. OTLP/HTTP export is
opt-in through `OTEL_EXPORTER_OTLP_ENDPOINT` or separate `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` and
`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`. `OTEL_SDK_DISABLED=true` disables it.

| Metric prefix `knotra.scheduler.`          | Meaning                                                |
| ------------------------------------------ | ------------------------------------------------------ |
| `dirty_run.age`, `ready_job.age`           | Oldest pending run wake and due delivery, seconds      |
| `steps`                                    | Scheduler calls, including rolled-back work            |
| `lease.expired`                            | Claimed attempts with expired ownership                |
| `outcome.pending`, `outcome.pending.age`   | Unimported outcome files and oldest age                |
| `outcome.unknown`                          | Instances waiting for resolution                       |
| `slot.reservations`                        | Unreleased scope reservations                          |
| `timer.lag`                                | Oldest due timer for a live run, seconds               |
| `publication.failures`, `cleanup.failures` | Failed publication and cleanup work, including retries |

Metric collection has a one-second database deadline; storage failures fail collection instead of
reporting healthy zeros. Pending scans scale with unimported outcome keys. Traces identify
deliveries, run/instance/attempt ownership and operation boundaries. Logs/traces exclude prompts,
credentials, provider reasoning, payloads, endpoint URLs and raw error text. Stored outputs and tool
results can still contain sensitive user data. Detailed client observations have explicit
truncation/gap markers; see [API history](api/desktop-v1.md#execution-observations-and-history).
