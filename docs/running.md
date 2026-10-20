# Running the engine and CLI

Knotra connects to PostgreSQL, model APIs and Docker. River runs inside the engine. Quickstart and
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
`--host-id` on each execution host. `--shared-data-dir` contains `artifacts/` and `outcomes/`;
`--data-dir` keeps local staging. Shared filesystems need hard links, atomic same-directory rename,
durable file/directory sync and consistent reads. Workers need compatible profiles, helpers and
images. [EngineProfile](notation/engine-profile.md) defines resources and limits.

Preserve PostgreSQL with the complete artifact/outcome directories, including unpublished envelopes
and MCP recovery evidence. A database dump alone is insufficient. Storage files and Knotra history
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
database dump and file archives finish. After a crash, confirm all writers have exited before
backing up the surviving database and files. An HTTP outage alone does not prove that writers have
stopped. This procedure preserves pending evidence without needing a distributed snapshot protocol.

Record the exact Knotra release/binary, PostgreSQL major version, engine ID (`GET /v1/info` before
stopping), host ID, data paths, profile/credential references, helper and image versions with the
backup. Restore with that Knotra release and execution format; an older binary is not a rollback
procedure. Preserve credentials/TLS/configuration through the deployment's existing secure backup
mechanism. Use PostgreSQL client tools matching the server major version.

With writers stopped, set these paths and libpq connection settings for the existing database:

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
