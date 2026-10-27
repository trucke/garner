#!/usr/bin/env bash
# Releases a commit of main as a version: tags it, pushes the tag, and
# follows the GitHub workflow that builds the server image, the terminal
# interface and the release. The commit is main's newest unless named; it
# must be on main on GitHub and newer than the last published release.
# Every push passed `mise run check`, so what is on main is checked.
#
# Usage: mise run release vX.Y.Z [revision]
set -euo pipefail

fail() {
	printf '✗ %s\n' "$1" >&2
	exit 1
}

version=${1:-}
rev=${2:-main@origin}
# Plain versions only: without pre-releases, sort -V orders them as
# semantic versioning does.
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] && [ $# -le 2 ] ||
	fail "give the version and, optionally, the commit: mise run release v0.2.0 [revision]"
root=$(git rev-parse --show-toplevel)
cd "$root"

jj git fetch --quiet
commits=$(jj log -r "$rev" --no-graph -T 'commit_id ++ "\n"' 2>/dev/null) || fail "$rev names no commit"
[ "$(printf '%s\n' "$commits" | grep -c .)" = 1 ] || fail "$rev is not exactly one commit"
commit=$commits
[ -n "$(jj log -r "$commit & ::main@origin" --no-graph -T commit_id)" ] ||
	fail "${commit:0:12} is not on main on GitHub; push it first"

git fetch --quiet --tags origin
git rev-parse --verify --quiet "refs/tags/$version" >/dev/null && fail "$version exists already"
latest=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' | sort -V | tail -1)
[ -z "$latest" ] || [ "$(printf '%s\n%s\n' "$latest" "$version" | sort -V | tail -1)" = "$version" ] ||
	fail "$version is not newer than $latest"
# A failed release keeps its tag, so its commit may go out again under the
# next version; only a published release counts.
published=$(gh release list --limit 100 --exclude-drafts --json tagName --jq '.[].tagName') ||
	fail "could not list the releases on GitHub"
released=$(printf '%s\n' "$published" | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)
[ -z "$released" ] || ! git merge-base --is-ancestor "$commit" "$released" ||
	fail "${commit:0:12} is already part of $released"

printf 'Releasing %s (%s) as %s\n' "${commit:0:12}" "$(jj log -r "$commit" --no-graph -T 'description.first_line()')" "$version"
git tag -a "$version" -m "Garner $version" "$commit"
git push --quiet origin "refs/tags/$version" || {
	git tag -d "$version" >/dev/null
	fail "could not push the tag $version; nothing was released"
}

# The tag push starts the release workflow; wait until GitHub lists it.
run=""
for _ in $(seq 1 30); do
	run=$(gh run list --workflow release.yml --limit 20 --json databaseId,headBranch \
		--jq ".[] | select(.headBranch == \"$version\") | .databaseId" | head -1)
	[ -n "$run" ] && break
	sleep 2
done
[ -n "$run" ] || fail "GitHub started no release workflow for $version; see the repository's Actions page"
# Version tags are never deleted, so a failed version stays taken.
next=$(printf '%s\n' "$version" | awk -F. '{ printf "%s.%s.%d", $1, $2, $3 + 1 }')
gh run watch "$run" --exit-status --interval 10 >/dev/null ||
	fail "the release workflow failed: gh run view $run --log-failed. $version stays taken; fix the cause and release the next patch version: mise run release $next"

# shellcheck disable=SC2016 # the backticks are literal, as the notes write them
image=$(gh release view "$version" --json body --jq .body | sed -n 's/^Server image: `\(.*\)`$/\1/p')
printf '\n✓ Released %s\n  %s\n  Server image: %s\n' "$version" "$(gh release view "$version" --json url --jq .url)" "$image"
