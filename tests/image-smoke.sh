#!/bin/sh
# Runtime image smoke-test harness: start the assembled image, wait for the
# container's own HEALTHCHECK to report healthy, fail fast on an early exit, dump the
# container log tail only on failure. Per-app knobs and hooks come from
# tests/image-smoke.conf beside this script; smoke-tests.md "Pattern B" documents
# every knob, every hook and what each tier proves.
# CANONICAL COPY in cplieger/ci (configs/image-smoke.sh), synced to each enrolled
# app's tests/image-smoke.sh: edit it there, never here.
set -eu

IMG="${1:?usage: image-smoke.sh <image-ref>}"

# Exposed to the .conf as $SMOKE_DIR so it can bind-mount a committed fixture dir:
# `docker -v` requires an absolute source path.
SMOKE_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

# Per-app config lives beside this script (repo-local, NOT synced). Pre-set the
# knobs so `set -u` is safe and a repo with no .conf still runs with defaults.
SMOKE_APP_NAME=""
SMOKE_TIMEOUT=""
SMOKE_RUN_ARGS=""
SMOKE_LOG_PATTERN=""
SMOKE_LICENSE_TREE=""
# A .conf that creates host state overrides this. Defined BEFORE the source so the
# EXIT trap can always call it.
# shellcheck disable=SC2329  # invoked indirectly via the EXIT trap's cleanup()
smoke_cleanup() {
  :
}
# A .conf overrides this for assertions needing the running healthy container. Same
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
case "$SMOKE_LICENSE_TREE" in
  '' | 0 | 1) ;;
  *)
    printf 'FAIL: SMOKE_LICENSE_TREE must be 1, 0 or unset, got "%s"\n' "$SMOKE_LICENSE_TREE" >&2
    exit 1
    ;;
esac
NAME="smoke-${APP}-$$"

# shellcheck disable=SC2317,SC2329  # invoked indirectly via trap
cleanup() {
  code=$?
  if [ "$code" -ne 0 ]; then
    printf '%s\n' "--- container logs (tail) ---" >&2
    docker logs "$NAME" 2>&1 | tail -40 >&2 || true
    # A shell-less image often logs nothing about its probe, so the HEALTHCHECK's own
    # output is the only evidence for an "unhealthy" verdict.
    printf '%s\n' "--- healthcheck probe log ---" >&2
    docker inspect --format '{{ if .State.Health }}{{ range .State.Health.Log }}exit={{ .ExitCode }}: {{ .Output }}{{ end }}{{ end }}' "$NAME" >&2 2>/dev/null || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  # Fixture teardown, after the container that consumed it is gone; never allowed to
  # change the run's verdict.
  smoke_cleanup || true
}
# An EXIT trap runs on SIGINT but NOT on SIGTERM/SIGHUP (measured under dash and sh),
# so convert those into a normal exit and let the one EXIT trap do the teardown once.
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
  # Poll .State.Running BEFORE health: a crash-boot is then caught by its exit code,
  # and the verdict never depends on what health a stopped container reports.
  if [ "$(docker inspect --format '{{ .State.Running }}' "$NAME" 2>/dev/null || echo missing)" != "true" ]; then
    ec=$(docker inspect --format '{{ .State.ExitCode }}' "$NAME" 2>/dev/null || echo '?')
    printf 'FAIL: %s container exited early (exit code %s)\n' "$APP" "$ec" >&2
    exit 1
  fi
  status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2>/dev/null || echo gone)
  case "$status" in
    healthy)
      # SMOKE_LOG_PATTERN keeps waiting inside the SAME deadline: healthy alone does
      # not prove a surface the HEALTHCHECK deliberately does not cover came up.
      if [ -n "$SMOKE_LOG_PATTERN" ] && ! docker logs "$NAME" 2>&1 | grep -qF -- "$SMOKE_LOG_PATTERN"; then
        sleep 1
        continue
      fi
      # Read through `docker cp`, since a distroless image has no shell to exec.
      if [ "$SMOKE_LICENSE_TREE" = 1 ]; then
        tree=$(mktemp -d)
        # The daemon's own message is the diagnosis (a missing path reads differently
        # from a refused read), so it is kept, not swallowed.
        if ! cp_err=$(docker cp "$NAME:/usr/share/licenses" "$tree/" 2>&1 >/dev/null); then
          rm -rf "$tree"
          printf 'FAIL: %s image has no readable /usr/share/licenses tree: %s\n' "$APP" "$cp_err" >&2
          exit 1
        fi
        if [ ! -f "$tree/licenses/$APP/LICENSE" ]; then
          rm -rf "$tree"
          printf 'FAIL: %s image lacks /usr/share/licenses/%s/LICENSE\n' "$APP" "$APP" >&2
          exit 1
        fi
        components=$(find "$tree/licenses" -mindepth 1 -maxdepth 1 -type d ! -name "$APP" | wc -l)
        rm -rf "$tree"
        if [ "$components" -lt 1 ]; then
          printf 'FAIL: %s license tree holds only the image'\''s own files; no bundled component\n' "$APP" >&2
          exit 1
        fi
        printf '%s license tree: own LICENSE plus %s bundled component(s)\n' "$APP" "$components"
      fi
      # A failure here is a verdict, not a retry: health said up, so anything
      # smoke_verify finds missing is missing from the image.
      # shellcheck disable=SC2034  # consumed by the sourced .conf's smoke_verify
      SMOKE_CONTAINER="$NAME"
      # A child shell that CARRIES errexit, with its status captured outside any
      # condition context: `if ! smoke_verify` runs the whole hook body in an
      # errexit-ignored context (dash and bash), so a hook whose early probe fails
      # but whose last command succeeds would PASS. It also contains a hook whose
      # own EXIT trap would otherwise REPLACE ours and leak the container.
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
