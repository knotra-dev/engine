# Contributing to Knotra

Knotra's YAML contract lives in [`docs/notation`](docs/notation/v1.md); the schema and prose
together define behavior. Start by reading the relevant contract and the existing tests before
changing execution semantics. A specification change must update its schema, diagnostics, fixtures,
runtime behavior and documentation together. New syntax must not silently change already persisted
runs.

## Development environment

Use the Go version in `go.mod`. The [running guide](docs/running.md) describes local Docker/Colima,
PostgreSQL and Ollama setup. River runs inside the engine. For fast development:

```sh
go mod download
make build
make check
```

Use `make help` to list commands. `make format` formats Go sources, `make test` runs unit tests,
`make test-race` enables the race detector, and `make build` writes the CLI to `bin/`. `make vet`
and `make format-check` are available separately.

`make integration` runs enabled database, Docker and service checks. For the real River disaster
restore check, also set `KNOTRA_TEST_RECOVERY=1`, `KNOTRA_TEST_HELPER`, and
`KNOTRA_TEST_POSTGRES_CONTAINER` to the PostgreSQL container serving `KNOTRA_TEST_DATABASE_URL`. It
uses that container's `pg_dump`/`pg_restore` and removes only its own temporary schema and data
directories. The manually triggered Go integration job enables it alongside the process recovery
checks.

`make check` runs strict golangci-lint checks, formatting checks, `go vet`, unit tests and the race
detector. Unit tests may open loopback listeners. They require no paid model calls, installed
Ollama, PostgreSQL or Docker daemon; integration tests skip unless explicitly enabled.

Run `make lint` for lint checks alone. The Makefile pins golangci-lint **2.14.0**; no global
installation is required. An installed copy of that version can be used with
`make lint GOLANGCI_LINT=golangci-lint`. Both Linux and Windows CI apply `.golangci.yml` to all Go
packages, including tests. The configuration checks error handling, contexts, HTTP/SQL resource
closure, error wrapping and readability. Only generated code is excluded; there are no blanket test
or directory exemptions. A necessary `//nolint:linter` must name the check and explain why it does
not apply; unused suppressions fail lint.

PostgreSQL queries live in `internal/store/queries`; regenerate their typed pgx methods with
`make generate-sql` after changing queries or `internal/store/schema.sql`. Commit the generated
`internal/store/db` files too. CI checks generation with the sqlc version pinned in the Makefile.

## Package boundaries

| Package                                    | Responsibility                                                                           |
| ------------------------------------------ | ---------------------------------------------------------------------------------------- |
| `internal/contract`                        | Package loading, strict parsing, semantic validation, CEL and immutable plan compilation |
| `internal/execution`, `internal/scheduler` | Shared execution types and pure bounded graph decisions                                  |
| `internal/queue`, `internal/executor`      | River delivery, transactional execution lifecycle and adapter hosting                    |
| `internal/adapters`                        | Model, MCP and isolated command execution behind injected durable hooks                  |
| `internal/store`                           | PostgreSQL transactions, projections, durable operations and artifact metadata           |
| `internal/api`, `internal/protocol`        | Versioned HTTP/SSE boundary and transport DTOs                                           |
| `internal/client`, `internal/cli`          | Client mutation journals, event observation and command presentation                     |
| `internal/app`                             | Dependency wiring and service lifecycle                                                  |

Keep external I/O out of scheduler decisions and expression evaluation. Pass `context.Context`
through blocking work, bound reads and concurrency, preserve typed errors, and make cleanup
ownership clear. Configuration and credentials are engine concerns; provider responses and pipeline
content are untrusted data. Avoid global mutable state and broad interfaces added for hypothetical
future use.

Use ordinary Go conventions: `gofmt`, small cohesive functions, explicit errors, table-driven tests
where they clarify behavior, and comments explaining invariants or non-obvious decisions. Tests
should demonstrate contract behavior or prevent a specific failure, not mirror implementation line
by line. Do not log secrets or provider-hidden reasoning. Never test arbitrary pipeline commands on
the host.

## Readability and client development

Review every touched source file, test and configuration for logical blocks, clear names and bounded
responsibilities. Wrap argument lists and object literals when that makes their structure easier to
follow. Keep SQL, prompts, embedded programs and fixture bytes intact unless the change explicitly
targets them. Generated files, assets and dependency locks are not hand formatted. Negative notation
fixtures may rely on their exact malformed syntax.

Use `gofmt` for Go, the root Prettier configuration for TypeScript/CSS/JSON/YAML and Markdown,
rustfmt for Rust, and Black 25.1.0 for Python. Avoid mixing behavior changes into mechanical edits
without explaining and testing them.

Desktop development and translations live in
[knotra-dev/desktop](https://github.com/knotra-dev/desktop). Use
[knotra-dev/dev](https://github.com/knotra-dev/dev) for paired changes and literal browser/native
acceptance. Its lock file selects the tested engine and desktop commits; `make integration` runs
`TestDesktopLiteralClients` with `KNOTRA_TEST_DESKTOP_DIR` set to the desktop checkout.

Engine CI checks Go, SQL generation, Windows builds, Python examples and contract fixtures. The
manually triggered Go workflow adds PostgreSQL, Docker, process recovery and backup/restore.

## Integration tests

Start development infrastructure and build sandbox dependencies:

```sh
docker compose up -d --wait
make helper firewall
docker pull python:3.13-alpine
export KNOTRA_TEST_DATABASE_URL='postgres://knotra:knotra-development@127.0.0.1:25432/knotra?sslmode=disable'
export KNOTRA_TEST_HELPER="$PWD/.knotra/bin/sandbox-helper"
export KNOTRA_TEST_WORKDIR="$PWD/.knotra/test-work"
export KNOTRA_TEST_FIREWALL_IMAGE=knotra-firewall:dev
make integration
```

Use a development database. Database tests create isolated schemas; Docker tests remove their own
containers. No blanket database reset or Docker prune is needed. The manually triggered Go workflow
runs these persistence, HTTP, sandbox and MCP checks without downloading a model.

To include real local model calls, install `qwen3.5:9b` in Ollama and use the same PostgreSQL and
Docker setup:

```sh
docker compose up -d --wait
export KNOTRA_TEST_OLLAMA=http://127.0.0.1:11434
export KNOTRA_TEST_REAL=1
go test -mod=readonly -race -count=1 -timeout=30m ./internal/adapters/... ./internal/integration
```

These tests intentionally run actual models and isolated processes. They are separate from default
CI so a contributor does not need model weights to submit a change. Report the exact integration
command and environment used in a pull request.

## Pull requests and compatibility

Keep changes focused. Explain the trigger or problem, resulting behavior, and checks performed.
Include a reproducible example for a bug and a regression test when appropriate. Keep schema errors,
runtime failures and uncertain external outcomes distinct; do not turn an ambiguous write into an
automatic retry.

New adapters must validate capabilities before admission, fix resolved resources in the plan, count
every outbound call, and document retry/idempotency behavior. The project has not launched: edit the
initial application schema directly; schema upgrades for existing deployments are outside the
current scope. Workflow changes must preserve persisted execution state and already recorded
decisions across restarts.

Knotra is MIT licensed. Contributions are made under the same [license](LICENSE). Follow the
[community conduct](CODE_OF_CONDUCT.md) and report vulnerabilities privately according to
[SECURITY](SECURITY.md).

## Cloud adapters and release bundles

Default adapter tests use literal Responses/Messages fixtures, including fragmented SSE, malformed
responses, auth/status handling, tool ID correlation, budgets and operation replay. Real Docker
agent checks use `KNOTRA_TEST_HELPER` and `KNOTRA_TEST_WORKDIR`. The recovery suite includes both
fixture providers against River/PostgreSQL when `KNOTRA_TEST_RECOVERY=1`; the generation count must
remain one across process restart. On Unix, `KNOTRA_TEST_RUNTIME_RECOVERY=1` also enables actual
River Runtime SIGKILL checks against literal HTTP fixtures and isolated PostgreSQL schemas, with
natural lease expiry. Run
`go test -race -count=1 -timeout=6m -run '^TestRuntimeProcessCrashBoundaries$' ./internal/queue`
with `KNOTRA_TEST_DATABASE_URL` set.

Paid smoke tests require explicit `KNOTRA_TEST_CLOUD=1`, a provider key and
`KNOTRA_TEST_OPENAI_MODEL` or `KNOTRA_TEST_ANTHROPIC_MODEL`. See the
[adapter guide](internal/adapters/README.md); credentials alone never enable paid tests.

`scripts/package-release.sh VERSION` builds four macOS/Linux `.tar.gz` archives and two Windows
`.zip` archives (amd64 and arm64), each containing both Linux helpers, license and startup
instructions. Packaging needs Go, Bash, tar, zip and shasum and can run on macOS or Linux without
Docker. `CGO_ENABLED=0` allows cross-compiling the Windows CLI on a Mac; it does not let macOS
execute the resulting `.exe`.

The Windows CI workflow runs native Go tests, including HTTP over a local named pipe, and CLI smoke
checks on an amd64 Windows runner. ARM64 Windows is cross-compiled during packaging. The Linux-only
sandbox helper is excluded from native Windows tests and built for Linux for every bundle. The Tauri
desktop is built separately and is not included in CLI bundles.

`SHA256SUMS` covers all six archives. A new `v*` tag runs Go checks on Linux and Windows, builds
these bundles and publishes a GitHub release. Publish new commits/tags; never move an existing
release tag to different source. Smoke-test `doctor` from an extracted archive outside the source
checkout before tagging.
