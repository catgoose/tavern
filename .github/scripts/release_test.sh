#!/usr/bin/env bash
# Dry-run checks for release.sh against a throwaway repository and a gh stub.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/release.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/bin" "$work/repo"
# The stub reports every release as missing unless listed in $work/releases.
cat >"$work/bin/gh" <<STUB
#!/usr/bin/env bash
if [ "\$1 \$2" = "release view" ]; then grep -qx "\$3" "$work/releases" 2>/dev/null; exit; fi
echo "unexpected gh call: \$*" >&2; exit 1
STUB
chmod +x "$work/bin/gh"
export PATH="$work/bin:$PATH" DRY_RUN=1

cd "$work/repo"
git init -q
git config user.email test@example.com
git config user.name test
commit() { echo "$1" >>file; git add file; git commit -qm "$1"; git rev-parse HEAD; }
printf 'module example\n\ngo 1.27.3\n' >go.mod
git add go.mod

fail=0
expect() {
	local name=$1 pattern=$2 output=$3
	if grep -q -- "$pattern" <<<"$output"; then
		echo "ok   $name"
	else
		echo "FAIL $name: missing '$pattern' in:"; echo "$output"; fail=1
	fi
}

first=$(commit "first")
out=$("$script" "$first")
expect "first release is v0.0.1" "+ git tag -a v0.0.1 $first" "$out"
expect "release notes name the commit" "Commit: $first" "$out"
expect "release notes carry the Go version" "Go version: 1.27.3" "$out"

git tag v0.4.7 "$first"
second=$(commit "second")
out=$("$script" "$second")
expect "next patch after the latest tag" "+ git tag -a v0.4.8 $second" "$out"
expect "notes list commits since the previous tag" "- second" "$out"
expect "release is created from the existing tag" "+ gh release create v0.4.8 --verify-tag" "$out"

git tag v0.4.8 "$second"
out=$("$script" "$second")
expect "rerun reuses the commit's tag" "already tagged v0.4.8" "$out"
if grep -q "git tag -a" <<<"$out"; then echo "FAIL rerun allocated a new tag"; fail=1; fi

echo v0.4.8 >"$work/releases"
out=$("$script" "$second")
expect "rerun after publishing does nothing" "release v0.4.8 already exists" "$out"

out=$("$script" "$first")
expect "an older commit is tagged already and not re-released" "already tagged v0.4.7" "$out"

git checkout -q -b side "$first"
side=$(commit "side")
out=$("$script" "$side")
expect "a commit without the latest release is skipped" "does not contain the latest release v0.4.8" "$out"

exit "$fail"
