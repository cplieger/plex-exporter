# check=error=true
FROM golang:1.26-alpine@sha256:7a3e50096189ad57c9f9f865e7e4aa8585ed1585248513dc5cda498e2f41812c AS builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /plex-exporter .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240

COPY --chmod=755 --from=builder /plex-exporter /plex-exporter
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=15s \
    CMD ["/plex-exporter", "health"]
ENTRYPOINT ["/plex-exporter"]
