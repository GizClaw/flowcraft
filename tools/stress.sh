#!/usr/bin/env bash
# Stress one test until it fails or the iteration count is exhausted.
#
#	 tools/stress.sh ./agent TestAgentRun 200
set -euo pipefail
if [[ $# -lt 2 ]]; then
	echo "usage: tools/stress.sh <package> <test regex> [iterations]" >&2
	exit 2
fi

cd "$(dirname "$0")/../core"
package="$1"
run="$2"
iterations="${3:-200}"
binary="$(mktemp -d)/stress.test"

go test -c -o "${binary}" "${package}"
go run golang.org/x/tools/cmd/stress@latest -count "${iterations}" "${binary}" -test.run "^${run}$"
