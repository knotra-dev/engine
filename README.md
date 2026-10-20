# Knotra Engine

Go execution engine and CLI for YAML AI workflows. Knotra compiles pipeline packages, runs their
steps and keeps execution history, artifacts and pending human requests across restarts.

- Typed inputs, artifacts and nine node types: `llm`, `agent`, `code`, `tool`, `switch`, `human`,
  `foreach`, `loop` and `pipeline`.
- Ollama, OpenAI Responses, Anthropic Messages and MCP through Streamable HTTP or isolated stdio.
- Docker sandboxes with explicit tool and secret grants, network restrictions and run budgets.
- Versioned HTTP/SSE API for submitting runs, inspecting execution and answering human requests.

PostgreSQL stores execution state; River delivers work to engine workers. Multiple execution hosts
can share the database and durable artifact/outcome storage. See [architecture](docs/architecture.md)
for scheduling, persistence and recovery boundaries.

[Desktop client](https://github.com/knotra-dev/desktop) ·
[User documentation](https://knotra-dev.github.io/docs/) ·
[Development workspace](https://github.com/knotra-dev/dev)

## Development

Use Go from `go.mod`, Make and Docker with Compose:

```sh
make install
make infra
make dev
```

`make infra` starts PostgreSQL and prepares sandbox helpers and images. `make dev` builds the CLI
and serves the local engine at `http://127.0.0.1:8787`. Set `KNOTRA_DATABASE_URL` for an existing
database or custom Compose credentials/port. `make stop` stops PostgreSQL while preserving its data.

Local model examples additionally need a running Ollama with this model downloaded separately:

```sh
ollama pull qwen3.5:9b
```

Run `make` or `make help` to list development, test, lint, format and build commands.
[Running](docs/running.md) covers source configuration, CLI archives, cloud models and deployment.

## Documentation

| Task                 | Guide                                                                                |
| -------------------- | ------------------------------------------------------------------------------------ |
| Run a workflow       | [First run](docs/first-run.md), [starter workflows](examples/starter/README.md)      |
| Operate the engine   | [Running](docs/running.md), [CLI reference](internal/cli/README.md)                  |
| Write pipelines      | [Notation](docs/notation.md), [JSON Schema](schemas/knotra-v1.schema.json)           |
| Integrate a client   | [HTTP API](docs/api/desktop-v1.md)                                                   |
| Understand execution | [Architecture](docs/architecture.md), [verification](docs/verification.md)           |
| Contribute           | [Contributing](CONTRIBUTING.md), [roadmap](docs/roadmap.md), [security](SECURITY.md) |

## Contributing

Run `make check` before submitting changes. Default checks need no model weights or paid keys;
integration checks are opt-in. See [CONTRIBUTING](CONTRIBUTING.md) for setup and compatibility
rules.

Authenticated multi-user approval, RBAC and retention policies remain further work.

[MIT](LICENSE) · [Code of conduct](CODE_OF_CONDUCT.md)
