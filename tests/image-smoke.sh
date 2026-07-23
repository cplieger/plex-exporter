#!/bin/sh
# Runtime image smoke-test harness — CANONICAL COPY in cplieger/ci
# (configs/image-smoke.sh), synced to each serving app's tests/image-smoke.sh
# by scripts/classify-repos.py (a repo enrolls by committing a
# tests/image-smoke.conf; see below). DO NOT edit the synced copy in an app
# repo — change it here and let the sync land it.
#
# Invoked by the shared CI docker job:  sh tests/image-smoke.sh <image-ref>
#
# It starts the assembled image and waits for the container's own HEALTHCHECK
# to report "healthy" — proving the binary runs in the final image, loads its
# config, binds any listener, and its health probe works, catching failures the
# build cannot see (a broken //go:embed frontend, a missing runtime dependency,
# a server that never binds, a broken HEALTHCHECK). It fails fast on an early
# exit (a crash-boot is reported by its exit code, more debuggable than
# "unhealthy") and dumps the container log tail only on failure.
#
# Per-app knobs come from tests/image-smoke.conf beside this script; everything
# below the config block is identical across apps. The .conf is a POSIX-sh
# fragment sourced for these variables (all optional):
#
#   SMOKE_APP_NAME   label for log lines + container name (default: "image")
#   SMOKE_TIMEOUT    seconds to wait for "healthy" (default: 120). Size it to
#                    cover the image's HEALTHCHECK start-period plus a couple of
#                    intervals; a slow-but-OK cold boot must not be failed early.
#   SMOKE_RUN_ARGS   extra `docker run` args (env, tmpfs, ...) as a word-split
#                    string, e.g. "-e FOO=bar --tmpfs /input". Values must not
#                    contain spaces (these are controlled test configs).
#
# The harness also sets $SMOKE_DIR (this script's own absolute directory)
# before sourcing the .conf, so an app that needs a config/fixture file on disk
# can bind-mount a committed fixture dir, e.g.:
#   SMOKE_RUN_ARGS="-e SYNC_INTERVAL=off -v ${SMOKE_DIR}/fixtures:/config:ro"
set -eu

IMG="${1:?usage: image-smoke.sh <image-ref>}"

# Absolute directory of this script (also holds image-smoke.conf and any per-app
# fixtures). Exposed to the .conf as $SMOKE_DIR so a .conf can bind-mount a
# committed fixture dir with an absolute source path (docker -v requires one).
SMOKE_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

# Per-app config lives beside this script (repo-local, NOT synced). Pre-set the
# knobs so `set -u` is safe and a repo with no .conf still runs with defaults.
SMOKE_APP_NAME=""
SMOKE_TIMEOUT=""
SMOKE_RUN_ARGS=""
CONF="$SMOKE_DIR/image-smoke.conf"
if [ -f "$CONF" ]; then
  # shellcheck disable=SC1090  # per-app config path, resolved at runtime
  . "$CONF"
fi

APP="${SMOKE_APP_NAME:-image}"
TIMEOUT="${SMOKE_TIMEOUT:-120}"
case "$TIMEOUT" in
  '' | *[!0-9]*)
    printf 'FAIL: SMOKE_TIMEOUT must be a non-negative integer, got "%s"\n' "$TIMEOUT" >&2
    exit 1
    ;;
esac
NAME="smoke-${APP}-$$"

# shellcheck disable=SC2317,SC2329  # invoked indirectly via trap
cleanup() {
  code=$?
  # Dump container logs only on failure (a passing run stays quiet).
  if [ "$code" -ne 0 ]; then
    printf '%s\n' "--- container logs (tail) ---" >&2
    docker logs "$NAME" 2>&1 | tail -40 >&2 || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# SMOKE_RUN_ARGS is intentionally word-split (simple test args, no spaces).
# shellcheck disable=SC2086
docker run -d --name "$NAME" $SMOKE_RUN_ARGS "$IMG" >/dev/null

start=$(date +%s)
deadline=$((start + TIMEOUT))
status=starting
while [ "$(date +%s)" -lt "$deadline" ]; do
  # Fail fast on an early exit: poll .State.Running before the health status so
  # a crash-boot is caught by its exit code (more debuggable than "unhealthy")
  # and the verdict never depends on what health a stopped container reports.
  if [ "$(docker inspect --format '{{ .State.Running }}' "$NAME" 2>/dev/null || echo missing)" != "true" ]; then
    ec=$(docker inspect --format '{{ .State.ExitCode }}' "$NAME" 2>/dev/null || echo '?')
    printf 'FAIL: %s container exited early (exit code %s)\n' "$APP" "$ec" >&2
    exit 1
  fi
  status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2>/dev/null || echo gone)
  case "$status" in
    healthy)
      printf '%s image smoke: ok (healthy after %ss)\n' "$APP" "$(($(date +%s) - start))"
      exit 0
      ;;
    unhealthy)
      printf 'FAIL: %s reported unhealthy\n' "$APP" >&2
      exit 1
      ;;
    no-healthcheck)
      printf 'FAIL: image has no HEALTHCHECK to assert against\n' >&2
      exit 1
      ;;
    gone)
      printf 'FAIL: %s container is gone\n' "$APP" >&2
      exit 1
      ;;
  esac
  sleep 1
done
printf 'FAIL: %s did not become healthy within %ss (last status: %s)\n' "$APP" "$TIMEOUT" "$status" >&2
exit 1
