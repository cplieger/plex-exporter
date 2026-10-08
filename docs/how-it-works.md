# How plex-exporter works

This page explains how often plex-exporter reads Plex, how it tracks streams and when each metric appears. It is for readers who want to know where a number on the dashboard comes from.

## How often it reads Plex

The exporter polls Plex on four schedules:

- Every 5 seconds, it reads `/status/sessions` for the streams playing now, so a new stream appears within seconds.
- Every 5 seconds, it also reads the server's root, host resources and bandwidth endpoints, because they carry live state such as the active transcode count and host CPU.
- Every 60 seconds, it reads `/media/providers`, which holds the server's identity and its library list with each library's total length and storage.
- Every 15 minutes, it reads the episode, track and item counts of each library, so a count can lag after a large library scan.

It also reads three background endpoints inside that 5-second refresh, whenever each is due. Each read has its own 10-second limit. A failure never fails the refresh, but a slow answer holds the refresh up until it arrives or the limit passes:

- Every 30 seconds, `/activities`, for the scans and analysis Plex is running.
- Every 60 seconds, `/library/sections`, for each library's last scan time.
- Every 15 minutes, `/updater/status`, for whether a Plex update is out.

Two longer reads run on schedules of their own, so they never hold up the refresh:

- Every hour, every movie, show and home-video library, item by item in pages of 500, for the largest items, storage by resolution, codec and last play, and the newest arrivals. A pass that takes longer than an hour starts the next one straight after it. A show among a library's 10 largest or 10 newest items gets one more read for its year, once while it stays there.
- At start, the whole watch history, then the new plays every 5 minutes and the last 30 days once a day.

All five share one budget of 2 requests a second, so a large library takes longer to read rather than loading Plex harder. A refresh read waits for its turn inside its 10-second limit. A server with 100,000 episodes makes about 400 of these requests an hour.

A refresh cycle has 45 seconds to finish, and a session poll has 30 seconds.

For each stream, the exporter reads the item's library metadata once and keeps it with the session. It reads it again when the item changes, such as when the next episode starts.

## How streams are tracked

The session tracker turns each poll into metric updates. A stream gets its series once the exporter has seen it playing, so a stream that was already paused when the exporter started appears once it resumes.

When a stream ends, its series stay for 1 to 2 minutes and then go. A session that gets no update for 5 minutes is removed as stale. The tracker holds 256 sessions at most. Past that, it logs `session map full, dropping new session` and leaves the new stream out, which the `PlexExporterSessionMapFull` alert rule catches.

## When each metric appears

The four exporter metrics, `plex_http_reachable`, `plex_session_poll_reachable`, `plex_http_retries_total` and `plex_exporter_errors_total`, carry no server labels. They appear from the first scrape, before Plex has answered, and each keeps one series from then on.

Every other metric carries the `server` and `server_id` labels of the server it describes. It appears once the exporter has read the server's identity from Plex. So `absent(plex_server_info)` means the exporter has never reached Plex since it started.

Host CPU and memory and the bandwidth counter come from statistics endpoints that answer only with Plex Pass. Those three series are absent until the endpoints have answered once, and every other metric works without Plex Pass.

A library's `plex_library_items` series is absent until its count has been read once. A library read as empty reports `0`.

The library content series appear after a library's first complete item-by-item read, and a library that fails a later read keeps the figures of its last complete one. The watch figures appear only once watch history has been read in full. [Monitoring and alerts](monitoring.md#watch-figures) explains when they are hidden.

## Why it is built this way

- It polls `/status/sessions` every 5 seconds. Plex embeds each stream's transcode decisions in that answer, so a timer is all the exporter needs.
- It is one Go binary. Its direct dependencies are `prometheus/client_golang` and a few small helper libraries, listed in [Security](hardening.md#what-the-image-contains), and the rest is the Go standard library.
- It runs on `gcr.io/distroless/static-debian13` as the non-root user 65532, with no shell or package manager.
- It serves a standard `/metrics` endpoint, so any Prometheus-compatible scraper and any Grafana dashboard can read it. It has no charts of its own.
- Each stream's bitrate is its own `plex_session_bitrate_kbps` series rather than a label on the play metrics. Plex changes the bitrate during adaptive streaming, and a label would start a new series each time.
- Titles and the library, user, device and device type names from Plex are cut to 128 bytes, and session keys to 64. The `stream_type`, `media_type`, `location` and resolution labels are mapped to a fixed set, and the tracker holds 256 sessions at most. So an unexpected Plex answer cannot create an unbounded number of series. [Monitoring and alerts](monitoring.md#session-labels) lists the sets.
