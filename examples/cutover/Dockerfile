# Image of the cutover example's app and traffic commands. Build from
# the repo root (cutover.sh does):
#   docker build -t gwag-cutover-app -f examples/cutover/Dockerfile .
FROM golang:1.26.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/app ./examples/cutover/cmd/app \
 && CGO_ENABLED=0 go build -trimpath -o /out/traffic ./examples/cutover/cmd/traffic

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
COPY --from=build /out/traffic /traffic
USER 65532:65532
ENTRYPOINT ["/app"]
