# Persistence

`Store` owns PostgreSQL transactions, durable command receipts, external operation journals,
budgets, requests and read projections. `Artifacts` keeps immutable bytes in S3-compatible storage
or the engine data directory, with metadata in PostgreSQL. A runtime artifact becomes visible when
its producing node commits validated outputs; uploaded inputs are visible immediately.

PostgreSQL owns authoritative execution records and River queue delivery. Execution transitions and
their client projections commit together; bounded projections never replace complete execution
values. Clients do not schedule from these projections.

## Initial schema

`schema.sql` is the complete application schema for a new database. `Store.Open` creates it once
under a transaction advisory lock; concurrent opens share the same engine identity, and reopening
preserves existing data. There is no application migration history or schema-upgrade runner. The
project has not launched, so change this schema directly. Existing databases require a stopped,
offline migration before adopting a changed schema. River initializes and manages its own queue
tables through its library.

## Queries

`queries/*.sql` contains named sqlc queries. `sqlc.yaml` reads `schema.sql` and generates the pgx/v5
methods in `db/`. Store and scheduler code pass their existing pool, connection or transaction to
`db.New`; transaction ownership, savepoints and locking remain in the calling Go code. Only
executing schema DDL stays outside generated methods. River maintains its own schema through its
migration library.

Run `make generate-sql` after editing queries or the initial schema. The Makefile pins sqlc
**1.31.1**; generated Go files are checked in, so ordinary builds need no generator.
`make check-sql` regenerates and checks for a diff, and CI runs it before Go checks. An installed
copy of the pinned version can be used with `make generate-sql SQLC=sqlc`.

River's resource ledger records ownership before sandbox creation or HTTP MCP initialization. MCP
cleanup identities contain only a frozen connection name, session ID and protocol version; resolved
credentials stay out of the ledger. Late initialization evidence must match the original owner and
cannot replace an existing identity. Live run-scoped resources remain protected after their creating
attempt ends. The executor first saves immutable initialization evidence in durable outcome storage,
then updates PostgreSQL; recovery can import a missing database identity after checking the entire
owner. Include this evidence in database/outcome backups. A lost response before
evidence publication leaves an unresolved resource intent, not a record of successful remote
cleanup.

For owned HTTP MCP execution, the same store also keeps one atomically replaced current-call
record per resource. S3 replacements use ETag preconditions so a delayed write cannot replace a newer
call record. It contains the original session/owner and JSON-RPC request ID, without call
arguments or credentials, and is saved durably before physical admission/dispatch. A confirmed reply marks
it complete. After process loss, cleanup notifies cancellation of an unconfirmed call before
deleting the recorded session. Failed notifications/deletions remain retryable. Keep current-call
records with initialization evidence in the backup; remote cancellation requires server cooperation.
Evidence reads accept the caller's context and remote requests have bounded deadlines. HTTP MCP
cleanup can run on another executor using the frozen profile and original ownership evidence;
sandbox cleanup remains local to its recorded host.

`serve --s3-bucket BUCKET` stores artifacts and outcomes in a configurable S3-compatible endpoint;
all processes share the bucket and prefix, with the engine ID appended to object keys. Immutable
objects use conditional creation: identical writers reuse checked bytes and conflicting bytes fail.
Providers must support `If-None-Match` creation, `If-Match` replacement and strongly consistent reads. Storage binding
rejects mismatched durable backends; it does not migrate existing filesystem data to S3.

Filesystem deployments can use `serve --shared-data-dir DIR`; local sandbox staging stays in
`--data-dir`. Artifact content addresses use immutable hard links, with validation of existing bytes
and file types. Confined root initialization syncs newly created parent entries. Shared filesystems
need hard links, atomic rename, durable sync and cross-client read consistency; operators must ensure
every host's mount accesses the same storage. See
[deployment](../../docs/running.md#multiple-pods-or-servers).

## Checks and recovery

Set `KNOTRA_TEST_DATABASE_URL` to enable database tests. Each test creates and removes only its own
schema:

```sh
KNOTRA_TEST_DATABASE_URL='postgres://localhost/knotra_dev?sslmode=disable' \
  go test -mod=readonly -race -count=1 ./internal/store ./internal/api
```

River recovery requires PostgreSQL plus complete immutable artifact/outcome storage, including
uncommitted envelopes and MCP evidence. SQLite in the desktop app is an independent client store.
See the [River backup procedure](../../docs/running.md#river-backup-and-restore) and
[verification](../../docs/verification.md) for live recovery checks.
