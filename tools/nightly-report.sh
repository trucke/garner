#!/usr/bin/env bash
# Reports a failure of the nightly workflow as a GitHub issue labelled
# nightly: one open issue per title, and a comment on it for every later
# failure, so a bug that fails every night opens one issue.
#
# Usage: tools/nightly-report.sh TITLE BODY-FILE
# Needs GH_TOKEN with issues: write and GH_REPO, and GitHub's
# GITHUB_SERVER_URL, GITHUB_REPOSITORY, GITHUB_RUN_ID and GITHUB_SHA.
set -euo pipefail

title=$1
body=$2
label=nightly
commit="$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/commit/$GITHUB_SHA"
run="$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"

issue=$(mktemp)
trap 'rm -f "$issue"' EXIT
# shellcheck disable=SC2016 # the backticks are Markdown
{
	printf 'Commit [`%s`](%s), [workflow run](%s). Check out the commit before you replay:\n\n' \
		"${GITHUB_SHA:0:12}" "$commit" "$run"
	printf '```bash\ngit checkout %s\n```\n\n' "$GITHUB_SHA"
	cat "$body"
} >"$issue"

gh label create "$label" --color d93f0b --description "Found by the nightly workflow" --force >/dev/null
open=$(gh issue list --label "$label" --state open --limit 100 --json number,title |
	jq -r --arg title "$title" '.[] | select(.title == $title) | .number' | head -1)
if [ -n "$open" ]; then
	gh issue comment "$open" --body-file "$issue"
else
	gh issue create --title "$title" --label "$label" --body-file "$issue"
fi
