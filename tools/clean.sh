#!/usr/bin/env bash
# Removes what local work leaves behind: the build output in the
# repository and the dev stack's scratch folder, where copies of the dev
# data, captures and other one-off files from testing belong. The stack,
# its data and Garner's own backups stay.
#
# Usage: mise run clean
set -euo pipefail

cd "$(dirname "$0")/.."
# tools/devstack keeps the stack in ~/garner-dev.
scratch="$HOME/garner-dev/scratch"

removed=0
for p in bin dist "$scratch"; do
	[ -e "$p" ] || continue
	printf 'Removing %s (%s)\n' "$p" "$(du -sh -- "$p" | awk '{print $1}')"
	rm -rf -- "$p"
	removed=$((removed + 1))
done
[ "$removed" -gt 0 ] || echo "Nothing to remove."
