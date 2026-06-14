#!/bin/sh
# Runtime image smoke test for plex-exporter. Invoked by the central CI docker
# job:  sh tests/image-smoke.sh <image-ref>
#
# Starts the assembled image and waits for the container's own HEALTHCHECK
# (the distroless `plex-exporter health` file-marker probe) to report
# "healthy" — proving the binary runs in the distroless image, binds its
# /metrics HTTP server, and the health probe works. A dummy PLEX_URL/token is
# supplied so config parsing succeeds; the exporter serves /metrics regardless
# of whether Plex is reachable (scrape errors surface as metrics, not a crash).
set -eu

IMG="${1:?usage: image-smoke.sh <image-ref>}"
NAME="smoke-plex-exporter-$$"
TIMEOUT=90

# shellcheck disable=SC2329  # invoked indirectly via trap
cleanup() {
	echo "--- container logs (tail) ---"
	docker logs "$NAME" 2>&1 | tail -40 || true
	docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker run -d --name "$NAME" \
	-e PLEX_URL=http://127.0.0.1:1 \
	-e PLEX_TOKEN=smoke-token \
	"$IMG" >/dev/null

i=0
status=starting
while [ "$i" -lt "$TIMEOUT" ]; do
	status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2>/dev/null || echo gone)
	case "$status" in
	healthy) echo "plex-exporter image smoke: ok (healthy after ${i}s)"; exit 0 ;;
	unhealthy) echo "FAIL: plex-exporter reported unhealthy"; exit 1 ;;
	no-healthcheck) echo "FAIL: image has no HEALTHCHECK to assert against"; exit 1 ;;
	gone) echo "FAIL: plex-exporter container exited early"; exit 1 ;;
	esac
	i=$((i + 1))
	sleep 1
done
echo "FAIL: plex-exporter did not become healthy within ${TIMEOUT}s (last status: $status)"
exit 1
