#!/usr/bin/env bash
# Failure-path checks for smoke.sh using stub docker, go and curl commands.
# Nothing here touches real containers or other processes: every stub only
# records its arguments and changes files in a temporary state directory.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/smoke.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

bin="$tmp/bin"
mkdir -p "$bin"

# curl answers for a port when $STATE/up-<port> exists; otherwise the
# connection is refused (exit 7), like a free port.
cat >"$bin/curl" <<'STUB'
#!/usr/bin/env bash
url=${*: -1}
port=${url##*:}; port=${port%%/*}
[ -e "$STATE/up-$port" ] || exit 7
[ -e "$STATE/broken-$port" ] && exit 52
exit 0
STUB

# go build writes a fake app that marks its port as up (unless
# $STATE/app-crash exists) and sleeps until killed; go test exits with
# $STATE/test-status (default 0).
cat >"$bin/go" <<'STUB'
#!/usr/bin/env bash
echo "go $*" >>"$STATE/calls"
case "$1" in
build)
	[ -e "$STATE/build-fails" ] && exit 9
	while [ "$1" != "-o" ]; do shift; done
	cat >"$2" <<'APP'
#!/usr/bin/env bash
[ -e "$STATE/app-crash" ] && exit 3
port=${2##*:}
echo $$ >"$STATE/app.pid"
touch "$STATE/up-$port"
exec sleep 300
APP
	chmod +x "$2"
	;;
test) exit "$(cat "$STATE/test-status" 2>/dev/null || echo 0)" ;;
esac
STUB

# docker run writes a container ID to --cidfile unless $STATE/create-fails;
# with $STATE/start-fails it still writes the ID (the container exists) but
# fails. Otherwise it marks the proxy port as up.
cat >"$bin/docker" <<'STUB'
#!/usr/bin/env bash
echo "docker $*" >>"$STATE/calls"
case "$1" in
run)
	[ -e "$STATE/create-fails" ] && exit 125
	while [ "$1" != "--cidfile" ]; do shift; done
	id="cid-$RANDOM$RANDOM"
	echo "$id" >"$2"
	echo "created $id" >>"$STATE/calls"
	[ -e "$STATE/start-fails" ] && exit 125
	[ -e "$STATE/proxy-silent" ] || touch "$STATE/up-${SMOKE_PROXY_PORT:-18080}"
	;;
exec) echo "nginx version: stub" ;;
esac
exit 0
STUB
chmod +x "$bin"/*

fail=0
check() {
	if eval "$2"; then echo "ok   $1"; else echo "FAIL $1"; fail=1; fi
}

# run_case <name> <expected exit: 0|nonzero> [state files...]
run_case() {
	local name=$1 want=$2
	shift 2
	STATE="$tmp/$name"
	mkdir -p "$STATE"
	: >"$STATE/calls"
	for f in "$@"; do touch "$STATE/$f"; done
	local rc=0
	STATE=$STATE PATH="$bin:$PATH" "$script" >"$STATE/out" 2>&1 || rc=$?
	if [ "$want" = 0 ]; then
		check "$name: succeeds" "[ $rc -eq 0 ]"
	else
		check "$name: fails" "[ $rc -ne 0 ]"
	fi
	check "$name: never removes a container by name" "! grep -q 'docker rm -f tavern' '$STATE/calls'"
	if [ -e "$STATE/app.pid" ]; then
		check "$name: stops the app it started" "! kill -0 $(cat "$STATE/app.pid") 2>/dev/null"
	fi
}

# Both helpers run inside check's eval.
# shellcheck disable=SC2329
removed() { grep -c '^docker rm -f ' "$STATE/calls" || true; }
# owned_removal: exactly one removal, of the container this run created.
# shellcheck disable=SC2329
owned_removal() {
	[ "$(removed)" -eq 1 ] &&
		[ "$(grep '^docker rm -f ' "$STATE/calls" | awk '{print $4}')" = "$(awk '/^created /{print $2}' "$STATE/calls")" ]
}

run_case proxy-port-in-use nonzero up-18080
check "proxy-port-in-use: stops before building or starting anything" "[ ! -s '$STATE/calls' ]"
check "proxy-port-in-use: explains the collision" "grep -q '127.0.0.1:18080 is already in use' '$STATE/out'"
check "proxy-port-in-use: leaves the existing service alone" "[ -e '$STATE/up-18080' ]"

run_case app-port-in-use nonzero up-18081 broken-18081
check "app-port-in-use: a non-HTTP listener also counts as in use" "grep -q '127.0.0.1:18081 is already in use' '$STATE/out'"
check "app-port-in-use: stops before starting anything" "[ ! -s '$STATE/calls' ]"

run_case build-fails nonzero build-fails
check "build-fails: does not touch docker" "! grep -q '^docker' '$STATE/calls'"

run_case app-crashes nonzero app-crash
check "app-crashes: reports the app did not start" "grep -q 'task board did not start' '$STATE/out'"
check "app-crashes: does not start nginx" "! grep -q '^docker' '$STATE/calls'"

run_case create-fails nonzero create-fails
check "create-fails: removes nothing it did not create" "[ \"\$(removed)\" -eq 0 ]"

run_case start-fails nonzero start-fails
check "start-fails: removes only the container it created, by ID" owned_removal

run_case proxy-silent nonzero proxy-silent
check "proxy-silent: reports nginx did not answer" "grep -q 'nginx did not answer' '$STATE/out'"
check "proxy-silent: removes only the container it created, by ID" owned_removal

run_case success 0
check "success: ran the proxy test" "grep -q '^go test .*TestThroughProxy' '$STATE/calls'"
check "success: removes only the container it created, by ID" owned_removal

exit "$fail"
