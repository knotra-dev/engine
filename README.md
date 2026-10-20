# Knotra Engine

Go engine and CLI for YAML AI workflows: models, tools, Docker sandboxes and human review, with durable execution history and artifacts.

[Wiki](https://github.com/knotra-dev/engine/wiki) · [Getting started](https://github.com/knotra-dev/engine/wiki/Getting-started) · [Examples](https://github.com/knotra-dev/engine/wiki/Examples) · [Architecture](https://github.com/knotra-dev/engine/wiki/Architecture)

## Run locally

Use Go from `go.mod`, Make, Docker with Compose, and Ollama. Start Docker and Ollama first:

```sh
make install
make infra
ollama pull qwen3.5:9b
make dev
```

Engine: `http://127.0.0.1:8787`. [Deployment](https://github.com/knotra-dev/engine/wiki/Deployment) covers cloud models, workers, storage and backups. Run `make help` for commands, `make check` before contributing.

[Desktop](https://github.com/knotra-dev/desktop) · [Development workspace](https://github.com/knotra-dev/dev) · [Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [MIT](LICENSE) · [Code of conduct](CODE_OF_CONDUCT.md)
