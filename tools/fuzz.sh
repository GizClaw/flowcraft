#!/usr/bin/env bash
# Run the fuzz targets for a fixed budget. CI uses the default as a smoke
# check; a developer extends FUZZTIME for a real hunt.
set -euo pipefail

cd "$(dirname "$0")/../core"

time="${FUZZTIME:-15s}"

# Core only: the targets whose packages moved out with the core rebuild run in
# their own modules — `./internal/a2awire` in agent, `./agent-graph` in agent,
# `./sandbox-remote/wire` in sandbox — and they rejoin this script there.
targets=(
	"./internal/decode FuzzToJSON"
	"./config FuzzLoad"
	"./internal/search FuzzSearch"
	"./policy FuzzFilesNormalized"
	"./sandbox FuzzNarrowIsMonotoneAndIdempotent"
	"./tool/middleware FuzzTruncateJSONResult"
)

for entry in "${targets[@]}"; do
	package="${entry% *}"
	function="${entry#* }"
	echo "==> ${package} ${function}"
	go test "${package}" -run '^$' -fuzz "^${function}\$" -fuzztime "${time}"
done
