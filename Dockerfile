# syntax=docker/dockerfile:1
#
# Backyard container image: one static Go binary on distroless, running as an
# unprivileged user.
#
# Build from source (cross-compiles natively, no QEMU needed for multi-arch):
#
#   docker build -t backyard .
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     --build-arg VERSION=0.1.0 --build-arg COMMIT="$(git rev-parse HEAD)" \
#     --build-arg DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" -t ghcr.io/thasami/backyard:dev .
#
# Build arguments: GO_IMAGE / RUNTIME_IMAGE (base images, e.g. from an internal
# mirror), GOPROXY (an internal Go module proxy), VERSION / COMMIT / DATE
# (stamped into `backyard version`).
#
# The `release` target packages a binary built outside Docker: GoReleaser
# (dockers_v2) places it at <os>/<arch>/backyard in the build context.
#
# Runtime contract:
#   * runs as uid/gid 65532 ("nonroot") and listens on :8080 (or :$PORT);
#   * /var/lib/backyard is the data volume (fs storage, sqlite database) and
#     the working directory, so `serve --dev` keeps its data there too;
#   * configuration: mount a file and pass `--config` (or set BACKYARD_CONFIG),
#     put the YAML in BACKYARD_CONFIG_YAML, or rely on the defaults (fs storage,
#     sqlite, no auth) plus BACKYARD_BASE_DOMAIN, BACKYARD_PUBLIC_URL,
#     BACKYARD_SECRET, DATABASE_URL and PORT;
#   * no HEALTHCHECK: distroless has no shell or curl. Point the orchestrator's
#     HTTP probes at GET /__backyard/healthz (liveness) and
#     GET /__backyard/readyz (readiness; pings the database and storage).

ARG GO_IMAGE=golang:1.26
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

# ---------------------------------------------------------------------------
# build: compile on the build platform, cross-compiling for the target.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
WORKDIR /src
ARG GOPROXY
ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=readonly \
    GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION
ARG COMMIT
ARG DATE
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=. \
    set -eu; \
    pkg=github.com/thasami/backyard/internal/version; \
    ldflags="-s -w"; \
    if [ -n "${VERSION:-}" ]; then ldflags="$ldflags -X $pkg.Version=${VERSION#v}"; fi; \
    if [ -n "${COMMIT:-}" ]; then ldflags="$ldflags -X $pkg.Commit=$COMMIT"; fi; \
    if [ -n "${DATE:-}" ]; then ldflags="$ldflags -X $pkg.Date=$DATE"; fi; \
    if [ "$TARGETARCH" = "arm" ] && [ -n "${TARGETVARIANT:-}" ]; then export GOARM="${TARGETVARIANT#v}"; fi; \
    GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -trimpath -buildvcs=false -ldflags "$ldflags" -o /out/backyard ./cmd/backyard

# ---------------------------------------------------------------------------
# runtime: shared settings of the published images.
FROM ${RUNTIME_IMAGE} AS runtime
LABEL org.opencontainers.image.title="Backyard" \
      org.opencontainers.image.description="Drop a folder of HTML, get an internal URL - with a zero-config database, realtime, files, AI and warehouse API for every site." \
      org.opencontainers.image.source="https://github.com/thasami/backyard" \
      org.opencontainers.image.url="https://github.com/thasami/backyard" \
      org.opencontainers.image.documentation="https://github.com/thasami/backyard/tree/main/docs" \
      org.opencontainers.image.licenses="Apache-2.0"
USER 65532:65532
# BuildKit creates the working directory owned by USER, so a fresh named
# volume mounted here is writable by the server.
WORKDIR /var/lib/backyard
VOLUME ["/var/lib/backyard"]
EXPOSE 8080
ENTRYPOINT ["/backyard"]
CMD ["serve"]

# ---------------------------------------------------------------------------
# release: GoReleaser images (prebuilt binary from the build context).
FROM runtime AS release
ARG TARGETPLATFORM
COPY --chmod=0755 ${TARGETPLATFORM}/backyard /backyard

# ---------------------------------------------------------------------------
# backyard (default): image built from source.
FROM runtime AS backyard
ARG VERSION
ARG COMMIT
LABEL org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
COPY --from=build /out/backyard /backyard
