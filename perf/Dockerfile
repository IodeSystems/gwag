# perf/Dockerfile — bundles every gateway under comparison plus the
# bench/cmd/traffic load generator and the perf/cmd/compare
# orchestrator. Built on ubuntu:latest per the README's spec.
#
# Multi-stage so each toolchain layer caches independently:
#   1. builder-go     — Go toolchain, builds gwag + hello-* + traffic + compare
#   2. builder-node   — Node toolchain, hosts graphql-mesh + its npm deps
#   3. fetch-apollo   — downloads apollo-router binary
#   4. runtime        — ubuntu:latest + binaries from above stages

# ---------------------------------------------------------------------
# Stage 1 — Go binaries (gwag, hello-*, traffic, compare)
# ---------------------------------------------------------------------
FROM ubuntu:latest AS builder-go

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# Pin Go version to match the host build; mismatched toolchains
# trigger a `go.mod`-side `toolchain` download.
ARG GO_VERSION=1.26.2
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
      | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:$PATH

WORKDIR /src

# Stage the fork into a known location and rewrite go.mod's
# host-absolute replace path to point inside the container. The
# fork is staged into perf/.build/graphql by perf/docker-build.sh
# before `docker build` is invoked.
COPY perf/.build/graphql /opt/graphql-fork

COPY go.mod go.sum ./
RUN go mod edit -replace github.com/graphql-go/graphql=/opt/graphql-fork \
    && go mod download

COPY . /src
# Re-apply the rewrite after the full COPY (go.mod from the host
# clobbered the edited file).
RUN go mod edit -replace github.com/graphql-go/graphql=/opt/graphql-fork
RUN mkdir -p /out/bin && \
    go build -o /out/bin/gateway        ./examples/multi/cmd/gateway && \
    go build -o /out/bin/hello-proto    ./examples/multi/cmd/hello-proto && \
    go build -o /out/bin/hello-openapi  ./examples/multi/cmd/hello-openapi && \
    go build -o /out/bin/hello-graphql  ./examples/multi/cmd/hello-graphql && \
    go build -o /out/bin/traffic        ./bench/cmd/traffic && \
    go build -o /out/bin/compare        ./perf/cmd/compare

# ---------------------------------------------------------------------
# Stage 2 — Node + graphql-mesh
# ---------------------------------------------------------------------
FROM ubuntu:latest AS builder-node

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /mesh
COPY perf/configs/mesh/package.json perf/configs/mesh/.meshrc.yaml ./
RUN npm install --no-audit --no-fund

# ---------------------------------------------------------------------
# Stage 3 — Apollo Router binary
# ---------------------------------------------------------------------
FROM ubuntu:latest AS fetch-apollo

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ARG APOLLO_VERSION=1.55.0
RUN curl -fsSL \
      "https://github.com/apollographql/router/releases/download/v${APOLLO_VERSION}/router-v${APOLLO_VERSION}-x86_64-unknown-linux-gnu.tar.gz" \
      | tar -xz -C /tmp \
    && mkdir -p /out \
    && mv /tmp/dist/router /out/router

# ---------------------------------------------------------------------
# Stage 4 — runtime
# ---------------------------------------------------------------------
FROM ubuntu:latest

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl bash procps \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /perf

# Repo layout (configs + scripts; binaries land under bench/.run/bin/
# and perf/.run/bin/ to match what the orchestrator expects).
COPY perf/             /perf/perf/
COPY bench/perf-scenarios.yaml /perf/bench/perf-scenarios.yaml

# Go-built artifacts.
RUN mkdir -p /perf/bench/.run/bin /perf/perf/.run/bin
COPY --from=builder-go /out/bin/gateway        /perf/bench/.run/bin/gateway
COPY --from=builder-go /out/bin/hello-proto    /perf/bench/.run/bin/hello-proto
COPY --from=builder-go /out/bin/hello-openapi  /perf/bench/.run/bin/hello-openapi
COPY --from=builder-go /out/bin/hello-graphql  /perf/bench/.run/bin/hello-graphql
COPY --from=builder-go /out/bin/traffic        /perf/bench/.run/bin/traffic
COPY --from=builder-go /out/bin/compare        /perf/perf/.run/bin/compare

# graphql-mesh + npm dependency tree.
COPY --from=builder-node /mesh /perf/perf/configs/mesh

# Apollo Router.
COPY --from=fetch-apollo /out/router /perf/perf/.run/bin/router

# Expose ports for any caller that wants to attach a profiler / curl.
EXPOSE 18080 50090 14000 14100 50053 50054 50055

# Output mount-point: docker run -v $(pwd)/out:/out will get the
# rendered comparison.md and per-gateway sweep JSONs.
VOLUME ["/out"]

# Run the comparator. Override CMD to drop into bash for debugging:
#   docker run --rm -it --entrypoint bash gwag-perf
CMD ["bash", "-c", "/perf/perf/run.sh && cp -r /perf/perf/.out/* /out/"]
