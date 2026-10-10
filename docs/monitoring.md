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
| `plex_exporter_errors_total` | Counter | `type` | Exporter errors by type: `refresh`, `sessions_fetch`, `metadata_fetch`, `invalid_rating_key`, `metrics_server`, `library_items`, `library_walk`, `history_fetch`, `activities_fetch`, `sections_fetch` or `updater_fetch` |

## Server metrics

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `plex_server_info` | Gauge, always 1 | `server`, `server_id`, `version`, `platform`, `platform_version`, `plex_pass` | Server details and Plex Pass status |
| `plex_host_cpu_utilization_ratio` | Gauge | `server`, `server_id` | Host CPU use from 0.0 to 1.0. Needs Plex Pass |
| `plex_host_memory_utilization_ratio` | Gauge | `server`, `server_id` | Host memory use from 0.0 to 1.0. Needs Plex Pass |
| `plex_transmit_bytes_total` | Counter | `server`, `server_id` | Bytes sent, from the Plex bandwidth API. Needs Plex Pass. Starts again from zero when the container restarts, so treat it as indicative |
| `plex_active_transcode_sessions` | Gauge | `server`, `server_id` | Active video transcodes, from the root endpoint. Works without Plex Pass |
| `plex_server_activity_progress_ratio` | Gauge | `server`, `server_id`, `activity_type`, `library_type`, `library_id` | Progress of a Plex background task from 0 to 1, or `-1` with no estimate. `activity_type` is `scan`, `analysis`, `metadata`, `maintenance`, `streaming` or `other`. `library_id` is empty for a server-wide task. The series disappear once the task list has not been read for 90 seconds |
| `plex_server_update_available` | Gauge | `server`, `server_id`, `state` | `1` when Plex lists a release other than the running version, `0` when it is up to date. `state` is the deciding release state. Absent until Plex has checked without an error |
| `plex_server_update_checked_timestamp_seconds` | Gauge | `server`, `server_id` | When Plex last checked for an update, as a Unix time |

## Library metrics

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `plex_library_duration_milliseconds` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id` | Total length of all items in the library, in milliseconds |
| `plex_library_storage_bytes` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id` | Disk space the library uses, in bytes |
| `plex_library_items` | Gauge | `server`, `server_id`, `library_type`, `library`, `library_id`, `content_type` | Items in the library, read every 15 minutes. `content_type` is `movies`, `episodes`, `tracks`, `photos` or `items` |

## Library content metrics

The exporter reads every movie, show and home-video library item by item once an hour, up to 32 libraries. These series appear after a library's first complete read. Music and photo libraries are not read this way. The base labels are `server`, `server_id`, `library_type` and `library_id`. They carry no `library` name, so join it from `plex_library_storage_bytes` on `server`, `server_id` and `library_id`.

| Metric | Type | Extra labels | Description |
| --- | --- | --- | --- |
| `plex_library_top_item_bytes` | Gauge | `rating_key`, `title`, `year` | Size of one of the 10 largest items in the library. A show is the sum of its episodes. An item or show with any unknown size is not ranked |
| `plex_library_top_item_last_played_timestamp_seconds` | Gauge | `rating_key`, `title`, `year` | When any account last played that item, as a Unix time, `0` for no recorded play. Present only while the watch figures are ready |
| `plex_library_recent_item_added_timestamp_seconds` | Gauge | `rating_key`, `title`, `year`, `episode` | When one of the 10 newest items on the server was added. A show is listed once, with its newest episode as `SxxEyy` in `episode` |
| `plex_library_watch_age_bytes` | Gauge | `last_watched` | Bytes of sized items by newest play by any account, `never`, `over_1y`, `90d_1y` or `under_90d`. Present only while the watch figures are ready |
| `plex_library_watch_age_items` | Gauge | `last_watched` | Items by the same buckets, sized or not. Present only while the watch figures are ready |
| `plex_library_resolution_bytes` | Gauge | `video_resolution` | Bytes of sized items by resolution: `sd`, `480`, `576`, `720`, `1080`, `2160`, `other` or `unknown` |
| `plex_library_codec_bytes` | Gauge | `video_codec` | Bytes of sized items by codec: `h264`, `hevc`, `av1`, `vp9`, `mpeg2video`, `mpeg4`, `vc1`, `other` or `unknown` |
| `plex_library_multi_version_items` | Gauge | _none_ | Items with more than one version |
| `plex_library_extra_version_bytes` | Gauge | _none_ | Bytes of every version of an item except its largest |
| `plex_library_newest_item_added_timestamp_seconds` | Gauge | _none_ | When the newest item was added. Absent when no item has a known added time |
| `plex_library_added_items` | Gauge | `window` | Items added in the last `24h` or `7d` |
| `plex_library_unsized_items` | Gauge | _none_ | Items whose file size Plex did not report for every version. They are left out of every byte figure |
| `plex_library_walkable` | Gauge, always 1 | _none_ | One series for each library the exporter reads item by item |
| `plex_library_walk_last_success_timestamp_seconds` | Gauge | _none_ | When the last complete read of the library finished |
| `plex_library_walk_duration_seconds` | Gauge | _none_ | How long that read took |
| `plex_library_last_scan_timestamp_seconds` | Gauge | _none_ | When Plex last scanned the library, for every library type. Absent when Plex reports no scan time |

Server-level read state:

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `plex_library_walk_pass_duration_seconds` | Gauge | `server`, `server_id` | How long the last pass over every read library took |
| `plex_library_walk_skipped_libraries` | Gauge | `server`, `server_id` | Video libraries past the limit of 32 |
| `plex_library_walk_complete` | Gauge | `server`, `server_id` | `1` when the last pass read every library and none was skipped |
| `plex_library_watch_age_complete` | Gauge | `server`, `server_id` | `1` when the watch figures of every read library come from current watch history with no unmatched rows |
| `plex_history_read_status` | Gauge | `server`, `server_id`, `status` | `1` for the outcome of the last full watch-history read: `complete`, `over_limit` or `failed` |
| `plex_history_last_success_timestamp_seconds` | Gauge | `server`, `server_id` | When a watch-history read last succeeded |
| `plex_history_unmatched_rows` | Gauge | `server`, `server_id` | History rows with a non-numeric item key, or an item key but no play time, since the last full read, counted up to 250,000 |
| `plex_history_deleted_item_rows` | Gauge | `server`, `server_id` | History rows for items no longer in Plex since the last full read, counted up to 250,000 |

### Watch figures

"No recorded play" means no account on the server has a play of the item in Plex's watch history, and the token owner has not marked it watched with a play count. History deleted in Plex still counts as no recorded play, and so does an item another account marked as watched without playing it. The dashboard calls this Never played.

The watch figures are the `last_watched` families and the last-played times. A library publishes them only when its last read ran while watch history was complete and current, and history has not been rebuilt since. After a rebuild, the exporter reads the affected libraries again at once. While any history row has a non-numeric item key, or an item key but no play time, the watch figures are hidden. `plex_library_watch_age_complete` then reads `0`, because a watched item could otherwise read as never played.

Plex keeps the plays of items you delete and sends them without an item. Those rows are counted in `plex_history_deleted_item_rows` and never hide the watch figures. A play of a deleted item does not count for a copy you add again later, because Plex gives that copy a new item.

The exporter reads the whole history when it starts, then the plays since its last read every 5 minutes, and the last 30 days once a day for plays a client reported late. A failed read keeps the figures for up to an hour. A failed full read is retried after 5 minutes, doubling to 6 hours.

### Fixed limits

| Limit | Value |
| --- | --- |
| Largest items per library, newest items per server | 10 |
| Libraries read item by item | 32 |
| Items per library read | 500,000, in pages of 500 |
| Time for one library read | 10 minutes |
| Watch-history rows read at start | 250,000, in pages of 500, within 20 minutes. Past either limit the status is `over_limit`, retried once a day |
| Watch-history rows per 5-minute read, per daily read | 10,000 and 100,000. A 5-minute read that reaches its limit carries on from the next row at the following read |
| Unmatched and deleted-item watch-history rows counted | 250,000 each, as many as a full read holds. Later reads stop counting at that number |
| Background requests to Plex | 2 per second at most, counting every page request, shared by the library reads, the history reads and the reads below |
| Title labels | 128 bytes, one line, control and direction-changing characters removed |

Item titles stay in Prometheus for as long as it keeps data, including titles that later leave the top 10. At most about 3,800 series exist at once on a server at the limits, and about 340 on a typical server.

## Session metrics

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `plex_plays_active` | Gauge | `server`, `server_id` and the play labels below | `1` for each active stream. Use `count(plex_plays_active)` for the number of streams |
| `plex_play_seconds_total` | Counter | the same as `plex_plays_active` | Time played in the session, in seconds |
| `plex_session_bandwidth_kbps` | Gauge | `server`, `server_id`, `session`, `user`, `location` | Bandwidth of the session from the Plex sessions API, in kbps |
| `plex_session_bitrate_kbps` | Gauge | `server`, `server_id`, `session`, `user`, `location` | Live bitrate of the stream, in kbps |
| `plex_session_video_transcode` | Gauge, 1 or 0 | `server`, `server_id`, `session`, `decode`, `encode`, `source_codec`, `target_codec` | One series for each session transcoding video. It reads 1 while the stream plays or pauses, and 0 after it ends until it is pruned. `decode` and `encode` are `hardware`, `software` or `unknown`, and the codecs use the `plex_library_codec_bytes` values. Join on `server_id` and `session`, because Plex numbers sessions per server |

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

On `plex_session_video_transcode`, a half of the transcode is `hardware` when Plex names the hardware API it uses for that half, such as `vaapi`, `nvenc` or `qsv`. Plex's own dashboard marks a stream `(hw)` by the same rule. A half is `software` when Plex says whether hardware was requested but names no API for that half. It is `unknown` when Plex sends no hardware fields or an unreadable one. A paused stream still reads 1, because Plex keeps its transcoder running.

Both labels report what Plex says about the session. Plex can keep reporting hardware for a session it has restarted on the CPU, so a `hardware` label beside high CPU load points at Plex.

## Dashboard

[`grafana-dashboard.json`](../grafana-dashboard.json) imports as the Plex Exporter dashboard and needs Grafana 13.2 or newer. It has six tabs:

- Overview shows the Plex version, the streams, transcodes and Plex tasks running now, the oldest library scan and the items added in the selected range. Below them, a tile appears only while something needs you: the exporter is not scraped, Plex or its session poll is unreachable, a library lost most of its items or has not been read, reads kept failing at some point in the selected range, watch history is stale or its figures are hidden, libraries are skipped, items have no size, or a Plex update is out. A tile with a matching alert rule below shows exactly while that alert fires, because it reads the rule's `ALERTS` series, which Prometheus and the Mimir ruler both store beside your metrics. Failed reads shows when `PlexExporterCollectionErrors` fired at any point in the selected range and counts every failed read in that range. While no `ALERTS` series exists for a rule, as on a Prometheus that does not run these rules, the tile checks the rule's condition itself and shows a minute or two after the alert would have fired. The plex-exporter tile then finds the exporters by the metrics they sent, so it needs no `job` name. The tab also shows the active streams with how each video transcode runs, their bandwidth beside the bitrate of the media they play, and what Plex is working on. A server with Plex Pass also gets the host's CPU and memory.
- Libraries shows the library totals, each library's size, averages and how long ago it last gained an item, was scanned and was read, and the storage and items each library added in the selected range, also as a share of its size. It shows storage and item count over time stacked, and each library's change since the start of the range on its own scale, so a small addition to a large library still shows. It also shows storage by resolution and codec, and the 10 newest items.
- Storage cleanup shows the space held by items nobody has played and by extra versions. It shows each library's space by when it was last watched, and a bar links to that library's unplayed items in Plex Web. Plex lists what your own account has not played there, so the list can be longer than the never-played figure, which counts every account. It also shows the 10 largest items in each library with when anyone last played them.
- Streaming history shows the watch time in the selected range by user and by library, and the 20 most watched shows, films and tracks, each show counted whole. It also shows the hours watched by delivery method and by home or remote network, in bars an hour long at 24 hours and a day long at 30 days, and the bandwidth by network. Watch time is an estimate from the 5-second session check and goes back only as far as your Prometheus keeps metrics.
- Transcoding shows the hours of video transcoding over the range by where Plex decoded and encoded the video, the share encoded on hardware, and the transcode hours by player, codec and resolution.
- Exporter health shows how long the last full library pass took, the Plex request retries in the range, any failed reads by type, the libraries with items of unknown size and each library's last read, so you can see what plex-exporter could read. A broken read state shows as a tile on Overview.

The Library variable filters the Libraries and Storage cleanup tabs by library. Plex numbers libraries per server, so with several servers selected one choice shows that library number on each of them. With several servers selected, every per-library row and bar names its server before the library. Two libraries with the same name on one server show their library number after the name, such as `Movies (id 2)`. Two servers that report the same name are selected together by the Server variable, and each shows the first eight characters of its server ID after the name, such as `home (id 3f2a9c1e)`.

The dashboard is versioned with the app. The `grafana-dashboard.json` of release `<tag>` matches the metrics that image serves. The file sets `metadata.name` to `plex-exporter`, which Grafana uses as the dashboard UID. Because the name stays the same from release to release, a re-import over the existing one, or a file provider reading the newer file, updates it in place. Pin it the way you pin the image, with the tag of the image you run.

On Grafana 13.1 or older, use the `grafana-dashboard.json` of release [v4.1.3](https://github.com/cplieger/plex-exporter/releases/tag/v4.1.3), the last one in the older dashboard format. That file gets no further changes, so you maintain it yourself.

Each release attaches the file and its checksum:

- `https://github.com/cplieger/plex-exporter/releases/download/<tag>/grafana-dashboard.json`, which works as the Grafana Helm chart's `dashboards.<provider>.<name>.url` with `curlOptions: "-sLf"`, because the release URL redirects, or as a Terraform `http` data source.
- `https://github.com/cplieger/plex-exporter/releases/download/<tag>/grafana-dashboard.json.sha256` beside it.

Renovate can keep the URL current with its `github-releases` datasource. The same file is also the OCI artifact `ghcr.io/cplieger/plex-exporter/dashboard:<tag>`, with the artifact type `application/vnd.grafana.dashboard.v2+json`. [Running on Kubernetes](kubernetes.md#delivering-the-dashboard) shows how to load the file with grafana-operator and with Terraform.

The tiles that show the current state ask for an instant value rather than a range. So when a label splits a series, such as a new `version` after a Plex upgrade, each tile still shows one row.

## Alerting

plex-exporter serves Prometheus metrics on `/metrics` and writes its own diagnostics to its container log. The seven PromQL rules in [`alerts/promql.yaml`](../alerts/promql.yaml) go to Prometheus or the Mimir ruler, and the three LogQL rules in [`alerts/logql.yaml`](../alerts/logql.yaml) go to Loki's ruler. [Loading metric alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-metric-alert-rules) and [Loading an app's alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-an-apps-alert-rules) show how. They cover:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `PlexExporterTargetDown` | no successful scrape of the exporter for 15m, so the scrape itself is failing | warning |
| `PlexExporterTargetAbsent` | the exporter has no `up` series at all for 15m, so the target has left service discovery | warning |
| `PlexAPIUnreachable` | the authenticated Plex API poll reports `plex_http_reachable=0` for 10m, often a revoked or invalid `PLEX_TOKEN` | warning |
| `PlexSessionPollFailing` | `plex_session_poll_reachable=0` for 10m while the rest of the Plex API answers, so every session metric is absent or stale | warning |
| `PlexExporterCollectionErrors` | the `plex_exporter_errors_total` counter keeps rising for some `type` over 30m | warning |
| `PlexLibraryItemsCollapsed` | a library's item count falls more than 50% below its level 1 to 2 hours earlier for 30m, a fall to zero included | warning |
| `PlexLibraryWalkStale` | a library has not been read item by item for 4h plus twice the last pass time, or never since the exporter started 6h ago | warning |
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
- `PlexLibraryWalkStale`. The threshold grows with `plex_library_walk_pass_duration_seconds`, so a large server whose pass takes hours is not paged for its size. The rule needs `process_start_time_seconds`, which the exporter serves from its Go runtime, and it stays quiet for 6h after a restart. The log line `library walk failed` names the library and the error.
- `PlexLibraryItemsCollapsed`. A fall to zero is covered too. The exporter publishes `plex_library_items=0` once it reads a library as empty, so the drop is a full 100%. A library whose count cannot be read is a different condition and does not fire here. The series holds its last value and the failed fetch raises `plex_exporter_errors_total{type="library_items"}`, which `PlexExporterCollectionErrors` alerts on.

Thresholds, the `for:` windows and the `severity` labels are starting points. If you run more than one instance, add your scrape `job` label to the metric selectors. Change the `container` selector to the label your log collector sets, and route by whatever labels your Alertmanager uses.
