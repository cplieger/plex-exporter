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
#   SMOKE_LOG_PATTERN  optional post-start assertion: a fixed string that must
#                    appear in the container log before the run passes (default:
#                    empty = health alone is the verdict). For an app whose
#                    HEALTHCHECK deliberately does not cover a surface - a
#                    listener started asynchronously, a feature whose failure is
#                    logged without flipping health - the log line it emits on
#                    success is the only evidence available to the harness. The
#                    wait shares the SMOKE_TIMEOUT deadline: the container must
#                    be healthy AND have logged the pattern before it expires.
#
# A .conf may also override smoke_verify() (default: no-op) for app-specific
# assertions that need the RUNNING healthy container - e.g. asserting that
# every target of a served importmap answers 200, which no static check can
# prove because the targets are produced during the image build. It runs once,
# after health (and SMOKE_LOG_PATTERN, when set), with $SMOKE_CONTAINER holding
# the container name; it runs in a subshell under `set -e`, so the first failing
# command fails the smoke test (a non-zero return does too). The harness never
# publishes ports, so probe from INSIDE the container (`docker exec
# "$SMOKE_CONTAINER" curl ...`) rather than assuming host reachability.
#
# A .conf that creates host state of its own (a `mktemp -d` fixture dir, a
# generated key) overrides the smoke_cleanup() function to remove it; the
# harness's EXIT trap calls it after removing the container, so acquisition and
# release live side by side in the .conf and every invocation - local or CI -
# leaves nothing behind.
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
SMOKE_LOG_PATTERN=""
# Default app cleanup hook: a .conf that creates host state overrides it. Defined
# BEFORE the source so the EXIT trap can always call it, and so a .conf that
# creates nothing needs no boilerplate.
# shellcheck disable=SC2329  # invoked indirectly via the EXIT trap's cleanup()
smoke_cleanup() {
  :
}
# Default post-health verification hook: a .conf overrides it for assertions
# that need the running healthy container (see the header). Same
# define-before-source shape as smoke_cleanup.
# shellcheck disable=SC2329  # invoked only when health is reached
smoke_verify() {
  :
}
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
    # The HEALTHCHECK's own output is the direct evidence for an "unhealthy"
    # verdict; a shell-less image often logs nothing about its probe.
    printf '%s\n' "--- healthcheck probe log ---" >&2
    docker inspect --format '{{ if .State.Health }}{{ range .State.Health.Log }}exit={{ .ExitCode }}: {{ .Output }}{{ end }}{{ end }}' "$NAME" 2>/dev/null >&2 || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  # The app's own fixture teardown, after the container that consumed it is gone.
  # Never allowed to change the run's verdict.
  smoke_cleanup || true
}
# An EXIT trap runs on SIGINT but NOT on SIGTERM/SIGHUP (measured under dash and sh),
# so convert those into a normal exit and let the one EXIT trap below remove the
# container and call smoke_cleanup exactly once.
trap 'exit 143' TERM HUP
trap cleanup EXIT

# SMOKE_RUN_ARGS is intentionally word-split (simple test args, no spaces).
# shellcheck disable=SC2086
docker run -d --name "$NAME" $SMOKE_RUN_ARGS "$IMG" >/dev/null

start=$(date +%s)
deadline=$((start + TIMEOUT))
# Pre-set so both post-loop verdicts have a state to name: a SMOKE_TIMEOUT of 0
# skips the loop body entirely.
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
      # An app-specific post-start assertion (SMOKE_LOG_PATTERN) keeps waiting
      # inside the same deadline: healthy alone does not prove a surface the
      # HEALTHCHECK deliberately does not cover actually came up.
      if [ -n "$SMOKE_LOG_PATTERN" ] && ! docker logs "$NAME" 2>&1 | grep -qF -- "$SMOKE_LOG_PATTERN"; then
        sleep 1
        continue
      fi
      # App-specific verification against the running container, once, after
      # health. A failure is a real verdict, not a retry: health said up, so
      # anything smoke_verify finds missing is missing from the image.
      # shellcheck disable=SC2034  # consumed by the sourced .conf's smoke_verify
      SMOKE_CONTAINER="$NAME"
      # Run the hook in a child shell that CARRIES errexit, capturing its status
      # outside any condition context. `if ! smoke_verify` puts the whole hook body
      # in an errexit-ignored context (verified in dash and bash), so a hook whose
      # early probe fails but whose last command succeeds would PASS. The subshell
      # also contains a hook that installs its own EXIT trap, which in-process would
      # REPLACE the harness's cleanup trap and leak the container.
      set +e
      (
        set -e
        smoke_verify
      )
      verify_rc=$?
      set -e
      if [ "$verify_rc" -ne 0 ]; then
        printf 'FAIL: %s smoke_verify failed (see output above)\n' "$APP" >&2
        exit 1
      fi
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
if [ -n "$SMOKE_LOG_PATTERN" ] && [ "$status" = healthy ]; then
  printf 'FAIL: %s became healthy but never logged "%s" within %ss\n' "$APP" "$SMOKE_LOG_PATTERN" "$TIMEOUT" >&2
  exit 1
fi
printf 'FAIL: %s did not become healthy within %ss (last status: %s)\n' "$APP" "$TIMEOUT" "$status" >&2
exit 1
