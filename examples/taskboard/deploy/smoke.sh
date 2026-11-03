#!/usr/bin/env bash
# Runs the task board behind a pinned nginx and checks streaming through it:
# first frame before close, live updates, heartbeats across the proxy's idle
# timeout, loss and reconnect, and graceful shutdown.
#
# Requires Docker and Linux host networking. Uses 127.0.0.1:18080 (proxy)
# and 127.0.0.1:18081 (app), or SMOKE_PROXY_PORT and SMOKE_APP_PORT, and
# refuses to start if either is already in use. Cleanup removes only the
# container and process this run created, identified by container ID and PID.
set -euo pipefail

image="nginx:1.30.6-alpine@sha256:0e52bd054126cdc040b1276c0af347aeb02a41b9cfe305d6094767ac30e97949"
proxy_port=${SMOKE_PROXY_PORT:-18080}
app_port=${SMOKE_APP_PORT:-18081}
root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
cd "$root"

die() {
	echo "smoke: $*" >&2
	exit 1
}

for tool in docker curl go; do
	command -v "$tool" >/dev/null || die "needs $tool"
done

# in_use reports whether something already listens on the port. curl exits 7
# only when the connection is refused.
in_use() {
	local rc=0
	curl -s -o /dev/null --max-time 2 "http://127.0.0.1:$1/" || rc=$?
	[ "$rc" -ne 7 ]
}

responds() {
	curl -fs -o /dev/null --max-time 2 "http://127.0.0.1:$1/"
}

for port in "$proxy_port" "$app_port"; do
	if in_use "$port"; then
		die "127.0.0.1:$port is already in use; stop that service or set SMOKE_PROXY_PORT/SMOKE_APP_PORT"
	fi
done

work=$(mktemp -d)
cidfile="$work/container.id"
app_pid=""
# shellcheck disable=SC2329 # invoked by the EXIT trap
cleanup() {
	if [ -s "$cidfile" ]; then
		docker rm -f "$(cat "$cidfile")" >/dev/null 2>&1 || true
	fi
	if [ -n "$app_pid" ]; then
		kill "$app_pid" 2>/dev/null || true
		wait "$app_pid" 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT

go build -o "$work/taskboard" ./examples/taskboard
"$work/taskboard" -addr "127.0.0.1:$app_port" -heartbeat 1s -buffer 4 >"$work/app.log" 2>&1 &
app_pid=$!

for _ in $(seq 50); do
	responds "$app_port" && break
	kill -0 "$app_pid" 2>/dev/null || break
	sleep 0.2
done
if ! kill -0 "$app_pid" 2>/dev/null || ! responds "$app_port"; then
	cat "$work/app.log" >&2
	die "the task board did not start on 127.0.0.1:$app_port"
fi

sed -e "s/127.0.0.1:18081/127.0.0.1:$app_port/" -e "s/127.0.0.1:18080/127.0.0.1:$proxy_port/" \
	examples/taskboard/deploy/nginx.conf >"$work/nginx.conf"

# --cidfile records the ID as soon as the container exists, so cleanup can
# remove exactly this container even if starting it fails.
docker run -d --cidfile "$cidfile" --name "tavern-taskboard-smoke-$$" --network host \
	-v "$work/nginx.conf:/etc/nginx/nginx.conf:ro" "$image" >/dev/null ||
	die "could not start nginx"

for _ in $(seq 50); do
	responds "$proxy_port" && break
	sleep 0.2
done
if ! responds "$proxy_port"; then
	docker logs "$(cat "$cidfile")" >&2 || true
	die "nginx did not answer on 127.0.0.1:$proxy_port"
fi

status=0
TASKBOARD_PROXY_URL="http://127.0.0.1:$proxy_port" TASKBOARD_APP_PID=$app_pid \
	go test -count=1 -run TestThroughProxy -v ./examples/taskboard || status=$?
echo "--- app log"
cat "$work/app.log"
echo "--- nginx $(docker exec "$(cat "$cidfile")" nginx -v 2>&1)"
exit "$status"
