# Knotra architecture

A Pipeline defines dependencies, data contracts, checks and approvals. Agents choose actions within
its granted tools and limits. Every run freezes the package and configuration; a new run can still
produce different model output.

The engine supports multiple hosts sharing PostgreSQL and durable artifact/outcome storage. API,
scheduler and executor are separate runtime roles; CLI and desktop are HTTP/SSE clients. The stack
is Go, River and Docker, with Tauri/Rust and React for desktop.

## Execution

| Component              | Responsibility                                                              |
| ---------------------- | --------------------------------------------------------------------------- |
| Contract compiler      | Strict YAML, JSON Schema, CEL, references, dependency graph and permissions |
| API                    | Admission, immutable definitions, commands, requests, events and artifacts  |
| Scheduler              | Bounded decisions over persisted graph state, waits, deadlines and budgets  |
| River runtime          | Transactional job delivery, attempt ownership, publication and maintenance  |
| Executors and adapters | Model calls, agent loops, MCP and Docker sandboxes                          |

1. The client submits an explicit package manifest, inputs and profile reference.
2. The engine validates the package and actual resources, then freezes the execution plan.
3. Admission commits the run and River wakeup in the same PostgreSQL transaction.
4. Executors claim attempts, journal external calls and save validated results before publication.
5. State transitions, projections and subsequent deliveries commit together; clients observe them.

Scheduling lives in `internal/scheduler`, delivery in `internal/queue`, and leaf execution in
`internal/executor` and `internal/adapters`. External I/O stays outside scheduler decisions and CEL.
The agent runtime owns its model/tool conversation; human review is a separate `human` node.

Independent nodes run concurrently within profile and ancestor budgets. Model text can stream to
observers, while downstream nodes receive complete validated outputs. JSON/file validation checks
structure; substantive quality needs explicit code, agent or human checks. Automatic agent context
truncation is not implemented.

See [notation](notation.md) for the authoritative execution and permission contracts.

## Storage

| Data                                                                                 | Storage                                      |
| ------------------------------------------------------------------------------------ | -------------------------------------------- |
| Immutable definitions/plans, execution state, budgets, requests and command receipts | PostgreSQL                                   |
| River jobs, projections and event history                                            | The same PostgreSQL database                 |
| Artifact bytes, outcome envelopes and MCP recovery evidence                          | Shared `artifacts/` and `outcomes/`          |
| Sandbox staging and legacy Temporal payloads                                         | Host-local data directory                    |
| Desktop drafts, receipts and cursors                                                 | SQLite; browser preview uses browser storage |

Client caches and bounded previews never replace authoritative execution records. Artifact bytes are
immutable and become visible after validated publication. Input uploads are visible immediately.
Shared filesystems must support hard links, atomic rename, durable sync and consistent reads across
clients. Recovery needs PostgreSQL and the complete durable directories together; see
[backup and restore](running.md#river-backup-and-restore).

## Recovery

Queue redelivery alone does not authorize repeating an external action. Completed calls and nodes
reuse saved results. An attempt saves its outcome before database publication so recovery can finish
an interrupted commit.

An unconfirmed external effect requires verification or explicit `resolve`. Arbitrary external APIs
have no exactly-once guarantee. A lost agent workspace or MCP session cannot be silently recreated
as a continuation. Cancellation stops admission and interrupts supported active work while keeping
confirmed results. Deadlines and spent budgets survive recovery.

Saved execution formats must remain compatible across updates; automatic conversion between
incompatible versions or backends is not promised. [Execution semantics](notation/execution.md)
define retries, unknown outcomes and cancellation; [the API](api/desktop-v1.md) defines command
receipts and reconciliation.

## Isolation and access

Docker sandboxes have explicit image, resource, network and secret grants. Nodes use separate
workspaces and exchange only declared values/artifacts. Model credentials stay in trusted adapters;
passing secrets into a sandbox requires a separate grant. MCP supports Streamable HTTP and isolated
stdio, with permission and argument checks on every call. A discovered tool schema does not make the
remote service trustworthy or immutable.

The API defaults to loopback. Remote access requires TLS and a bearer token for one trusted
principal; independent-user RBAC is not available. Docker host filtering uses pinned IP addresses,
without HTTP Host/SNI checks. See [adapter guarantees](../internal/adapters/README.md) and
[SECURITY](../SECURITY.md).

## Observation and legacy execution

SSE and paginated history expose state and bounded model/tool diagnostics. Detailed observations can
have explicitly marked gaps; operation journals retain authoritative outcomes. Provider-hidden
reasoning is excluded. JSON logs and optional OTLP traces/metrics are described in
[running](running.md#observability-and-operational-limits). Monetary estimates are not implemented.

River is the default backend. Retained live Temporal runs or unsent legacy commands still start the
legacy worker; completed legacy projections alone do not require it. Existing runs keep their
admitted backend. [Temporal](temporal.md) documents its additional history and payload requirements.
