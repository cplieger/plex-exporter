# check=error=true
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:376f4a381b112a7cfef541ecee0263ece432119fbbdad8d75f2f51fc197287f4 AS builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /plex-exporter .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240

COPY --chmod=755 --from=builder /plex-exporter /plex-exporter
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=15s \
    CMD ["/plex-exporter", "health"]
ENTRYPOINT ["/plex-exporter"]
