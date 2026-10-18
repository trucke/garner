#!/usr/bin/env bash
# Checks the working copy in place: formatting, vet, the generated database
# code and known vulnerabilities; the tests on macOS, and again on Linux in
# the Go image the container is built with; and builds for Linux x86. With jj, every file that is not ignored belongs to the
# working-copy commit, so the folder is what gets committed.
#
# When all of it passes, a git note on the working copy's tree records it;
# the production deploy requires one. After a push, jj's new empty change
# has the pushed commit's tree, so the pass counts for that commit too.
#
# Usage: mise run check
set -euo pipefail

[ $# -eq 0 ] || {
	echo "mise run check takes no arguments; it checks the working copy" >&2
	exit 2
}
root=$(git rev-parse --show-toplevel)
cd "$root"

fail() {
	printf '✗ %s\n' "$1" >&2
	exit 1
}
step() { printf '\n· %s\n' "$1"; }

# Passes go test's output on without a line per passing package.
quiet() { grep -v -E '^(ok|\?) ' || true; }

# The tree jj records for the working copy now.
tree() { git rev-parse "$(jj log -r @ --no-graph -T commit_id)^{tree}"; }

podman info >/dev/null 2>&1 || fail "Podman does not answer; start it with: podman machine start"
image=$(sed -n 's/^ARG GOLANG=//p' Dockerfile)
tags=$(sed -n 's/.*go build -tags \([^ ]*\) .*/\1/p' Dockerfile)
[ -n "$image" ] && [ -n "$tags" ] || fail "the Dockerfile names no Go image or build tags"

checked=$(tree)
printf 'Checking the working copy (%s)\n' "$(jj log -r @ --no-graph -T 'coalesce(description.first_line(), "no description")')"

export GOTOOLCHAIN=local

step "Formatting"
unformatted=$(gofmt -l cmd internal tools)
[ -z "$unformatted" ] || fail "not formatted: $unformatted"

step "Vet"
go vet ./... || fail "vet found problems"

step "Generated database code"
(cd internal/store && sqlc diff) || fail "internal/store/query does not match the SQL; run mise run generate"

# govulncheck reads the code and what it calls; it needs nothing built.
step "Known vulnerabilities, on macOS and in the server as the container image builds it"
govulncheck ./... || fail "known vulnerabilities reach Garner's code on macOS; see above"
GOOS=linux GOARCH=amd64 govulncheck -tags "$tags" ./... || fail "known vulnerabilities reach Garner's code on Linux; see above"

step "Tests on macOS"
go test ./... 2>&1 | quiet || fail "tests failed on macOS"

step "Tests on Linux ($(podman info --format '{{.Host.Arch}}'), tags $tags, as the container image builds)"
# The container is thrown away; it needs no SELinux separation, and
# disabling it leaves the labels of the mounted folders alone.
podman run --rm --security-opt label=disable \
	-v "$root:/src:ro" \
	-v garner-check-gomod:/go/pkg/mod -v garner-check-gocache:/root/.cache/go-build \
	-e CGO_ENABLED=0 -e GOFLAGS="-tags=$tags" -e GOTOOLCHAIN=local \
	-w /src "$image" \
	sh -c 'set -eo pipefail; apk add --no-cache -q libgcc; go vet ./...; go test ./... 2>&1 | { grep -v -E "^(ok|\?) " || true; }' ||
	fail "vet or tests failed on Linux"

step "Build for Linux x86: garnerd as the container image builds it, garner as a release"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags "$tags" -o /dev/null ./cmd/garnerd || fail "garnerd does not build for Linux x86"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/garner || fail "garner does not build for Linux x86"

[ "$(tree)" = "$checked" ] || fail "files changed while checking; run mise run check again"
git notes --ref=garner-check add -f \
	-m "Passed mise run check on $(hostname -s) at $(date -u +%Y-%m-%dT%H:%M:%SZ) with $(go env GOVERSION)" "$checked"
printf '\n✓ Passed; recorded on tree %s\n' "${checked:0:12}"
