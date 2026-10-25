# compare/Dockerfile — bundles every gateway under comparison plus the
# bench/cmd/traffic load generator and the compare/cmd/compare
# orchestrator. Built on ubuntu:26.04 (pinned, so rebuilds stay comparable).
#
# Multi-stage so each toolchain layer caches independently:
#   1. builder-go     — Go toolchain, builds gwag + hello-* + traffic + compare
#   2. builder-node   — Node toolchain, hosts Hive Gateway + graphql-mesh npm deps
#   3. fetch-apollo   — downloads apollo-router binary
#   4. fetch-federation — downloads cosmo-router + grafbase-gateway
#   5. runtime        — ubuntu:26.04 + binaries from above stages

# ---------------------------------------------------------------------
# Stage 1 — Go binaries (gwag, hello-*, traffic, compare)
# ---------------------------------------------------------------------
FROM ubuntu:26.04 AS builder-go

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# Pin Go version to match the host build; mismatched toolchains
# trigger a `go.mod`-side `toolchain` download.
ARG GO_VERSION=1.27.3
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
      | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:$PATH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . /src

# The gateway's `ui/embed.go` uses `//go:embed all:dist` which fails
# if ui/dist/ is empty. .dockerignore strips the host's built bundle
# to keep the context small; stub a placeholder so the embed pattern
# matches. None of the comparison endpoints serve the UI, so
# the stub is sufficient.
RUN mkdir -p /src/ui/dist && echo '<!-- placeholder for go:embed -->' > /src/ui/dist/index.html
RUN mkdir -p /out/bin && \
    go build -o /out/bin/gateway        ./examples/multi/cmd/gateway && \
    go build -o /out/bin/hello-proto    ./examples/multi/cmd/hello-proto && \
    go build -o /out/bin/hello-openapi  ./examples/multi/cmd/hello-openapi && \
    go build -o /out/bin/hello-graphql  ./examples/multi/cmd/hello-graphql && \
    go build -o /out/bin/traffic        ./bench/cmd/traffic && \
    go build -o /out/bin/compare        ./compare/cmd/compare

# ---------------------------------------------------------------------
# Stage 2 — Node + Hive Gateway + graphql-mesh (legacy)
# ---------------------------------------------------------------------
FROM ubuntu:26.04 AS builder-node

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates \
    && curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /mesh
COPY compare/configs/mesh/package.json compare/configs/mesh/.meshrc.yaml ./
RUN npm install --no-audit --no-fund

# Hive Gateway. Compose runs at start time (it reads the live
# backends), so only the npm tree is baked in here.
WORKDIR /hive
COPY compare/configs/hive/package.json compare/configs/hive/mesh.config.ts compare/configs/hive/gateway.config.ts ./
RUN npm install --no-audit --no-fund

# ---------------------------------------------------------------------
# Stage 3 — Apollo Router binary
# ---------------------------------------------------------------------
FROM ubuntu:26.04 AS fetch-apollo

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ARG APOLLO_VERSION=2.19.0
RUN curl -fsSL \
      "https://github.com/apollographql/router/releases/download/v${APOLLO_VERSION}/router-v${APOLLO_VERSION}-x86_64-unknown-linux-gnu.tar.gz" \
      | tar -xz -C /tmp \
    && mkdir -p /out \
    && mv /tmp/dist/router /out/router

# ---------------------------------------------------------------------
# Stage 4 — Cosmo Router + Grafbase Gateway binaries
# ---------------------------------------------------------------------
FROM ubuntu:26.04 AS fetch-federation

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Cosmo's tarball also names its binary `router`; install it as
# cosmo-router so it doesn't collide with Apollo's.
ARG COSMO_VERSION=0.363.0
ARG GRAFBASE_VERSION=0.53.5
RUN mkdir -p /out \
    && curl -fsSL \
      "https://github.com/wundergraph/cosmo/releases/download/router%40${COSMO_VERSION}/router-router%40${COSMO_VERSION}-linux-amd64.tar.gz" \
      | tar -xz -C /tmp router \
    && mv /tmp/router /out/cosmo-router \
    && curl -fsSL -o /out/grafbase-gateway \
      "https://github.com/grafbase/grafbase/releases/download/gateway-${GRAFBASE_VERSION}/grafbase-gateway-x86_64-unknown-linux-musl" \
    && chmod +x /out/cosmo-router /out/grafbase-gateway

# ---------------------------------------------------------------------
# Stage 5 — runtime
# ---------------------------------------------------------------------
FROM ubuntu:26.04

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl bash procps \
    && curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /compare

# Repo layout (configs + scripts; binaries land under bench/.run/bin/
# and compare/.run/bin/ to match what the orchestrator expects).
COPY compare/             /compare/compare/
COPY bench/perf-scenarios.yaml /compare/bench/perf-scenarios.yaml

# hello-proto reads protos/hello.proto from CWD at startup. Mirror
# the host layout (examples/multi/protos/) so the orchestrator's
# cmd.Dir works the same way in-container and on the host.
COPY examples/multi/protos /compare/examples/multi/protos

# Go-built artifacts.
RUN mkdir -p /compare/bench/.run/bin /compare/compare/.run/bin
COPY --from=builder-go /out/bin/gateway        /compare/bench/.run/bin/gateway
COPY --from=builder-go /out/bin/hello-proto    /compare/bench/.run/bin/hello-proto
COPY --from=builder-go /out/bin/hello-openapi  /compare/bench/.run/bin/hello-openapi
COPY --from=builder-go /out/bin/hello-graphql  /compare/bench/.run/bin/hello-graphql
COPY --from=builder-go /out/bin/traffic        /compare/bench/.run/bin/traffic
COPY --from=builder-go /out/bin/compare        /compare/compare/.run/bin/compare

# graphql-mesh + npm dependency tree.
COPY --from=builder-node /mesh /compare/compare/configs/mesh
COPY --from=builder-node /hive /compare/compare/configs/hive

# Apollo Router.
COPY --from=fetch-apollo /out/router /compare/compare/.run/bin/router

# Cosmo Router + Grafbase Gateway.
COPY --from=fetch-federation /out/cosmo-router     /compare/compare/.run/bin/cosmo-router
COPY --from=fetch-federation /out/grafbase-gateway /compare/compare/.run/bin/grafbase-gateway

# Expose ports for any caller that wants to attach a profiler / curl.
EXPOSE 18080 50090 14000 14100 14200 14300 50053 50054 50055

# Output mount-point: docker run -v $(pwd)/out:/out will get the
# rendered comparison.md and per-gateway sweep JSONs.
VOLUME ["/out"]

# Run the comparator. Override CMD to drop into bash for debugging:
#   docker run --rm -it --entrypoint bash gwag-compare
#
# Pass HOST_UID/HOST_GID (e.g. `-e HOST_UID=$(id -u) -e HOST_GID=$(id -g)`)
# to chown the output back to the invoking host user — otherwise the
# container's root identity bleeds into the bind mount and host-side
# `compare/.out/` ends up unwritable on the next host-local run.
CMD ["bash", "-c", "set -e; /compare/compare/run.sh && cp -r /compare/compare/.out/. /out/ && if [ -n \"${HOST_UID:-}\" ]; then chown -R \"${HOST_UID}:${HOST_GID:-${HOST_UID}}\" /out; fi"]
