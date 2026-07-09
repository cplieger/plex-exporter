#!/bin/sh
# Runtime image smoke test for plex-exporter. Invoked by the central CI docker
# job:  sh tests/image-smoke.sh <image-ref>
#
# Starts the assembled image and waits for the container's own HEALTHCHECK
# (the distroless `plex-exporter health` file-marker probe) to report
# "healthy" — proving the binary runs in the distroless image, binds its
# /metrics HTTP server, and the health probe works. A dummy PLEX_SERVER/token is
# supplied so config parsing succeeds; the exporter serves /metrics regardless
# of whether Plex is reachable (scrape errors surface as metrics, not a crash).
set -eu

IMG="${1:?usage: image-smoke.sh <image-ref>}"
NAME="smoke-plex-exporter-$$"
TIMEOUT=90 # must cover the image's healthcheck start-period + a few intervals

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

docker run -d --name "$NAME" \
  -e PLEX_SERVER=http://127.0.0.1:1 \
  -e PLEX_TOKEN=smoke-token \
  "$IMG" >/dev/null

i=0
status=starting
while [ "$i" -lt "$TIMEOUT" ]; do
  # Fail fast on an early exit: poll .State.Running before the health status so
  # a crash-boot is caught by its exit code (more debuggable than "unhealthy")
  # and the verdict never depends on what health a stopped container reports.
  if [ "$(docker inspect --format '{{ .State.Running }}' "$NAME" 2>/dev/null || echo missing)" != "true" ]; then
    ec=$(docker inspect --format '{{ .State.ExitCode }}' "$NAME" 2>/dev/null || echo '?')
    printf 'FAIL: plex-exporter container exited early (exit code %s)\n' "$ec" >&2
    exit 1
  fi
  status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2>/dev/null || echo gone)
  case "$status" in
    healthy)
      printf 'plex-exporter image smoke: ok (healthy after %ss)\n' "$i"
      exit 0
      ;;
    unhealthy)
      printf 'FAIL: plex-exporter reported unhealthy\n' >&2
      exit 1
      ;;
    no-healthcheck)
      printf 'FAIL: image has no HEALTHCHECK to assert against\n' >&2
      exit 1
      ;;
    gone)
      printf 'FAIL: plex-exporter container is gone\n' >&2
      exit 1
      ;;
  esac
  i=$((i + 1))
  sleep 1
done
printf 'FAIL: plex-exporter did not become healthy within %ss (last status: %s)\n' "$TIMEOUT" "$status" >&2
exit 1
