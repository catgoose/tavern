#!/usr/bin/env bash
# Runs the task board behind a pinned nginx and checks streaming through it:
# first frame before close, live updates, heartbeats across the proxy's idle
# timeout, loss and reconnect, and graceful shutdown.
#
# Requires Docker and Linux host networking. Uses 127.0.0.1:18080 (proxy)
# and 127.0.0.1:18081 (app) and removes only what it starts.
set -euo pipefail

image="nginx:1.30.6-alpine@sha256:0e52bd054126cdc040b1276c0af347aeb02a41b9cfe305d6094767ac30e97949"
name="tavern-taskboard-smoke"
root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
cd "$root"

command -v docker >/dev/null || { echo "smoke.sh needs docker" >&2; exit 1; }

work=$(mktemp -d)
app_pid=""
# shellcheck disable=SC2329 # invoked by the EXIT trap
cleanup() {
	docker rm -f "$name" >/dev/null 2>&1 || true
	if [ -n "$app_pid" ]; then kill "$app_pid" 2>/dev/null || true; wait "$app_pid" 2>/dev/null || true; fi
	rm -rf "$work"
}
trap cleanup EXIT

go build -o "$work/taskboard" ./examples/taskboard
"$work/taskboard" -addr 127.0.0.1:18081 -heartbeat 1s -buffer 4 >"$work/app.log" 2>&1 &
app_pid=$!

docker run -d --name "$name" --network host \
	-v "$root/examples/taskboard/deploy/nginx.conf:/etc/nginx/nginx.conf:ro" \
	"$image" >/dev/null

for _ in $(seq 50); do
	if curl -fs -o /dev/null http://127.0.0.1:18080/; then break; fi
	sleep 0.2
done
curl -fsS -o /dev/null http://127.0.0.1:18080/ || { docker logs "$name"; cat "$work/app.log"; exit 1; }

status=0
TASKBOARD_PROXY_URL=http://127.0.0.1:18080 TASKBOARD_APP_PID=$app_pid \
	go test -count=1 -run TestThroughProxy -v ./examples/taskboard || status=$?
echo "--- app log"; cat "$work/app.log"
echo "--- nginx $(docker exec "$name" nginx -v 2>&1)"
exit "$status"
