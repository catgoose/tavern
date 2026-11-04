#!/usr/bin/env bash
# Checks the task board's vendored browser assets.
#
#   vendor.sh check    compare the third-party files in static/ with vendor.sha256
#   vendor.sh upstream also confirm that the tavern-js file and license are the
#                      published release asset and license at the pinned tag
#   vendor.sh update   download the pinned tavern-js release into static/
#
# To move to another tavern-js release, change the pins below, run update,
# remove the old file, and record the new digests:
#   (cd examples/taskboard && sha256sum static/*.min.js static/LICENSE.* > vendor.sha256)
set -euo pipefail

TAVERN_JS_REPO=catgoose/tavern-js
TAVERN_JS_TAG=v0.0.24
TAVERN_JS_COMMIT=066f573f584ecd6eff0b576552dd2ea573d01bee
TAVERN_JS_FILE=tavern-0.0.24.min.js

dir="$(cd "$(dirname "$0")" && pwd)"
static="$dir/static"

die() {
	echo "vendor: $*" >&2
	exit 1
}

check() {
	(cd "$dir" && sha256sum --quiet -c vendor.sha256) || die "static/ does not match vendor.sha256"
	local listed actual
	listed=$(awk '{print $2}' "$dir/vendor.sha256" | sort)
	actual=$(cd "$dir" && printf '%s\n' static/*.min.js static/LICENSE.* | sort)
	[ "$listed" = "$actual" ] || die "third-party files in static/ and vendor.sha256 differ"
	echo "vendor: static/ matches vendor.sha256"
}

# fetch downloads the pinned release asset and license into $1.
fetch() {
	local commit
	commit=$(git ls-remote "https://github.com/$TAVERN_JS_REPO" "refs/tags/$TAVERN_JS_TAG^{}" | cut -f1)
	[ "$commit" = "$TAVERN_JS_COMMIT" ] || die "$TAVERN_JS_TAG resolves to ${commit:-nothing}, expected $TAVERN_JS_COMMIT"
	curl -fsSL -o "$1/$TAVERN_JS_FILE" \
		"https://github.com/$TAVERN_JS_REPO/releases/download/$TAVERN_JS_TAG/tavern.min.js"
	curl -fsSL -o "$1/LICENSE.tavern-js" \
		"https://raw.githubusercontent.com/$TAVERN_JS_REPO/$TAVERN_JS_COMMIT/LICENSE"
}

upstream() {
	check
	local tmp
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' RETURN
	fetch "$tmp"
	for f in "$TAVERN_JS_FILE" LICENSE.tavern-js; do
		cmp -s "$tmp/$f" "$static/$f" || die "static/$f differs from $TAVERN_JS_REPO $TAVERN_JS_TAG"
	done
	echo "vendor: tavern-js matches $TAVERN_JS_REPO $TAVERN_JS_TAG ($TAVERN_JS_COMMIT)"
}

case "${1:-}" in
check) check ;;
upstream) upstream ;;
update) fetch "$static" ;;
*) die "usage: vendor.sh check|upstream|update" ;;
esac
