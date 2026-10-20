# Roadmap

Knotra implements all nine v1 node types, River/PostgreSQL execution across multiple hosts, model
and MCP adapters, Docker sandboxes, CLI and desktop. See [README](../README.md) for capabilities and
[verification](verification.md) for checks.

Further directions have no assigned schedule:

| Direction                   | Work                                                                        |
| --------------------------- | --------------------------------------------------------------------------- |
| Long-running runs on update | Saved execution format versioning and compatibility checks                  |
| New run from selected place | Explicit reuse of results while preserving the original run                 |
| Operations                  | Event/file retention, storage quotas and broader backup/failure acceptance  |
| Multiple users              | Authentication, RBAC, quotas and enhanced isolation                         |
| Desktop delivery            | Signed builds, notarization and verification on supported operating systems |
| Quality assessment          | Domain examples, model/prompt comparisons, quality and cost measurement     |

Contract changes update schema, diagnostics, fixtures, runtime and documentation together. Recovery,
idempotency and unknown outcomes require checks beyond a successful execution. Development and
compatibility rules live in [CONTRIBUTING](../CONTRIBUTING.md).
