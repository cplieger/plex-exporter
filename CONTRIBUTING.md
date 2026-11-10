# Contributing to plex-exporter

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

- A new metric needs six edits. Add its descriptor in `internal/metrics/descs.go`, list it in `AllDescs`, emit it from the collector in `internal/server/collect.go`, regenerate `internal/metrics/testdata/families.golden` with `UPDATE_GOLDEN=1 go test ./internal/metrics/`, add its row to the metrics tables in `docs/monitoring.md` and raise the metric count in the README's `## Monitoring` section.
- A new `type` value for `plex_exporter_errors_total` goes into `metrics.ErrorTypes` and into that metric's row in `docs/monitoring.md`. `RecordError` drops a type the list does not hold, so the counter never moves.
- A new value for `stream_type`, `media_type`, `location` or a resolution label goes into its allowlist in `internal/metrics/descs.go` and into `docs/monitoring.md`. An unlisted value is reported as `other`.
- The rules in `alerts/` and the panels in `grafana-dashboard.json` match metric names and label names as text. Update them with any rename. A test fails when a rule or a panel reads a metric name the exporter does not serve, and the family golden fails on any change to a family's name, type, HELP text or labels. No test checks the label names a rule or panel reads, so update them by hand with a label rename.
- Save a `grafana-dashboard.json` change made in the Grafana UI with Export, then Export as code, choosing the V2 Resource model.
- When code holds both, take the `Server` mutex before the session tracker's lock, because the reverse order can deadlock. A function passed to `Tracker.UpdateLibraryLabels` runs under the tracker's lock, so it must not call `RecordError` or anything that locks `Server`.

## Releases

Renaming or removing a metric or a label, or changing when a series appears, breaks the dashboards and alerts users built on it. Mark such a commit as breaking with `!`.

A change to `grafana-dashboard.json` takes `fix:` or `feat:`. Under `chore:` or `docs:` it replaces the dashboard's OCI artifact at the current version tags, and the GitHub Release keeps the old file.
