#!/usr/bin/env bash
# Tag and publish a patch release for one validated commit.
#
#   release.sh <sha>
#
# The tag always points at <sha>. Rerunning for a commit that already has a
# release tag reuses that tag instead of allocating a new version, and a
# commit older than the newest release is skipped. With DRY_RUN=1 the
# commands that change the remote are printed instead of run.
set -euo pipefail

sha=$(git rev-parse --verify "${1:?usage: release.sh <sha>}^{commit}")
tag_pattern='v[0-9]*.[0-9]*.[0-9]*'

run() {
	if [ "${DRY_RUN:-0}" = 1 ]; then
		echo "+ $*"
	else
		"$@"
	fi
}

latest_tag() {
	git tag --list "$tag_pattern" --sort=-v:refname "$@" | head -n 1
}

tag=$(latest_tag --points-at "$sha")
if [ -n "$tag" ]; then
	echo "$sha is already tagged $tag"
else
	latest=$(latest_tag)
	if [ -n "$latest" ] && ! git merge-base --is-ancestor "$latest" "$sha"; then
		echo "$sha does not contain the latest release $latest; not releasing"
		exit 0
	fi
	IFS=. read -r major minor patch <<<"${latest#v}"
	tag="v${major:-0}.${minor:-0}.$((${patch:-0} + 1))"
	echo "tagging $sha as $tag"
	run git tag -a "$tag" "$sha" -m "Release $tag"
	run git push origin "refs/tags/$tag"
fi

if gh release view "$tag" >/dev/null 2>&1; then
	echo "release $tag already exists"
	exit 0
fi

notes=$(mktemp)
trap 'rm -f "$notes"' EXIT
previous=$(git tag --list "$tag_pattern" --sort=-v:refname --merged "$sha" | grep -vx "$tag" | head -n 1 || true)
{
	echo "## Changes"
	if [ -n "$previous" ]; then
		git log "$previous..$sha" --pretty=format:"- %s"
	else
		git log "$sha" --pretty=format:"- %s"
	fi
	echo
	echo
	echo "Commit: $sha"
	echo "Go version: $(git show "$sha:go.mod" | awk '$1 == "go" { print $2 }')"
} >"$notes"
if [ "${DRY_RUN:-0}" = 1 ]; then
	cat "$notes"
fi
run gh release create "$tag" --verify-tag --title "$tag" --notes-file "$notes"
