# gwag gateway image. Build from the repo root:
#   docker build -t gwag:dev .
# `gwag deploy up` runs this for you.
#
# Stage 1 runs the repo's own `bin/build --no-go` (pnpm install, GraphQL
# codegen via the gateway's --gen mode, Vite build, lint), so it needs
# both Node and Go. Stage 2 builds the binary with the UI embedded.
# The runtime image is distroless, non-root (uid 65532), no shell.

FROM golang:1.26-bookworm AS ui
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
 && rm -rf /var/lib/apt/lists/*
ARG NODE_VERSION=22.14.0
RUN curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-$(dpkg --print-architecture | sed 's/amd64/x64/').tar.xz" \
  | tar -xJ -C /usr/local --strip-components=1 \
 && npm install -g pnpm@11.0.8
WORKDIR /src
COPY . .
RUN bin/build --no-go

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/ui/dist ./ui/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gwag ./gw/cmd/gwag \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gwag /gwag
# Named volumes copy this directory's ownership on first use.
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME /data
EXPOSE 8080 50090 14222 14248
USER 65532:65532
ENTRYPOINT ["/gwag"]
CMD ["up", "--data-dir", "/data", "--no-context-write"]
