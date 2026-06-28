# Contributing to plex-exporter

A Prometheus exporter for Plex Media Server, written in Go. This guide
covers what a contributor needs beyond what the code makes obvious.

## Architecture

`main.go` is the composition root and holds wiring only: env parsing,
constructing the concrete types from `internal/*`, the HTTP listener,
and goroutine launch. Keep behaviour out of it — all logic lives in the
`internal/` packages:

- `internal/plexapi` — pure JSON/XML wire types for the Plex API
  responses. No imports beyond `encoding/json`.
- `internal/plex` — HTTP client for Plex, including retry semantics, the
  `ErrNotFound` sentinel, and the `HTTPStatusError` type that lets callers
  tell 4xx from 5xx.
- `internal/library` — the `Library` value type plus pure classification
  helpers (`IsType`, `ContentTypeLabel`, `Build`, `ItemCountTypes`).
  Deterministic and side-effect free.
- `internal/sessions` — in-memory active-session tracker, updated by the
  poll loop and snapshotted by the collector. Owns the prune logic and
  the session bounds.
- `internal/metrics` — the Prometheus descriptor set (labels, descs,
  error-type allowlist). Exports descriptor variables only.
- `internal/server` — the `Server` orchestrator: refresh loop,
  per-subsystem refresh methods, and the Prometheus `Describe`/`Collect`
  implementation that emits metrics from `Server` state.

Sessions are tracked by polling `/status/sessions` every 5s; a stateful
tracker reconciles poll snapshots into metric updates (prune after 60s
idle). Library item counts are cached and refreshed every 15 minutes.

## Local development

The module targets the Go version pinned in `go.mod`. There is no
Makefile — use the standard Go toolchain:

```sh
go build ./...        # compile
go test ./...         # run the suite
golangci-lint run     # lint + vet (config in .golangci.yaml)
golangci-lint fmt     # apply gofumpt + gci formatting
```

`golangci-lint run` reports unformatted files as issues, so formatting
is enforced by the lint step. The config sets gofumpt with `extra-rules`
(groups adjacent same-type params, forbids naked returns) and `gci`
import ordering (standard → third-party → local). `sloglint` is
`kv-only`, so write key/value `slog` calls, not attribute helpers.

Tests are property-based (`pgregory.net/rapid`) plus table-driven, and
live beside the code they test. Cover pure functions with properties;
the poll path is covered by `session_poll_test.go`. Not tested: main
event loop, ticker scheduling (I/O-bound runtime paths — monitored via
`plex_http_reachable`).

The container build is reproducible and rootless:

```sh
docker build -t plex-exporter .
```

It compiles with `CGO_ENABLED=0` and ships on
`gcr.io/distroless/static:nonroot` — no shell, no package
manager. Runtime config is via env (`PLEX_SERVER`, `PLEX_TOKEN`,
`LISTEN_ADDRESS`, `PLEX_CA_CERT_PATH`, `TZ`); see the README for the
full reference.

## Conventions and gotchas

- **Metric cardinality is load-bearing.** Per-session bitrate lives in
  its own `plex_session_bitrate_kbps` gauge, _not_ as a label on
  `plex_plays_active`/`plex_play_seconds_total` — adaptive streaming
  reports changing bitrates that would otherwise explode label
  cardinality. Don't add high-churn values as labels.
- **Lock ordering.** When holding both, acquire the `Server` mutex
  before the session tracker's mutex; the reverse risks deadlock.
- **Plex Pass degrades gracefully.** Host CPU/memory and
  bandwidth-transmission metrics come from undocumented endpoints that
  404 without Plex Pass. Those paths must stay non-fatal — the exporter
  keeps serving every other metric.
- **New metrics:** add the descriptor in `internal/metrics`, emit it
  from `Collect` in `internal/server`, and document it in the README's
  metrics tables.

## Commits and PRs

Open an issue first for larger changes so the approach can be discussed.
Commits follow
[Conventional Commits](https://www.conventionalcommits.org/) and are
parsed by git-cliff to generate release notes: `feat:`, `fix:`, and
`sec:` drive releases; `chore:`/`ci:`/`docs:`/`style:`/`test:` do not.
Write the subject as the changelog line a user would read.

## Conduct & security

By participating you agree to the
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report security issues through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md),
never in a public issue.
