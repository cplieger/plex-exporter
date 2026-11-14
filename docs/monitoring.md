# Monitoring and alerts

plex-exporter serves a metrics page and ships metric alert rules and a Grafana dashboard for it. Every alert needs a metric store such as Prometheus, and no rule reads the log. This page lists the alert rules, the log lines and the dashboard downloads, for readers who load the rules or import the dashboard.

## What you get

| You run | You get | You set up |
| --- | --- | --- |
| Prometheus, Mimir or VictoriaMetrics, with Alertmanager (the monitoring guide's starting stack) | the ten alerts, the `up` series, and with Grafana the bundled dashboard | a scrape job named `plex-exporter` on port 9594, and [`alerts/promql.yaml`](../alerts/promql.yaml) |
| Loki and Alertmanager, no metric store | no alerts: plex-exporter needs a metric store | Prometheus or another metric store, as the [monitoring guide](https://github.com/cplieger/docs/blob/main/docs/monitoring.md) sets up |
| Grafana-managed alerting only | the rules imported into Grafana | a Prometheus data source and the rules of `alerts/promql.yaml` |
| Docker or Unraid only | the Docker healthcheck, which notifies nobody | an uptime prober on `/healthz`, or something that watches Docker health |

## Metrics

The page is always on, at `/metrics` on port 9594, which `LISTEN_ADDR` changes. It has no login, so keep the port on your own network, because its labels carry Plex user names, device names and the titles being played. [How plex-exporter works](how-it-works.md#when-each-metric-appears) explains when each series appears. [Metrics reference](metrics.md) lists every metric and label.

## Logs

The exporter logs `key=value` text to stderr at the info level. No alert rule reads the log. While Plex is unreachable it logs `refresh failed` once, and again when the error changes, then `refresh recovered` with `failed_for` once Plex answers. A full session tracker logs `session map full, dropping new sessions` once, and `session map has room again` with the number it dropped once a slot frees.

## Alerting

plex-exporter serves Prometheus metrics on `/metrics`, and every alert rule reads them. The ten PromQL rules in [`alerts/promql.yaml`](../alerts/promql.yaml) go to Prometheus, the Mimir ruler or vmalert, and no rule reads the container log. [Loading metric alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-metric-alert-rules) shows how. They cover:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `PlexExporterTargetDown` | no successful scrape of the exporter for 15m, so the scrape itself is failing | warning |
| `PlexExporterTargetAbsent` | the exporter has no `up` series at all for 15m, so the target has left service discovery | warning |
| `PlexExporterAPIUnreachable` | the authenticated Plex API poll reports `plex_http_reachable=0` for 10m, often a revoked or invalid `PLEX_TOKEN` | warning |
| `PlexExporterSessionPollFailing` | `plex_session_poll_reachable=0` for 10m while the rest of the Plex API answers, so every session metric is absent or stale | warning |
| `PlexExporterCollectionErrors` | the `plex_exporter_errors_total` counter keeps rising for some `type` over 30m | warning |
| `PlexExporterSessionMapFull` | `plex_sessions_dropped_total` rose in the last 15m: the session tracker is at its cap of 256 and drops new Plex sessions, so the session metrics undercount | warning |
| `PlexExporterRefreshIncomplete` | `plex_refresh_deadline_exceeded_total` rose by more than 3 in 15m: refresh cycles run out of time before the Plex Pass gauges are read, so they keep serving values from an earlier cycle | warning |
| `PlexExporterLibraryItemsCollapsed` | a library's item count falls more than 50% below its level 1 to 2 hours earlier for 30m, a fall to zero included | warning |
| `PlexExporterLibraryWalkStale` | a library has not been read item by item for 4h plus twice the last pass time, or never since the exporter started 6h ago | warning |
| `PlexExporterLibraryScanOverdue` | Plex scans on a schedule, and has not scanned a library for more than twice its scan interval plus an hour, for 15m | warning |

Every rule except the two target rules reads a series the exporter publishes, so all of them go quiet together when it stops being scraped. The two target rules catch that case.

`PlexExporterTargetDown` and `PlexExporterTargetAbsent` must carry a `job` matcher, because they ask whether this exporter is visible at all. Set both to the job name your scrape config uses. Keep the matcher exact rather than a regex. A regex asks whether any matching target is up, so one healthy replica hides a failed one, and the `absent()` result then has no label to route on.

The two target rules need each other. `PlexExporterTargetDown` catches a target that is configured and failing, and keeps its labels so the alert names the instance. `PlexExporterTargetAbsent` catches a target that no longer exists, such as a deleted Kubernetes pod or ServiceMonitor, a dropped scrape target or a removed scrape config. In that case `up` has no series at all, so `up == 0` can never match.

The metric rules need nothing beyond the scrape. The eight exporter series are published from the first scrape, and no setting can turn one off. `plex_library_items`, which `PlexExporterLibraryItemsCollapsed` reads, appears once the exporter has learned the server identity and read a library count.

### Notes on each rule

- `PlexExporterAPIUnreachable`. A rejected token, a 404 or a TLS error at startup keeps the exporter running and reports here, and the log names the cause. The exporter stops at startup only when its configuration is wrong, and `PlexExporterTargetDown` then fires after 15m. It stops when `PLEX_URL` is unset, or when neither `PLEX_TOKEN` nor `PLEX_TOKEN_FILE` supplies a token. It stops when the token file cannot be read or holds only whitespace, or when `PLEX_URL` is not an http or https address or has no host. It also stops when the `PLEX_CA_CERT_PATH` file cannot be read or holds no certificate, or when the listen address cannot be bound.
- `PlexExporterSessionMapFull`. The dropped session is missing, every other series stays healthy and no error counter moves, so `plex_sessions_dropped_total` is the signal, and `plex_sessions_tracked` shows how full the tracker is. Expect either a genuinely large number of concurrent streams or a Plex server minting session keys faster than the tracker reclaims them. A stopped session is reclaimed after 60s, an idle one after 5m. The alert resolves 15m after the last dropped session.
- `PlexExporterRefreshIncomplete`. The stale gauges are `plex_host_cpu_utilization_ratio`, `plex_host_memory_utilization_ratio` and `plex_transmit_bytes_total`. They keep the values from an earlier cycle rather than going absent. This path records no `plex_exporter_errors_total` error and leaves `plex_http_reachable` at 1, because the fetches earlier in the same cycle succeeded. The threshold of more than 3 skipped reads in 15m ignores a single slow cycle, which skips both reads. More than that means Plex has been answering slowly for minutes.
- `PlexExporterSessionPollFailing`. The session metrics it leaves absent or stale are `plex_plays_active`, `plex_play_seconds_total` and the bandwidth and bitrate gauges. A poll that fails only while something is playing points at the session payload itself.
- `PlexExporterLibraryWalkStale`. The threshold grows with `plex_library_walk_pass_duration_seconds`, so a large server whose pass takes hours is not paged for its size. The rule needs `process_start_time_seconds`, which the exporter serves from its Go runtime, and it stays quiet for 6h after a restart. The log line `library walk failed` names the library and the error.
- `PlexExporterLibraryScanOverdue`. Plex counts its scan schedule from when the server started, so a restart just before a scan can push the next one almost two intervals out, and the extra hour covers a long scan. The rule reads `plex_server_scheduled_scan_interval_seconds`, which exists only while Scan my library periodically is on, so it never fires while Plex scans only when files change or when you ask. Reading Plex's settings needs the server owner's token, as the rest of plex-exporter does.
- `PlexExporterLibraryItemsCollapsed`. A fall to zero is covered too. The exporter publishes `plex_library_items=0` once it reads a library as empty, so the drop is a full 100%. A library whose count cannot be read is a different condition and does not fire here. The series holds its last value and the failed fetch raises `plex_exporter_errors_total{type="library_items"}`, which `PlexExporterCollectionErrors` alerts on.

Thresholds, the `for:` windows and the `severity` labels are starting points. If you run more than one instance, add your scrape `job` label to the metric selectors.

## Dashboard

[`grafana-dashboard.json`](../grafana-dashboard.json) imports as the Plex Exporter dashboard and needs Grafana 13.2 or newer. It has six tabs:

- Overview shows the Plex version, the streams and transcodes running now and the items added in the selected range. Below them, a Needs attention row appears only while something needs you. One tile each covers an exporter that is not scraped, Plex or its session poll being unreachable, a full session tracker and stale Plex Pass data. One tile each covers a library that lost most of its items, has not been read or is overdue for a Plex scan. One tile each covers reads that kept failing at some point in the selected range, stale or hidden watch history, skipped libraries, items with no size and a Plex update. A tile with a matching alert rule below shows exactly while that alert fires, because it reads the rule's `ALERTS` series, which Prometheus and the Mimir ruler both store beside your metrics. Failed reads shows when `PlexExporterCollectionErrors` fired at any point in the selected range and counts every failed read in that range. On a Prometheus that does not run these rules, no `ALERTS` series exists. The tile then checks the rule's condition itself, and shows a minute or two after the alert would have fired. The plex-exporter tile then finds the exporters by the metrics they sent, so it needs no `job` name. The tab also shows the active streams with how each video transcode runs and their bandwidth beside the bitrate of the media they play. A server with Plex Pass also gets the host's CPU and memory.
- Libraries shows the library totals and, below them, a tile for each item type you have. It shows each library's size, averages and how long ago it last gained an item, was scanned and was read. It also shows the scans, analysis and metadata tasks Plex is running now. It shows the storage and items each library added in the selected range, also as a share of its size. Two charts then show that change over the range, one band per library stacked on the others, and the 10 newest items follow. It also shows storage by resolution and codec.
- Storage cleanup shows the space held by items nobody has played and by extra versions. It shows each library's space by when it was last watched, and a bar links to that library's unplayed items in Plex Web. Plex lists what your own account has not played there, so the list can be longer than the never-played figure, which counts every account. It also shows the 10 largest items in each library with when anyone last played them.
- Streaming history shows the watch time in the selected range by user and by library, and the 20 most watched shows, films and tracks, each show counted whole. It also shows the hours watched by delivery method and by home or remote network, in bars an hour long at 24 hours and a day long at 30 days, and the bandwidth by network. Watch time is an estimate from the 5-second session check and goes back only as far as your Prometheus keeps metrics.
- Transcoding shows the hours of video transcoding over the range by where Plex decoded and encoded the video, the share encoded on hardware, and the transcode hours by player, codec and resolution.
- Exporter health shows how long the last full library pass took, the Plex request retries in the range and any failed reads by type. It also shows the libraries with items of unknown size, each library's last read, the sessions it tracks against its cap of 256 and the Plex Pass fetches that ran out of time. Together they show what plex-exporter could read. A broken read state shows as a tile on Overview.

A title over the selected range names its length, such as Items added in 7 days, and says the range instead while the data source cannot answer.

The Library variable filters the Libraries and Storage cleanup tabs by library. Plex numbers libraries per server, so with several servers selected one choice shows that library number on each of them. With several servers selected, every per-library row and bar names its server before the library. Two libraries with the same name on one server show their library number after the name, such as `Movies (id 2)`. Two servers that report the same name are selected together by the Server variable, and each shows the first eight characters of its server ID after the name, such as `home (id 3f2a9c1e)`.

The dashboard is versioned with the app. The `grafana-dashboard.json` of release `<tag>` matches the metrics that image serves. The file sets `metadata.name` to `plex-exporter`, which Grafana uses as the dashboard UID. Because the name stays the same from release to release, a re-import over the existing one, or a file provider reading the newer file, updates it in place. Pin it the way you pin the image, with the tag of the image you run.

On Grafana 13.1 or older, use the `grafana-dashboard.json` of release [v4.1.3](https://github.com/cplieger/plex-exporter/releases/tag/v4.1.3), the last one in the older dashboard format. That file gets no further changes, so you maintain it yourself.

Each release attaches the file and its checksum:

- `https://github.com/cplieger/plex-exporter/releases/download/<tag>/grafana-dashboard.json`, which works as the Grafana Helm chart's `dashboards.<provider>.<name>.url` with `curlOptions: "-sLf"`, because the release URL redirects, or as a Terraform `http` data source.
- `https://github.com/cplieger/plex-exporter/releases/download/<tag>/grafana-dashboard.json.sha256` beside it.

Renovate can keep the URL current with its `github-releases` datasource. The same file is also the OCI artifact `ghcr.io/cplieger/plex-exporter/dashboard:<tag>`, with the artifact type `application/vnd.grafana.dashboard.v2+json`. [Running on Kubernetes](kubernetes.md#delivering-the-dashboard) shows how to load the file with grafana-operator and with Terraform.

The tiles that show the current state ask for an instant value rather than a range. So when a label splits a series, such as a new `version` after a Plex upgrade, each tile still shows one row.

## Knowing the app is alive

| Route | Method | Port | Login | Answers |
| --- | --- | --- | --- | --- |
| `/healthz` | `GET`, `HEAD` | 9594, or the port in `LISTEN_ADDR` | none | `200 {"status":"ok"}`, or `503 {"status":"unhealthy","reasons":[...]}` |
| `/metrics` | `GET` | the same port | none, so keep the port on your network | Prometheus text format, gzipped when the scraper asks for it |

**What healthy means.** `/healthz` is healthy from the moment the exporter's metrics server listens until it starts to shut down. The exporter keeps this verdict in memory, so a read-only root filesystem never makes it unhealthy. Plex being unreachable, a rejected token included, never makes it unhealthy. That shows on `/metrics` as `plex_http_reachable` at `0` and in the log.

**Docker health.** The image checks itself every 30 seconds with `/plex-exporter health`, which asks `/healthz` on the container's loopback, or on the address `LISTEN_ADDR` names. Docker marks the container unhealthy after 3 failures, and allows 15 seconds after a start. Docker's health status notifies nobody on its own, and Docker does not restart an unhealthy container.

**Who is told.**

| You run | A dead app | A wedged app |
| --- | --- | --- |
| Prometheus with this app's rules | `PlexExporterTargetDown`, or `PlexExporterTargetAbsent` once the target is gone | `PlexExporterTargetDown` when the page stops answering, `PlexExporterLibraryWalkStale` when the library reads stop |
| Loki only | an uptime prober on `/healthz` | the same |
| Docker or Unraid only | an uptime prober on `/healthz`, or something that watches Docker health | the same |

**Probing from outside.** Check `GET http://<host>:9594/healthz` and pass on status 200 only. Do not match the body. Use a 5-second timeout, check every 60 seconds, alert after 3 failures, and wait 15 seconds after a first start. A Kubernetes liveness probe that must ignore readiness uses a `tcpSocket` probe on the port, as [Running on Kubernetes](kubernetes.md#probes) shows.
