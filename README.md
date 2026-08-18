# plex-exporter

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/plex-exporter/badges/size.json)](https://github.com/cplieger/plex-exporter/pkgs/container/plex-exporter)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/plex-exporter/badges/coverage.json)](https://github.com/cplieger/plex-exporter/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/plex-exporter/badges/mutation.json)](https://github.com/cplieger/plex-exporter/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13216/badge)](https://www.bestpractices.dev/projects/13216)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/plex-exporter/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/plex-exporter)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/plex-exporter/releases)

See what your Plex server is doing in Grafana: sessions, libraries, bandwidth, transcoding.

## What it does

Connects to your Plex Media Server and exposes metrics (active sessions, library sizes, bandwidth, transcoding status) in a format that Prometheus can scrape and Grafana can visualize.

**Key metrics exposed:**

- Library duration, storage, and item counts (movies, episodes, tracks)
- Active session details (user, device, resolution, stream type)
- Transcode type detection (video/audio/both) and subtitle handling
- Session bandwidth and location (LAN/WAN)
- Host CPU and memory utilization (Plex Pass)
- Bandwidth transmission totals (Plex Pass)
- HTTP polling reachability
- Active transcode session count

### Why this design

- **Polling `/status/sessions` for real-time session tracking:** polls the Plex sessions API every 5s, so new sessions appear within seconds; the tracker prunes sessions after 60s of inactivity
- **Single binary:** direct dependencies are `prometheus/client_golang` and a few small helper libraries (see [Dependencies](#dependencies)); everything else is the standard library
- **Distroless and rootless:** runs on `gcr.io/distroless/static-debian13` as UID 65532 with no shell or package manager, minimizing attack surface
- **Prometheus-native:** exposes a standard `/metrics` endpoint that works with any Prometheus-compatible scraper and any Grafana dashboard, no custom visualization layer

### Limitations

- **Plex Pass features degrade gracefully.** CPU/memory utilization
  and bandwidth statistics require Plex Pass. Without it, those
  metrics are simply absent; every other metric still works.
- **Library item counts are cached.** Episode, track, and item
  counts are refreshed every 15 minutes to avoid hammering the
  Plex API. Counts may lag slightly after large library scans.

## Quick start

Available from both `ghcr.io/cplieger/plex-exporter` and `docker.io/cplieger/plex-exporter`; identical images and tags.

```yaml
services:
  plex-exporter:
    image: ghcr.io/cplieger/plex-exporter:latest
    container_name: plex-exporter
    restart: unless-stopped

    environment:
      PLEX_URL: "http://plex:32400"  # full URL including scheme and port
      PLEX_TOKEN: "your-plex-token"  # admin token from Plex Web settings

    ports:
      - "9594:9594"
```

## Configuration reference

### Environment variables

| Variable | Description | Default | Required |
| --- | --- | --- | --- |
| `PLEX_URL` | Full URL of your Plex Media Server including scheme and port (e.g. `http://192.0.2.100:32400`) | _none_ | Yes |
| `PLEX_TOKEN` | Plex authentication token for the server administrator. Get it from Plex Web → Settings → XML view → myPlexAccessToken | _none_ | Yes |
| `LISTEN_ADDR` | Address and port for the metrics HTTP server | `:9594` | No |
| `PLEX_CA_CERT_PATH` | Path to a PEM file with the CA that signed your Plex server's certificate; TLS verification stays on, pinned to that CA. See [TLS / certificate setup](#tls--certificate-setup). | _(unset)_ | No |

### TLS / certificate setup

Pick the configuration that matches your Plex server:

| Your `PLEX_URL` looks like | What to do |
| --- | --- |
| `http://plex:32400` (Docker network, LAN, etc.) | nothing; TLS isn't in use |
| `https://<hash>.plex.direct:32400` (Plex's official cert) | nothing; Let's Encrypt is trusted by default |
| `https://192.0.2.100:32400` or `https://plex.local` (self-signed / private CA) | set `PLEX_CA_CERT_PATH` to the PEM file of the CA that signed your Plex cert |

### Ports

| Port | Description |
| --- | --- |
| `9594` | Prometheus metrics endpoint (`/metrics`) and health check (`/api/health`) |

## Metrics reference

### HTTP Endpoints

| Endpoint | Method | Description |
| --- | --- | --- |
| `/metrics` | GET | Prometheus metrics (see below) |
| `/api/health` | GET | Returns `{"status":"OK"}` when ready, 503 when starting/stopping |

### Server Metrics

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `plex_server_info` | Gauge (always 1) | `server`, `server_id`, `version`, `platform`, `platform_version`, `plex_pass` | Server metadata and Plex Pass status |
| `plex_host_cpu_utilization_ratio` | Gauge | `server`, `server_id` | Host CPU utilization as a ratio (0.0–1.0). Requires Plex Pass. |
| `plex_host_memory_utilization_ratio` | Gauge | `server`, `server_id` | Host memory utilization as a ratio (0.0–1.0). Requires Plex Pass. |
| `plex_transmit_bytes_total` | Counter | `server`, `server_id` | Cumulative bytes transmitted (from Plex bandwidth API). Requires Plex Pass. Resets on container restart; indicative only. |
| `plex_active_transcode_sessions` | Gauge | `server`, `server_id` | Number of active video transcode sessions (from root endpoint, no Plex Pass needed) |
| `plex_http_reachable` | Gauge | `server`, `server_id` | HTTP polling reachability: `1` = last refresh succeeded, `0` = failed |
| `plex_session_poll_reachable` | Gauge | `server`, `server_id` | Session poll reachability: `1` = last `/status/sessions` poll succeeded, `0` = failed |
| `plex_http_retries_total` | Counter | `server`, `server_id` | Total HTTP retries performed by the Plex client's retry round-tripper across all requests |
| `plex_exporter_errors_total` | Counter | `server`, `server_id`, `type` | Exporter error count by type. Types: `refresh`, `sessions_fetch`, `metadata_fetch`, `invalid_rating_key`, `metrics_server`, `library_items`. |

### Library Metrics

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `plex_library_duration_milliseconds` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id` | Total duration of all items in the library (ms) |
| `plex_library_storage_bytes` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id` | Total storage used by the library (bytes) |
| `plex_library_items` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id`, `content_type` | Number of items in the library. `content_type` is `movies`, `episodes`, `tracks`, `photos`, or `items`. Refreshed every 15 minutes. |

### Session Metrics

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `plex_plays_active` | Gauge | `server`, `server_id`, `library`, `library_id`, `library_type`, `media_type`, `title`, `child_title`, `grandchild_title`, `grandchild_index`, `stream_type`, `stream_resolution`, `stream_file_resolution`, `device`, `device_type`, `user`, `session`, `transcode_type`, `subtitle_action`, `location`, `local` | Currently active play sessions (1 per session). Use `count(plex_plays_active)` for total stream count. Removed after 60s of inactivity. |
| `plex_play_seconds_total` | Counter | _(same as above)_ | Cumulative play time for the session (seconds) |
| `plex_session_bandwidth_kbps` | Gauge | `server`, `server_id`, `session`, `user`, `location` | Real-time session bandwidth from the Plex Sessions API (kbps) |
| `plex_session_bitrate_kbps` | Gauge | `server`, `server_id`, `session`, `user`, `location` | Live stream bitrate per session (kbps). Kept as its own series rather than a label on the play metrics, so adaptive-streaming bitrate changes cannot inflate label cardinality. |

### Session Label Reference

| Label | Values | Description |
| --- | --- | --- |
| `stream_type` | `directplay`, `copy`, `transcode` | How the stream is being delivered |
| `transcode_type` | `none`, `video`, `audio`, `both` | What is being transcoded |
| `subtitle_action` | `none`, `burn`, `copy`, `transcode` | How subtitles are handled |
| `location` | `lan`, `wan` | Client network location |
| `local` | `true`, `false` | Whether the client is on the local network |
| `media_type` | `movie`, `episode`, `track`, etc. | Plex media type |

For episodes: `title` = show name, `child_title` = season,
`grandchild_title` = episode title, `grandchild_index` = episode
number (track number for music). For movies: `title` = movie
name, others are empty.

Beyond the values above, every user-controlled label value is
normalized to a bounded set, so an unexpected Plex response can never
explode Prometheus cardinality: a value outside the documented set
becomes `other` and missing data becomes `unknown`. This covers
`stream_type`, `media_type`, `location`, `subtitle_action`, and the
resolution labels. An empty Plex `subtitleDecision` is reported as
`subtitle_action="none"`.

## Alerting

plex-exporter exposes Prometheus metrics on `/metrics` (see
[Metrics reference](#metrics-reference)). Scrape that endpoint and
evaluate the rules in [`alerts.yaml`](alerts.yaml) with Prometheus or
the Mimir ruler; firing alerts deliver through your Alertmanager like
any other Prometheus alert. They cover:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `PlexAPIUnreachable` | the authenticated Plex API poll reports `plex_http_reachable=0` for 10m (often a revoked or invalid `PLEX_TOKEN`) | warning |
| `PlexExporterCollectionErrors` | the exporter logs collection errors of some `type` continuously for 30m | warning |
| `PlexLibraryItemsCollapsed` | a library's item count drops more than 50% versus its level ~1-2h earlier and stays down for 30m | warning |

Thresholds, the `for:` windows, and the `severity` labels are starting points;
add your scrape `job` label to the selectors if you run more than one instance,
and route by whatever labels your Alertmanager uses.

## Healthcheck

The image ships a `HEALTHCHECK` (the CLI probe `/plex-exporter health`) that verifies the HTTP server is listening; `/api/health` serves the same status over HTTP. The container exits (and Docker restarts it) only on a non-recoverable startup error: a bad token or other 4xx (except 408 and 429), the wrong server (404), a TLS/certificate misconfiguration, or a metrics-server start failure. A transient startup failure (DNS, dial, timeout, a 408 or 429, or a 5xx from a Plex that is still starting up) instead brings the exporter up degraded but healthy: it binds `/metrics`, reports `plex_http_reachable=0`, and recovers automatically once Plex is reachable again.

## Security

Connects outbound to Plex only. The `/metrics` endpoint serves
read-only Prometheus data (standard for internal exporters).
`PLEX_TOKEN` is never logged or exposed in metrics.

The Plex client sends the token in a request header, refuses
redirects, restricts requests to the configured server, bounds
response reads, and caps each request at 30s. TLS verification
always stays on; `PLEX_CA_CERT_PATH` pins a private CA instead of
disabling it. Rating keys are validated as integers before URL
construction, and the metrics server sets explicit read, write,
and header timeouts. The image runs as a non-root user on a
distroless base with no shell or package manager.

Static analysis and vulnerability scans run in CI on every change;
current results are on the repository's Security tab. One accepted
finding: semgrep reports two informational matches, both reviewed
as false positives.

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency | Source |
| --- | --- |
| golang | [Go](https://hub.docker.com/_/golang) |
| gcr.io/distroless/static | [Distroless](https://github.com/GoogleContainerTools/distroless) |
| github.com/prometheus/client_golang | [GitHub](https://github.com/prometheus/client_golang) |
| github.com/prometheus/client_model | [GitHub](https://github.com/prometheus/client_golang) |
| github.com/cplieger/plexapi/v2 | [GitHub](https://github.com/cplieger/plexapi) |
| github.com/cplieger/webhttp/v2 | [GitHub](https://github.com/cplieger/webhttp) |
| github.com/cplieger/health | [GitHub](https://github.com/cplieger/health) |
| github.com/cplieger/envx/v2 | [GitHub](https://github.com/cplieger/envx) |
| github.com/cplieger/slogx | [GitHub](https://github.com/cplieger/slogx) |
| golang.org/x/sync | [golang.org/x](https://pkg.go.dev/golang.org/x/sync) |
| pgregory.net/rapid | [pkg.go.dev](https://pkg.go.dev/pgregory.net/rapid) |

## Credits

This is an original tool building on the Grafana Hackathon 2022 [prometheus-plex-exporter](https://github.com/jsclayton/prometheus-plex-exporter) lineage: the [@jsclayton](https://github.com/jsclayton) post-hackathon fork and the actively maintained [@timothystewart6](https://github.com/timothystewart6) fork. It also uses the [Plex Media Server API](https://developer.plex.tv/pms/) and [prometheus/client_golang](https://github.com/prometheus/client_golang).

## Contributing

Issues and pull requests are welcome. Please open an issue first for
larger changes so the approach can be discussed before implementation.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
