#!/usr/bin/env bash
# Run the fuzz targets for a fixed budget. CI uses the default as a smoke
# check; a developer extends FUZZTIME for a real hunt.
set -euo pipefail

cd "$(dirname "$0")/../core"

time="${FUZZTIME:-15s}"

targets=(
	"./internal/decode FuzzToJSON"
	"./agent-a2a/wire FuzzFromMessage"
	"./agent-graph FuzzStateEvent"
)

for entry in "${targets[@]}"; do
	package="${entry% *}"
	function="${entry#* }"
	echo "==> ${package} ${function}"
	go test "${package}" -run '^$' -fuzz "^${function}\$" -fuzztime "${time}"
done
