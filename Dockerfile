FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=readonly -trimpath \
      -ldflags "-X main.version=$VERSION" -o /out/knotra ./cmd/knotra && \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -mod=readonly -trimpath \
      -o /out/sandbox-helper ./internal/adapters/sandboxhelper

FROM alpine:3.23
RUN apk add --no-cache ca-certificates docker-cli
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/knotra-dev/engine" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION
COPY --from=build /out/knotra /usr/local/bin/knotra
COPY --from=build /out/sandbox-helper /usr/local/libexec/knotra/sandbox-helper
COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/knotra-entrypoint
COPY examples/local/profile.yaml /etc/knotra/profile.yaml
COPY LICENSE /usr/share/licenses/knotra/LICENSE
ENV DOCKER_HOST=unix:///var/run/docker.sock \
    KNOTRA_DATA_DIR=/var/lib/knotra
WORKDIR /workspace
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/knotra-entrypoint"]
CMD ["--help"]
