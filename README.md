# Knotra Engine

Go engine and CLI for YAML AI workflows: models, tools, Docker sandboxes and human review, with durable execution history and artifacts.

[Wiki](https://github.com/knotra-dev/engine/wiki) · [Getting started](https://github.com/knotra-dev/engine/wiki/Getting-started) · [Examples](https://github.com/knotra-dev/engine/wiki/Examples) · [Architecture](https://github.com/knotra-dev/engine/wiki/Architecture)

Prefer the app? [Knotra Desktop](https://github.com/knotra-dev/desktop) includes a first-launch tour, saved model sources and MCP servers, per-block model/tool selection, and an MCP server for assistants to control the live workspace. See [models and environments](https://github.com/knotra-dev/engine/wiki/Resources) and [MCP setup](https://github.com/knotra-dev/engine/wiki/MCP).

## Run with Docker

Use the prebuilt engine and CLI without Go, Make or a source checkout. Install Docker with Compose and start Ollama. This setup uses host networking: Linux Docker Engine supports it; in Docker Desktop 4.34+, enable **Settings → Resources → Network → Enable host networking**. See [Docker's platform requirements](https://docs.docker.com/engine/network/drivers/host/).

```sh
mkdir knotra && cd knotra
curl -fsSLO https://raw.githubusercontent.com/knotra-dev/engine/main/compose.container.yaml
docker compose -f compose.container.yaml --profile tools pull
ollama pull qwen3.5:9b
docker compose -f compose.container.yaml up -d --wait
```

Engine: `http://127.0.0.1:8787`. Create and run your first pipeline from the same directory:

```sh
docker compose -f compose.container.yaml run --rm --user "$(id -u):$(id -g)" cli init my-workflow
docker compose -f compose.container.yaml run --rm --user "$(id -u):$(id -g)" cli validate my-workflow/pipeline.yaml
docker compose -f compose.container.yaml run --rm --user "$(id -u):$(id -g)" cli run my-workflow/pipeline.yaml --profile local --watch
```

The engine image includes the CLI, Linux sandbox helper and Docker CLI; the firewall has its own image. Both `ghcr.io/knotra-dev/engine` and `ghcr.io/knotra-dev/firewall` support Linux `amd64` and `arm64`, with `latest`, `main`, `sha-<full-commit>` and release `v…` tags. To inspect the CLI without starting an engine:

```sh
docker run --rm ghcr.io/knotra-dev/engine:latest --help
```

Compose stores engine files in `.knotra-container` and PostgreSQL in a persistent volume. `docker compose -f compose.container.yaml down` stops the stack and keeps both. The engine controls the local Docker socket, so run it as a trusted operator; workflow code still executes in its separate sandboxes.

[Getting started](https://github.com/knotra-dev/engine/wiki/Getting-started) covers results and native CLI archives. [Deployment](https://github.com/knotra-dev/engine/wiki/Deployment) covers image versions, cloud models, workers, storage and backups.

## Develop from source

Contributors need Go from `go.mod`, Make, Docker with Compose, and Ollama:

```sh
make install
make infra
ollama pull qwen3.5:9b
make dev
```

Run `make help` for commands and `make check` before contributing.

[Desktop](https://github.com/knotra-dev/desktop) · [Development workspace](https://github.com/knotra-dev/dev) · [Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [MIT](LICENSE) · [Code of conduct](CODE_OF_CONDUCT.md)
