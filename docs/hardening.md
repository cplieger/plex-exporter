# Security

This page covers the limits plex-exporter enforces, how to run it with a read-only root filesystem, and what the image contains. It is for readers who harden their deployment or review what they run.

## What the exporter limits

The exporter connects out to the configured Plex server only, and its `/metrics` endpoint serves read-only Prometheus data.

- The Plex client sends the token in a request header, refuses redirects and sends requests only to the configured server.
- It caps how much of each response it reads. Each request has a deadline, 30 seconds for a session poll and 45 seconds for a refresh cycle.
- TLS verification always stays on. `PLEX_CA_CERT_PATH` pins a private CA instead of turning verification off.
- Rating keys from Plex are checked to be integers before the exporter builds a URL from them.
- The metrics server sets a 5-second header timeout, a 5-second read timeout and a 10-second write timeout.
- `PLEX_TOKEN` is never logged and never appears in a metric. `PLEX_TOKEN_FILE` reads it from a [secret file](https://github.com/cplieger/docs/blob/main/docs/hardening.md#secrets-in-files) instead, so `docker inspect` does not show it.

Current code-scanning and vulnerability results are on the repository's Security tab.

## Read-only root filesystem

Add these lines to the `plex-exporter` service to run it with a read-only root filesystem, no Linux capabilities and no privilege escalation. The exporter writes no file and keeps its health verdict in memory. So it needs no `tmpfs` or volume, and `/healthz` never depends on a writable folder.

```yaml
    read_only: true
    cap_drop: [ALL]
    security_opt:
      - no-new-privileges:true
```

On Kubernetes, `securityContext.readOnlyRootFilesystem: true` likewise needs no volume.

## What the image contains

The image is one static Go binary on `gcr.io/distroless/static-debian13:nonroot`. It runs as the non-root user 65532, with no shell or package manager. It also carries the license text of every bundled component under `/usr/share/licenses/`.

[Renovate](https://github.com/renovatebot/renovate) updates every dependency of the build, and each is pinned by digest or version. `golang` is the build image only, and `pgregory.net/rapid` is used only by the tests.

| Dependency | Source |
| --- | --- |
| golang | [Go](https://hub.docker.com/_/golang) |
| gcr.io/distroless/static | [Distroless](https://github.com/GoogleContainerTools/distroless) |
| github.com/cplieger/plexapi/v3 | [GitHub](https://github.com/cplieger/plexapi) |
| github.com/cplieger/go/httpserver | [GitHub](https://github.com/cplieger/go/tree/main/httpserver) |
| github.com/cplieger/go/health/v2 | [GitHub](https://github.com/cplieger/go/tree/main/health) |
| github.com/cplieger/go/measure | [GitHub](https://github.com/cplieger/go/tree/main/measure) |
| github.com/cplieger/go/envx | [GitHub](https://github.com/cplieger/go/tree/main/envx) |
| github.com/cplieger/go/slogx | [GitHub](https://github.com/cplieger/go/tree/main/slogx) |
| github.com/cplieger/go/textsafe | [GitHub](https://github.com/cplieger/go/tree/main/textsafe) |
| golang.org/x/sync | [golang.org/x](https://pkg.go.dev/golang.org/x/sync) |
| pgregory.net/rapid | [pkg.go.dev](https://pkg.go.dev/pgregory.net/rapid) |
