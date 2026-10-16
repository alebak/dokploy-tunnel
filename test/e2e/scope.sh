#!/usr/bin/env bash
#
# Decides whether a pull request runs the e2e job, and prints "run=true" or
# "run=false" for $GITHUB_OUTPUT. A path filter on the workflow trigger
# cannot also let the release-please PR through whatever it touches, so the
# decision is made here instead.
#
# Inputs (environment): BASE_SHA, HEAD_SHA and HEAD_REF of the pull request.

set -euo pipefail

: "${BASE_SHA:?}" "${HEAD_SHA:?}" "${HEAD_REF:?}"

# Paths whose changes can break what the e2e job proves: the companion and
# what it runs, the CLI's forward path, and the deployment it installs.
patterns=(
	'internal/companion/*'
	'internal/repeater/*'
	'internal/dockerproxy/*'
	'internal/docker/*'
	'internal/forward/*'
	'internal/cli/forward*.go'
	'internal/tunnel/*'
	'cmd/doktunnel-companion/*'
	'cmd/doktunnel-socket-proxy/*'
	'deploy/dokploy/*'
	'build/*'
	'.github/workflows/e2e.yml'
	'test/e2e/*'
)

decide() {
	echo "run=$1"
	echo "e2e: run=$1 ($2)" >&2
}

# The release PR ships whatever main holds: always prove it end to end.
if [[ $HEAD_REF == release-please--* ]]; then
	decide true "release-please pull request"
	exit 0
fi

while IFS= read -r file; do
	for pattern in "${patterns[@]}"; do
		# shellcheck disable=SC2053 # the right-hand side is a glob on purpose
		if [[ $file == $pattern ]]; then
			decide true "$file matches $pattern"
			exit 0
		fi
	done
done < <(git diff --name-only "$BASE_SHA...$HEAD_SHA")

decide false "no changed file matches the e2e paths"
