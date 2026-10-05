# Contributing to plex-exporter

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

- A new metric needs five edits. Add its descriptor in `internal/metrics/descs.go`, list it in `AllDescs`, emit it from `Collect` in `internal/server`, add its row to the metrics tables in `docs/monitoring.md` and raise the metric count in the README's `## Monitoring` section.
- A new `type` value for `plex_exporter_errors_total` goes into `metrics.ErrorTypes` and into that metric's row in `docs/monitoring.md`. `RecordError` drops a type the list does not hold, so the counter never moves.
- A new value for `stream_type`, `media_type`, `location` or a resolution label goes into its allowlist in `internal/metrics/descs.go` and into `docs/monitoring.md`. An unlisted value is reported as `other`.
- The rules in `alerts/` and the panels in `grafana-dashboard.json` match metric names, label names and log messages as text. Update them with any rename. No test compares them, so a rename passes CI and breaks the alert or panel.
- When code holds both, take the `Server` mutex before the session tracker's lock, because the reverse order can deadlock. A function passed to `Tracker.UpdateLibraryLabels` runs under the tracker's lock, so it must not call `RecordError` or anything that locks `Server`.

## Releases

Renaming or removing a metric or a label, or changing when a series appears, breaks the dashboards and alerts users built on it. Mark such a commit as breaking with `!`.

A change to `grafana-dashboard.json` takes `fix:` or `feat:`. Under `chore:` or `docs:` it replaces the dashboard's OCI artifact at the current version tags, and the GitHub Release keeps the old file.
