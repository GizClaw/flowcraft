#!/usr/bin/env bash
# Run the fuzz targets for a fixed budget. CI uses the default as a smoke
# check; a developer extends FUZZTIME for a real hunt.
set -euo pipefail

cd "$(dirname "$0")/.."

time="${FUZZTIME:-15s}"

# One entry per target, as "module:package function": the checkout is four
# modules and a target runs in the module that owns its package.
targets=(
	"core:./internal/decode FuzzToJSON"
	"core:./config FuzzLoad"
	"core:./internal/search FuzzSearch"
	"core:./policy FuzzFilesNormalized"
	"core:./sandbox FuzzNarrowIsMonotoneAndIdempotent"
	"core:./tool/middleware FuzzTruncateJSONResult"
	"agent:./graph FuzzStateEvent"
	"agent:./graph/script FuzzDecodeConfig"
	"agent:./a2a/wire FuzzFromMessage"
)

for entry in "${targets[@]}"; do
	module="${entry%%:*}"
	rest="${entry#*:}"
	package="${rest% *}"
	function="${rest#* }"
	echo "==> ${module} ${package} ${function}"
	( cd "${module}" && go test "${package}" -run '^$' -fuzz "^${function}\$" -fuzztime "${time}" )
done
