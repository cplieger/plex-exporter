# docker-plex-exporter

![License: GPL-3.0](https://img.shields.io/badge/license-GPL--3.0-blue)
[![GitHub release](https://img.shields.io/github/v/release/cplieger/docker-plex-exporter)](https://github.com/cplieger/docker-plex-exporter/releases)
[![Image Size](https://ghcr-badge.egpl.dev/cplieger/plex-exporter/size)](https://github.com/cplieger/docker-plex-exporter/pkgs/container/plex-exporter)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)

Plex Media Server Prometheus exporter with real-time session tracking

## Overview

A ground-up rewrite of the
[prometheus-plex-exporter](https://github.com/jsclayton/prometheus-plex-exporter)
project (originally a Grafana hackathon project), rebuilt for
reliability, minimal dependencies, and distroless deployment.
Connects to Plex over HTTP and WebSocket to collect metrics in
real time and serve them in Prometheus format.

**Example use case:** You run a Plex Media Server and want to
track library sizes, active sessions, transcode load, bandwidth,
and host resource utilization in Grafana. Point this exporter at
your Plex server, scrape `/metrics` with Prometheus or Alloy, and
get dashboards covering everything from per-session transcode
details to WebSocket connection health.

**Key metrics exposed:**
- Library duration, storage, and item counts (movies, episodes, tracks)
- Active session details (user, device, resolution, stream type)
- Transcode type detection (video/audio/both) and subtitle handling
- Session bandwidth and location (LAN/WAN)
- Host CPU and memory utilization (Plex Pass)
- Bandwidth transmission totals (Plex Pass)
- WebSocket connection health
- Active transcode session count

This is a distroless, rootless container running on
`gcr.io/distroless/static` with no shell or package manager.
Only two direct Go dependencies: `coder/websocket` for the Plex
notification stream and `prometheus/client_golang` for metrics.

### Comparison With Upstream

This is a complete rewrite — no code is shared with the upstream
projects. The architecture and dependency choices are fundamentally
different. The comparison below is against the
[timothystewart6](https://github.com/timothystewart6/prometheus-plex-exporter)
fork (the actively maintained upstream):

| | Upstream | This Project |
|---|---|---|
| **Dependencies** | 5 direct (go-plex-client, zap, multierr, prometheus client, prometheus model) | 2 (coder/websocket, prometheus client) |
| **Logging** | uber-go/zap | stdlib `log/slog` (zero dep) |
| **Plex client** | Vendored fork of go-plex-client (~900+ lines in plex.go alone) | Built-in minimal client (~80 lines) |
| **Image user** | root | nonroot (UID 65534) |
| **WebSocket reconnect** | Delegated to go-plex-client (no built-in reconnect) | Automatic with exponential backoff (1s→30s) |
| **Health check** | None | CLI probe (`/plex-exporter health`) + HTTP `/health` |
| **Transcode tracking** | Via vendored client events | Direct WebSocket JSON parsing |
| **Session bandwidth** | Estimated from bitrates only | Real bandwidth from Plex Session API + estimates |
| **Go version** | 1.23 | 1.26 |

Additional metrics not in upstream:
- `plex_websocket_connected` — monitor exporter↔Plex connection
- `plex_active_transcode_sessions` — from root endpoint, no Plex Pass needed
- `plex_session_bandwidth_kbps` — actual bandwidth per session
- `plex_server_info` includes `plex_pass` label
- Play metrics include `location` (lan/wan) and `local` (true/false)

### Limitations

- **Plex Pass features degrade gracefully.** CPU/memory utilization
  and bandwidth statistics require Plex Pass. Without it, those
  metrics are simply absent — the exporter still works for all
  other metrics.
- **WebSocket is required.** The exporter uses the Plex WebSocket
  notification stream for real-time session tracking. If your Plex
  server is behind a reverse proxy, ensure WebSocket connections
  are forwarded correctly.
- **Library item counts are cached.** Episode, track, and item
  counts are refreshed every 15 minutes to avoid hammering the
  Plex API. Counts may lag slightly after large library scans.


## Container Registries

This image is published to both GHCR and Docker Hub:

| Registry | Image |
|----------|-------|
| GHCR | `ghcr.io/cplieger/plex-exporter` |
| Docker Hub | `docker.io/cplieger/plex-exporter` |

```bash
# Pull from GHCR
docker pull ghcr.io/cplieger/plex-exporter:latest

# Pull from Docker Hub
docker pull cplieger/plex-exporter:latest
```

Both registries receive identical images and tags. Use whichever you prefer.

## Quick Start

```yaml
services:
  plex-exporter:
    image: ghcr.io/cplieger/plex-exporter:latest
    container_name: plex-exporter
    restart: unless-stopped
    user: "1000:1000"  # match your host user
    mem_limit: 64m

    environment:
      TZ: "Europe/Paris"
      PLEX_SERVER: "http://plex:32400"  # full URL including scheme and port
      PLEX_TOKEN: "your-plex-token"  # admin token from Plex Web settings

    ports:
      - "9594:9594"

    healthcheck:
      test:
        - CMD
        - /plex-exporter
        - health
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 15s
```

## Deployment

1. Set `PLEX_SERVER` to the full URL of your Plex server
   (e.g. `http://192.0.2.100:32400` or `https://plex.local:32400`).
2. Set `PLEX_TOKEN` to a Plex authentication token belonging to the
   server administrator. See
   [Finding an authentication token](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/).
3. The exporter connects immediately, performs an initial metadata
   refresh, and starts listening for WebSocket events. Metrics are
   available at `/metrics` within seconds.
4. If your Plex server uses a self-signed TLS certificate, set
   `SKIP_TLS_VERIFICATION=true`.
5. For Grafana integration, see the
   [Grafana Dashboard](#grafana-dashboard) section below.


## Environment Variables

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| `TZ` | Container timezone | `Europe/Paris` | No |
| `PLEX_SERVER` | Full URL of your Plex Media Server including scheme and port (e.g. `http://192.0.2.100:32400`) | `http://plex:32400` | Yes |
| `PLEX_TOKEN` | Plex authentication token for the server administrator. Get it from Plex Web → Settings → XML view → myPlexAccessToken | - | Yes |


## Ports

| Port | Description |
|------|-------------|
| `9594` | Prometheus metrics endpoint (`/metrics`) and health check (`/health`) |

## API Reference

### HTTP Endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/metrics` | GET | Prometheus metrics (see below) |
| `/health` | GET | Returns `ok` if the metrics server is running |

The CLI health probe (`/plex-exporter health`) checks for a marker
file and does not require HTTP — it works in distroless containers
with no shell or curl.

### Prometheus Metrics

#### Server Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `plex_server_info` | Gauge (always 1) | `server`, `server_id`, `version`, `platform`, `platform_version`, `plex_pass` | Server metadata and Plex Pass status |
| `plex_host_cpu_utilization_ratio` | Gauge | `server`, `server_id` | Host CPU utilization as a ratio (0.0–1.0). Requires Plex Pass. |
| `plex_host_memory_utilization_ratio` | Gauge | `server`, `server_id` | Host memory utilization as a ratio (0.0–1.0). Requires Plex Pass. |
| `plex_transmit_bytes_total` | Counter | `server`, `server_id` | Cumulative bytes transmitted (from Plex bandwidth API). Requires Plex Pass. Resets on container restart — indicative only. |
| `plex_estimated_transmit_bytes_total` | Counter | `server`, `server_id` | Estimated bytes transmitted based on session bitrates. Resets on container restart — indicative only. |
| `plex_active_transcode_sessions` | Gauge | `server`, `server_id` | Number of active video transcode sessions (from root endpoint, no Plex Pass needed) |
| `plex_websocket_connected` | Gauge | `server`, `server_id` | WebSocket connection status: `1` = connected, `0` = disconnected |

#### Library Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `plex_library_duration_milliseconds` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id` | Total duration of all items in the library (ms) |
| `plex_library_storage_bytes` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id` | Total storage used by the library (bytes) |
| `plex_library_items` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id`, `content_type` | Number of items in the library. `content_type` is `movies`, `episodes`, `tracks`, `photos`, or `items`. Refreshed every 15 minutes. |

#### Session Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `plex_plays_total` | Counter | `server`, `server_id`, `library`, `library_id`, `library_type`, `media_type`, `title`, `child_title`, `grandchild_title`, `stream_type`, `stream_resolution`, `stream_file_resolution`, `stream_bitrate`, `device`, `device_type`, `user`, `session`, `transcode_type`, `subtitle_action`, `location`, `local` | Active play sessions (1 per session). Removed after 60s of inactivity. |
| `plex_play_seconds_total` | Counter | *(same as above)* | Cumulative play time for the session (seconds) |
| `plex_session_bandwidth_kbps` | Gauge | `server`, `server_id`, `session`, `user`, `location` | Real-time session bandwidth from the Plex Sessions API (kbps) |

#### Session Label Reference

| Label | Values | Description |
|---|---|---|
| `stream_type` | `direct play`, `copy`, `transcode` | How the stream is being delivered |
| `transcode_type` | `none`, `video`, `audio`, `both` | What is being transcoded |
| `subtitle_action` | `none`, `burn`, `copy`, `transcode` | How subtitles are handled |
| `location` | `lan`, `wan` | Client network location |
| `local` | `true`, `false` | Whether the client is on the local network |
| `media_type` | `movie`, `episode`, `track`, etc. | Plex media type |

For episodes: `title` = show name, `child_title` = season,
`grandchild_title` = episode title. For movies: `title` = movie
name, others are empty.

## Grafana Integration

A ready-to-import Grafana dashboard is included in the repository.
It works with Prometheus as the datasource — no special plugins
required.

### Prerequisites

The exporter exposes a standard `/metrics` endpoint. You need a
Prometheus-compatible scraper to collect the metrics and store them
in a time-series database. Common setups:

- **Grafana Alloy** → scrapes `/metrics` → pushes to **Mimir** or
  **Prometheus** → Grafana queries the TSDB
- **Prometheus** → scrapes `/metrics` directly → Grafana queries
  Prometheus

Add a scrape target for the exporter in your Alloy config or
Prometheus config:

```yaml
# Alloy example
prometheus.scrape "plex_exporter" {
  targets    = [{"__address__" = "plex-exporter:9594"}]
  forward_to = [prometheus.remote_write.mimir.receiver]
}
```

```yaml
# Prometheus example
scrape_configs:
  - job_name: plex-exporter
    static_configs:
      - targets: ['plex-exporter:9594']
```

### Import the Dashboard

1. In Grafana, go to **Dashboards → Import**
2. Upload `grafana-dashboard.json` from this repository
3. Select your Prometheus datasource when prompted

The dashboard includes panels for server info, library sizes and
item counts, active sessions with transcode details, bandwidth
usage, host resource utilization, and WebSocket connection status.

## Docker Healthcheck

The container includes both an HTTP health endpoint and a CLI
health probe for distroless Docker healthchecks.

The main process writes a marker file at `/tmp/.healthy` once the
HTTP server is listening. The `health` subcommand checks for this
file — it requires no shell, HTTP client, or open port.

**When it becomes unhealthy:**
- The initial connection to Plex fails (bad URL, invalid token)
- The HTTP metrics server fails to start

**WebSocket disconnects do not cause unhealthy status.** The
exporter automatically reconnects with exponential backoff. The
`plex_websocket_connected` metric tracks connection state for
alerting.

| Type | Command | Meaning |
|------|---------|---------|
| Docker | `/plex-exporter health` | Exit 0 = metrics server running |


## Code Quality

| Metric | Value |
|--------|-------|
| [Test Coverage](https://go.dev/blog/cover) | 76.3% |
| Tests | 160 |
| [Cyclomatic Complexity](https://en.wikipedia.org/wiki/Cyclomatic_complexity) (avg) | 4.0 |
| [Cognitive Complexity](https://www.sonarsource.com/docs/CognitiveComplexity.pdf) (avg) | 4.0 |
| [Mutation Efficacy](https://en.wikipedia.org/wiki/Mutation_testing) | 87.3% (59 runs) |
| Test Framework | Property-based ([rapid](https://github.com/flyingmutant/rapid)) + table-driven |

Tests cover Prometheus metric collection (all 13 metric descriptors,
server/library/session metrics, Plex Pass gating), session tracking
(play/stop/resume lifecycle, concurrent sessions, bandwidth
accumulation, prune timeouts), transcode detection and subtitle
classification, library item counting with artist-type fallback,
bandwidth tracking with boundary conditions, HTTP client retry logic,
and the full refresh cycle (server info, library items, resources).
Property-based tests verify invariants across all pure functions.

Not tested: WebSocket connection management, the main event loop,
and ticker-based refresh scheduling — these are I/O-bound runtime
paths. WebSocket health is monitored via the
`plex_websocket_connected` Prometheus metric.

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency | Version | Source |
|------------|---------|--------|
| golang | `1.26-alpine` | [Go](https://hub.docker.com/_/golang) |
| gcr.io/distroless/static-debian13 | `nonroot` | [Distroless](https://github.com/GoogleContainerTools/distroless) |
| github.com/coder/websocket | `v1.8.14` | [GitHub](https://github.com/coder/websocket) |
| github.com/prometheus/client_golang | `v1.23.2` | [GitHub](https://github.com/prometheus/client_golang) |
| pgregory.net/rapid | `v1.2.0` | [pkg.go.dev](https://pkg.go.dev/pgregory.net/rapid) |

## Design Principles

- **Always up to date**: Base images, packages, and libraries are updated automatically via Renovate. Unlike many community Docker images that ship outdated or abandoned dependencies, these images receive continuous updates.
- **Minimal attack surface**: When possible, pure Go apps use `gcr.io/distroless/static:nonroot` (no shell, no package manager, runs as non-root). Apps requiring system packages use Alpine with the minimum necessary privileges.
- **Digest-pinned**: Every `FROM` instruction pins a SHA256 digest. All GitHub Actions are digest-pinned.
- **Multi-platform**: Built for `linux/amd64` and `linux/arm64`.
- **Healthchecks**: Every container includes a Docker healthcheck.
- **Provenance**: Build provenance is attested via GitHub Actions, verifiable with `gh attestation verify`.

## Credits

This is an original tool that builds upon [prometheus-plex-exporter](https://github.com/jsclayton/prometheus-plex-exporter).
- Grafana Hackathon 2022
  — the original hackathon project that started it all
- [prometheus-plex-exporter](https://github.com/jsclayton/prometheus-plex-exporter)
  by [@jsclayton](https://github.com/jsclayton) — the post-hackathon
  fork that added graceful shutdown and Go module updates
- [prometheus-plex-exporter](https://github.com/timothystewart6/prometheus-plex-exporter)
  by [@timothystewart6](https://github.com/timothystewart6) — the
  actively maintained upstream with multi-package architecture,
  transcode tracking, and configurable library refresh
- [Plex Media Server API](https://developer.plex.tv/pms/) — the
  official API documentation
- [coder/websocket](https://github.com/coder/websocket) — Go
  WebSocket implementation
- [prometheus/client_golang](https://github.com/prometheus/client_golang)
  — Prometheus instrumentation library for Go

## Disclaimer

These images are built with care and follow security best practices, but they are intended for **homelab use**. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).
