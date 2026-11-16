# Metrics reference

This page lists every metric and label plex-exporter serves, for readers who build their own panels or queries. The page is always on, at `/metrics` on port 9594, which `LISTEN_ADDR` changes. It has no login, so keep the port on your own network, because its labels carry Plex user names, device names and the titles being played. [How plex-exporter works](how-it-works.md#when-each-metric-appears) explains when each series appears.

## Exporter metrics

These eight carry no server labels and appear from the first scrape.

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `plex_http_reachable` | Gauge | _none_ | `1` when the last refresh of the Plex API succeeded, `0` when it failed |
| `plex_session_poll_reachable` | Gauge | _none_ | `1` when the last `/status/sessions` poll succeeded, `0` when it failed |
| `plex_http_retries_total` | Counter | _none_ | HTTP retries the exporter's Plex client made, across all requests |
| `plex_exporter_errors_total` | Counter | `type` | Exporter errors by type: `refresh`, `sessions_fetch`, `metadata_fetch`, `invalid_rating_key`, `metrics_server`, `library_items`, `library_walk`, `history_fetch`, `activities_fetch`, `sections_fetch`, `updater_fetch` or `prefs_fetch` |
| `plex_exporter_build_info` | Gauge | `version` | Always `1`. `version` is the plex-exporter release the process runs, `dev` for a build without a release tag. A query joins it `on (job, instance)` to scope a Go or process metric to the exporter |
| `plex_sessions_tracked` | Gauge | _none_ | Plex sessions the exporter tracks, at most 256 |
| `plex_sessions_dropped_total` | Counter | _none_ | New Plex sessions the exporter refused because it already tracked 256. A session refused on several polls counts on each |
| `plex_refresh_deadline_exceeded_total` | Counter | `fetch` | Plex Pass reads skipped because the refresh ran out of its 45-second deadline, by `fetch`: `resources` (host CPU and memory) or `bandwidth` (`plex_transmit_bytes_total`). A read Plex answers with 404, as it does without Plex Pass, does not count |

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
| `plex_server_scheduled_scan_interval_seconds` | Gauge | `server`, `server_id` | How often Plex scans every library on its own, Plex's Library scan interval setting. Present only while Scan my library periodically is on and Plex's settings were read in the last 45 minutes |

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

## Go runtime and process metrics

The page also carries the Go runtime and process families (`go_*`, `process_*`) under the names Go's official Prometheus client uses, so stock Go dashboards work. `process_start_time_seconds`, which `PlexExporterLibraryWalkStale` reads, is one of them. `process_gc_pause_seconds_total` and `process_uptime_seconds` have no counterpart in that client. On Linux, the CPU, memory, file descriptor and network families appear when the exporter can read them from `/proc`.
