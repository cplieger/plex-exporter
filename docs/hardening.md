# Security

This page covers the limits plex-exporter enforces, how to run it with a read-only root filesystem, and what the image contains. It is for readers who harden their deployment or review what they run.

## What the exporter limits

The exporter connects out to the configured Plex server only, and its `/metrics` endpoint serves read-only Prometheus data.

- The Plex client sends the token in a request header, refuses redirects and sends requests only to the configured server.
- It caps how much of each response it reads. Each request has a deadline, 30 seconds for a session poll and 45 seconds for a refresh cycle.
- TLS verification always stays on. `PLEX_CA_CERT_PATH` pins a private CA instead of turning verification off.
- Rating keys from Plex are checked to be integers before the exporter builds a URL from them.
- The metrics server sets a 5-second header timeout, a 5-second read timeout and a 10-second write timeout.
- `PLEX_TOKEN` is never logged and never appears in a metric. `PLEX_TOKEN_FILE` also keeps it out of the container environment, so `docker inspect` does not show it.

Current code-scanning and vulnerability results are on the repository's Security tab.

## Read-only root filesystem

The exporter records that it is ready by writing an empty marker file, `/tmp/.healthy`. A read-only root filesystem therefore needs a writable `/tmp`, and a small in-memory mount is enough:

```yaml
    read_only: true
    tmpfs:
      - "/tmp:size=1m,mode=1777,noexec,nosuid,nodev"
```

Without that mount the marker is never written. The image's Docker healthcheck still reports healthy after one warning at start, so a missing mount does not restart a working container. `/api/health`, however, answers 503 for as long as the exporter runs, so any probe on that endpoint fails. [Running on Kubernetes](kubernetes.md#a-writable-tmp) has the Kubernetes form.

## What the image contains

The image is one static Go binary on `gcr.io/distroless/static-debian13:nonroot`. It runs as the non-root user 65532, with no shell or package manager. It also carries the license text of every bundled component under `/usr/share/licenses/`.

[Renovate](https://github.com/renovatebot/renovate) updates every dependency of the build, and each is pinned by digest or version. `golang` is the build image only, and `pgregory.net/rapid` is used only by the tests.

| Dependency | Source |
| --- | --- |
| golang | [Go](https://hub.docker.com/_/golang) |
| gcr.io/distroless/static | [Distroless](https://github.com/GoogleContainerTools/distroless) |
| github.com/prometheus/client_golang | [GitHub](https://github.com/prometheus/client_golang) |
| github.com/prometheus/client_model | [GitHub](https://github.com/prometheus/client_model) |
| github.com/cplieger/plexapi/v2 | [GitHub](https://github.com/cplieger/plexapi) |
| github.com/cplieger/webhttp/v3 | [GitHub](https://github.com/cplieger/webhttp) |
| github.com/cplieger/health | [GitHub](https://github.com/cplieger/health) |
| github.com/cplieger/envx/v2 | [GitHub](https://github.com/cplieger/envx) |
| github.com/cplieger/slogx | [GitHub](https://github.com/cplieger/slogx) |
| github.com/cplieger/runesafe/v2 | [GitHub](https://github.com/cplieger/runesafe) |
| golang.org/x/sync | [golang.org/x](https://pkg.go.dev/golang.org/x/sync) |
| pgregory.net/rapid | [pkg.go.dev](https://pkg.go.dev/pgregory.net/rapid) |
