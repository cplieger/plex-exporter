# plex-exporter

[![CI](https://github.com/cplieger/plex-exporter/actions/workflows/ci.yaml/badge.svg)](https://github.com/cplieger/plex-exporter/actions/workflows/ci.yaml)
[![GitHub release](https://img.shields.io/github/v/release/cplieger/plex-exporter)](https://github.com/cplieger/plex-exporter/releases)
[![Image Size](https://ghcr-badge.egpl.dev/cplieger/plex-exporter/size)](https://github.com/cplieger/plex-exporter/pkgs/container/plex-exporter)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/plex-exporter/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/plex-exporter)
[![Coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/plex-exporter/badges/coverage.json)](https://github.com/cplieger/plex-exporter/actions/workflows/coverage.yml)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13216/badge)](https://www.bestpractices.dev/projects/13216)

See what your Plex server is doing in Grafana — sessions, libraries, bandwidth, transcoding.

## What it does

Connects to your Plex Media Server and exposes metrics (active sessions, library sizes, bandwidth, transcoding status) in a format that Prometheus can scrape and Grafana can visualize.

**Key metrics exposed:**

- Library duration, storage, and item counts (movies, episodes, tracks)
- Active session details (user, device, resolution, stream type)
- Transcode type detection (video/audio/both) and subtitle handling
- Session bandwidth and location (LAN/WAN)
- Host CPU and memory utilization (Plex Pass)
- Bandwidth transmission totals (Plex Pass)
- WebSocket connection health
- Active transcode session count

### Why this design

- **WebSocket for real-time session tracking** — listens to the Plex notification stream for instant session updates instead of polling on an interval
- **Single binary with no runtime dependencies** — minimal direct Go dependencies (`coder/websocket` and `prometheus/client_golang`), everything else is stdlib
- **Distroless and rootless** — runs on `gcr.io/distroless/static` as UID 65534 with no shell or package manager, minimizing attack surface
- **Prometheus-native** — exposes a standard `/metrics` endpoint that works with any Prometheus-compatible scraper and any Grafana dashboard, no custom visualization layer

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

## Quick start

Available from both `ghcr.io/cplieger/plex-exporter` and `docker.io/cplieger/plex-exporter` — identical images and tags.

```yaml
services:
  plex-exporter:
    image: ghcr.io/cplieger/plex-exporter:latest
    container_name: plex-exporter
    restart: unless-stopped
    user: "1000:1000"  # match your host user

    environment:
      TZ: "Europe/Paris"
      PLEX_SERVER: "http://plex:32400"  # full URL including scheme and port
      PLEX_TOKEN: "your-plex-token"  # admin token from Plex Web settings

    ports:
      - "9594:9594"
```

## Configuration reference

### Environment variables

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| `PLEX_SERVER` | Full URL of your Plex Media Server including scheme and port (e.g. `http://192.0.2.100:32400`) | `http://plex:32400` | Yes |
| `PLEX_TOKEN` | Plex authentication token for the server administrator. Get it from Plex Web → Settings → XML view → myPlexAccessToken | - | Yes |
| `TZ` | Container timezone | `Europe/Paris` | No |
| `LISTEN_ADDRESS` | Address and port for the metrics HTTP server | `:9594` | No |
| `PLEX_CA_CERT_PATH` | Path to a PEM file containing your Plex server's CA certificate. When set, that CA is added to the TLS RootCAs pool — TLS verification stays **on**, pinned to your CA. Required only when (a) your `PLEX_SERVER` uses `https://` and (b) the cert isn't trusted by the OS bundle (i.e. you signed it yourself or with a private CA). Plain `http://` URLs and Plex's official `*.plex.direct` HTTPS URLs need **no** TLS env var. | unset | No |

### TLS / certificate setup

Pick the configuration that matches your Plex server:

| Your `PLEX_SERVER` looks like | What to do |
|---|---|
| `http://plex:32400` (Docker network, LAN, etc.) | nothing — TLS isn't in use |
| `https://<hash>.plex.direct:32400` (Plex's official cert) | nothing — Let's Encrypt is trusted by default |
| `https://192.0.2.100:32400` or `https://plex.local` (self-signed / private CA) | set `PLEX_CA_CERT_PATH` to the PEM file of the CA that signed your Plex cert |

### Ports

| Port | Description |
|------|-------------|
| `9594` | Prometheus metrics endpoint (`/metrics`) and health check (`/api/health`) |

## Metrics reference

### HTTP Endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/metrics` | GET | Prometheus metrics (see below) |
| `/api/health` | GET | Returns `{"status":"OK"}` when ready, 503 when starting/stopping |

### Server Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `plex_server_info` | Gauge (always 1) | `server`, `server_id`, `version`, `platform`, `platform_version`, `plex_pass` | Server metadata and Plex Pass status |
| `plex_host_cpu_utilization_ratio` | Gauge | `server`, `server_id` | Host CPU utilization as a ratio (0.0–1.0). Requires Plex Pass. |
| `plex_host_memory_utilization_ratio` | Gauge | `server`, `server_id` | Host memory utilization as a ratio (0.0–1.0). Requires Plex Pass. |
| `plex_transmit_bytes_total` | Counter | `server`, `server_id` | Cumulative bytes transmitted (from Plex bandwidth API). Requires Plex Pass. Resets on container restart — indicative only. |
| `plex_estimated_transmit_bytes_total` | Counter | `server`, `server_id` | Estimated bytes transmitted based on session bitrates. Resets on container restart — indicative only. |
| `plex_active_transcode_sessions` | Gauge | `server`, `server_id` | Number of active video transcode sessions (from root endpoint, no Plex Pass needed) |
| `plex_websocket_connected` | Gauge | `server`, `server_id` | WebSocket connection status: `1` = connected, `0` = disconnected |
| `plex_http_reachable` | Gauge | `server`, `server_id` | HTTP polling reachability: `1` = last refresh succeeded, `0` = failed |
| `plex_exporter_errors_total` | Counter | `server`, `server_id`, `type` | Exporter error count by type. Types: `refresh`, `websocket_dial`, `websocket_read`, `invalid_message`, `sessions_fetch`, `metadata_fetch`, `invalid_rating_key`, `metrics_server`, `library_items`. |

### Library Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `plex_library_duration_milliseconds` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id` | Total duration of all items in the library (ms) |
| `plex_library_storage_bytes` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id` | Total storage used by the library (bytes) |
| `plex_library_items` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id`, `content_type` | Number of items in the library. `content_type` is `movies`, `episodes`, `tracks`, `photos`, or `items`. Refreshed every 15 minutes. |

### Session Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `plex_plays_active` | Gauge | `server`, `server_id`, `library`, `library_id`, `library_type`, `media_type`, `title`, `child_title`, `grandchild_title`, `stream_type`, `stream_resolution`, `stream_file_resolution`, `device`, `device_type`, `user`, `session`, `transcode_type`, `subtitle_action`, `location`, `local` | Currently active play sessions (1 per session). Use `count(plex_plays_active)` for total stream count. Removed after 60s of inactivity. |
| `plex_play_seconds_total` | Counter | _(same as above)_ | Cumulative play time for the session (seconds) |
| `plex_session_bandwidth_kbps` | Gauge | `server`, `server_id`, `session`, `user`, `location` | Real-time session bandwidth from the Plex Sessions API (kbps) |
| `plex_session_bitrate_kbps` | Gauge | `server`, `server_id`, `session`, `user`, `location` | Live stream bitrate per session (kbps). Replaces the former `stream_bitrate` label on `plex_plays_active`/`plex_play_seconds_total`, which caused unbounded cardinality as Plex reports changing bitrates during adaptive streaming. |

### Session Label Reference

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

## Healthcheck

The container includes an HTTP health endpoint (`/api/health`) and a CLI probe (`/plex-exporter health`) that checks a `/tmp/.healthy` marker file written once the HTTP server is listening — no shell, HTTP client, or open port required. The container becomes unhealthy only if the initial Plex connection fails or the metrics server fails to start; WebSocket disconnects do not trigger unhealthy status because the exporter reconnects automatically with exponential backoff (monitor via `plex_websocket_connected`).

## Security

**No vulnerabilities found.** All scans clean.

| Tool | Result |
|------|--------|
| [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) | No vulnerabilities in call graph |
| [golangci-lint](https://golangci-lint.run/) (gosec, gocritic) | 0 issues |
| [trivy](https://trivy.dev/) | 0 vulnerabilities (distroless base) |
| [grype](https://github.com/anchore/grype) | 0 vulnerabilities |
| [gitleaks](https://github.com/gitleaks/gitleaks) | No secrets detected |
| [semgrep](https://semgrep.dev/) | 2 info (false positives) |
| [hadolint](https://github.com/hadolint/hadolint) | Clean |

Connects outbound to Plex only. The `/metrics` endpoint serves
read-only Prometheus data (standard for internal exporters).
`PLEX_TOKEN` is never logged or exposed in metrics. Runs as
`nonroot` on a distroless base image with no shell.

**Details for advanced users:** Plex response bodies capped at
10 MB via `io.LimitReader`. WebSocket messages capped at 1 MB.
All HTTP clients use explicit 10s timeouts; the metrics server
sets `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`,
`IdleTimeout`, and `MaxHeaderBytes` (1 MB). Rating keys
validated via `strconv.Atoi` before URL construction. Explicit
`MinVersion: tls.VersionTLS12` set on TLS config. Semgrep flags
the `/tmp/.healthy` marker and the opt-in TLS skip (both
intentional).

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency | Source |
|------------|--------|
| golang | [Go](https://hub.docker.com/_/golang) |
| gcr.io/distroless/static | [Distroless](https://github.com/GoogleContainerTools/distroless) |
| github.com/coder/websocket | [GitHub](https://github.com/coder/websocket) |
| github.com/prometheus/client_golang | [GitHub](https://github.com/prometheus/client_golang) |
| github.com/prometheus/client_model | [GitHub](https://github.com/prometheus/client_golang) |
| golang.org/x/sync | [Go stdlib](https://pkg.go.dev/golang.org/x/sync) |
| pgregory.net/rapid | [pkg.go.dev](https://pkg.go.dev/pgregory.net/rapid) |

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

## Contributing

Issues and pull requests are welcome. Please open an issue first for
larger changes so the approach can be discussed before implementation.

## Disclaimer

These images are built with care and follow security best practices, but they are intended for **homelab use**. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).
