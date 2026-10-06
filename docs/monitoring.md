# Monitoring and alerts

This page lists every metric and label plex-exporter serves, how to get the matching Grafana dashboard, and the alert rules that ship with it. It is for readers who build their own panels or load the rules into a ruler.

## Endpoints

| Endpoint | Method | Description |
| --- | --- | --- |
| `/metrics` | GET | Prometheus metrics, listed below |
| `/api/health` | GET | `{"status":"OK"}` when ready, 503 while starting or stopping |

Both answer on port 9594 by default. [How plex-exporter works](how-it-works.md#when-each-metric-appears) explains when each series appears.

## Exporter metrics

These four carry no server labels and appear from the first scrape.

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `plex_http_reachable` | Gauge | _none_ | `1` when the last refresh of the Plex API succeeded, `0` when it failed |
| `plex_session_poll_reachable` | Gauge | _none_ | `1` when the last `/status/sessions` poll succeeded, `0` when it failed |
| `plex_http_retries_total` | Counter | _none_ | HTTP retries the exporter's Plex client made, across all requests |
| `plex_exporter_errors_total` | Counter | `type` | Exporter errors by type: `refresh`, `sessions_fetch`, `metadata_fetch`, `invalid_rating_key`, `metrics_server` or `library_items` |

## Server metrics

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `plex_server_info` | Gauge, always 1 | `server`, `server_id`, `version`, `platform`, `platform_version`, `plex_pass` | Server details and Plex Pass status |
| `plex_host_cpu_utilization_ratio` | Gauge | `server`, `server_id` | Host CPU use from 0.0 to 1.0. Needs Plex Pass |
| `plex_host_memory_utilization_ratio` | Gauge | `server`, `server_id` | Host memory use from 0.0 to 1.0. Needs Plex Pass |
| `plex_transmit_bytes_total` | Counter | `server`, `server_id` | Bytes sent, from the Plex bandwidth API. Needs Plex Pass. Starts again from zero when the container restarts, so treat it as indicative |
| `plex_active_transcode_sessions` | Gauge | `server`, `server_id` | Active video transcodes, from the root endpoint. Works without Plex Pass |

## Library metrics

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `plex_library_duration_milliseconds` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id` | Total length of all items in the library, in milliseconds |
| `plex_library_storage_bytes` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id` | Disk space the library uses, in bytes |
| `plex_library_items` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id`, `content_type` | Items in the library, read every 15 minutes. `content_type` is `movies`, `episodes`, `tracks`, `photos` or `items` |

## Session metrics

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `plex_plays_active` | Gauge | `server`, `server_id` and the play labels below | `1` for each active stream. Use `count(plex_plays_active)` for the number of streams |
| `plex_play_seconds_total` | Counter | the same as `plex_plays_active` | Time played in the session, in seconds |
| `plex_session_bandwidth_kbps` | Gauge | `server`, `server_id`, `session`, `user`, `location` | Bandwidth of the session from the Plex sessions API, in kbps |
| `plex_session_bitrate_kbps` | Gauge | `server`, `server_id`, `session`, `user`, `location` | Live bitrate of the stream, in kbps |

The play labels are `library`, `library_id`, `library_type`, `media_type`, `title`, `child_title`, `grandchild_title`, `grandchild_index`, `stream_type`, `stream_resolution`, `stream_file_resolution`, `device`, `device_type`, `user`, `session`, `transcode_type`, `subtitle_action`, `location` and `local`.

## Session labels

| Label | Values | Description |
| --- | --- | --- |
| `stream_type` | `directplay`, `copy`, `transcode`, `unknown` | How the stream is delivered |
| `transcode_type` | `none`, `video`, `audio`, `both` | What is being transcoded |
| `subtitle_action` | `none`, `burn`, `copy`, `transcode` | How subtitles are handled |
| `location` | `lan`, `wan`, `unknown` | Where the client is on the network |
| `local` | `true`, `false` | Whether the client is on the local network |
| `media_type` | `movie`, `episode`, `track`, `clip`, `photo` | The Plex media type |

For an episode, `title` is the show, `child_title` the season, `grandchild_title` the episode title and `grandchild_index` the episode number. For a music track, `grandchild_index` is the track number. For a movie, `title` is the movie name and the other three are empty.

A value outside the sets above becomes `other`. A missing `stream_type` or `location` becomes `unknown`, and a missing `media_type` becomes `other`. The two resolution labels take `sd`, `480`, `576`, `720`, `1080`, `4k` or `2160`, stay empty when Plex reports none, and become `other` for any other value. An empty `subtitleDecision` from Plex is reported as `subtitle_action="none"`.

## Dashboard

The dashboard is versioned with the app. The `grafana-dashboard.json` of release `<tag>` matches the metrics that image serves, and its `uid` stays the same, so a re-import updates the existing dashboard in place. Pin it the way you pin the image, with the tag of the image you run.

Each release attaches the file and its checksum:

- `https://github.com/cplieger/plex-exporter/releases/download/<tag>/grafana-dashboard.json`, which works as grafana-operator `spec.url`, as the Grafana Helm chart's `dashboards.<provider>.<name>.url`, or as a Terraform `http` data source.
- `https://github.com/cplieger/plex-exporter/releases/download/<tag>/grafana-dashboard.json.sha256` beside it.

The same file is the OCI artifact `ghcr.io/cplieger/plex-exporter/dashboard:<tag>`, for grafana-operator `spec.oci`. [Running on Kubernetes](kubernetes.md#delivering-the-dashboard) has a `GrafanaDashboard` example. Renovate can keep either form current, with its `github-releases` datasource for the URL and its `docker` datasource for the OCI tag.

The tiles that show the current state ask for an instant value rather than a range. So when a label splits a series, such as a new `version` after a Plex upgrade, each tile still shows one row.

## Alerting

plex-exporter serves Prometheus metrics on `/metrics` and writes its own diagnostics to its container log. The six PromQL rules in [`alerts/promql.yaml`](../alerts/promql.yaml) go to Prometheus or the Mimir ruler, and the three LogQL rules in [`alerts/logql.yaml`](../alerts/logql.yaml) go to Loki's ruler. [Loading metric alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-metric-alert-rules) and [Loading an app's alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-an-apps-alert-rules) show how. They cover:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `PlexExporterTargetDown` | no successful scrape of the exporter for 15m, so the scrape itself is failing | warning |
| `PlexExporterTargetAbsent` | the exporter has no `up` series at all for 15m, so the target has left service discovery | warning |
| `PlexAPIUnreachable` | the authenticated Plex API poll reports `plex_http_reachable=0` for 10m, often a revoked or invalid `PLEX_TOKEN` | warning |
| `PlexSessionPollFailing` | `plex_session_poll_reachable=0` for 10m while the rest of the Plex API answers, so every session metric is absent or stale | warning |
| `PlexExporterCollectionErrors` | the `plex_exporter_errors_total` counter keeps rising for some `type` over 30m | warning |
| `PlexLibraryItemsCollapsed` | a library's item count falls more than 50% below its level 1 to 2 hours earlier for 30m, a fall to zero included | warning |
| `PlexExporterFatalError` | the exporter logs an `ERROR`, such as a rejected config or token, a bind failure, a metrics-server failure or a recovered panic | warning |
| `PlexExporterSessionMapFull` | the session tracker is at its cap of 256 and drops new Plex sessions, so the session metrics undercount | warning |
| `PlexExporterRefreshIncomplete` | refresh cycles run out of time before the Plex Pass gauges are read, so they keep serving values from an earlier cycle | warning |

The three log rules exist because their conditions leave no series to read. A configuration the exporter refuses outright reaches `PlexExporterFatalError` before any series exists. Every metric rule reads a series the exporter publishes, so all of them go quiet together when it stops being scraped. The two target rules catch that case.

`PlexExporterTargetDown` and `PlexExporterTargetAbsent` must carry a `job` matcher, because they ask whether this exporter is visible at all. Set both to the job name your scrape config uses. Keep the matcher exact rather than a regex. A regex asks whether any matching target is up, so one healthy replica hides a failed one, and the `absent()` result then has no label to route on.

The two target rules need each other. `PlexExporterTargetDown` catches a target that is configured and failing, and keeps its labels so the alert names the instance. `PlexExporterTargetAbsent` catches a target that no longer exists, such as a deleted Kubernetes pod or ServiceMonitor, a dropped scrape target or a removed scrape config. In that case `up` has no series at all, so `up == 0` can never match.

The log rules need no parser stage. The exporter writes Go slog logfmt to stderr, which Docker captures, so `level=ERROR` and the message text are plain substrings. Go renders the level in uppercase, so match `level=ERROR` and never `level=error`. There is no log-level setting. The level is fixed at INFO, so every WARN and ERROR line these rules match is always written, and a DEBUG line never is.

The metric rules need nothing beyond the scrape. The four exporter series are published from the first scrape, and no setting can turn one off. `plex_library_items`, which `PlexLibraryItemsCollapsed` reads, appears once the exporter has learned the server identity and read a library count.

### Notes on each rule

- `PlexExporterFatalError`. Every configuration and startup error is followed by a non-zero exit, so the process is gone before `/metrics` can publish anything, and a restart policy repeats the line on every attempt. The causes are an unset `PLEX_URL` or `PLEX_TOKEN`, a rejected token or another 4xx, a 404 from a host that is not the right Plex server, a TLS or CA misconfiguration, a `LISTEN_ADDR` already in use, and a metrics-server failure after startup. The HTTP middleware also logs at ERROR and does not exit, for a recovered panic or a 5xx on `/metrics` or `/api/health`. The line names which. `PlexExporterTargetDown` reaches the same outage 15m later, without the cause.
- `PlexExporterSessionMapFull`. Nothing in `/metrics` reports this. The dropped session is missing, every other series stays healthy and no error counter moves. Expect either a genuinely large number of concurrent streams or a Plex server minting session keys faster than the tracker reclaims them. A stopped session is reclaimed after 60s, an idle one after 5m. The log line carries the `tracked` and `cap` attributes.
- `PlexExporterRefreshIncomplete`. The stale gauges are `plex_host_cpu_utilization_ratio`, `plex_host_memory_utilization_ratio` and `plex_transmit_bytes_total`. They keep the values from an earlier cycle rather than going absent. This path records no error and leaves `plex_http_reachable` at 1, because the fetches earlier in the same cycle succeeded. The threshold of 3 lines in 15m ignores a single slow cycle. More than that means Plex has been answering slowly for minutes.
- `PlexSessionPollFailing`. The session metrics it leaves absent or stale are `plex_plays_active`, `plex_play_seconds_total` and the bandwidth and bitrate gauges. A poll that fails only while something is playing points at the session payload itself.
- `PlexLibraryItemsCollapsed`. A fall to zero is covered too. The exporter publishes `plex_library_items=0` once it reads a library as empty, so the drop is a full 100%. A library whose count cannot be read is a different condition and does not fire here. The series holds its last value and the failed fetch raises `plex_exporter_errors_total{type="library_items"}`, which `PlexExporterCollectionErrors` alerts on.

Thresholds, the `for:` windows and the `severity` labels are starting points. If you run more than one instance, add your scrape `job` label to the metric selectors. Change the `container` selector to the label your log collector sets, and route by whatever labels your Alertmanager uses.
