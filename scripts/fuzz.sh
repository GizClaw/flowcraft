#!/usr/bin/env bash
# Run the fuzz targets for a fixed budget.
#
#   scripts/fuzz.sh                  # the CI smoke budget (FUZZTIME, default 15s)
#   FUZZ_SEED=5m scripts/fuzz.sh     # a real hunt
#   FUZZ_COPY=8 scripts/fuzz.sh      # ...and keep what it found
#
# # The corpus is the point
#
# `go test -fuzz` writes a failing input to <pkg>/testdata/fuzz/<Name>/, and
# `make test` (an ordinary `go test`) replays every file in that directory as a
# test case. So a committed corpus is a permanent regression suite that costs the
# gate nothing extra. Covering-but-passing inputs are *not* written there: the
# engine keeps them in $GOCACHE/fuzz, which is per-machine and thrown away. That
# is what FUZZ_COPY copies, and it is the only reason to run a long fuzz locally:
# a corpus nobody commits is a hunt that starts from zero every night.
#
# Keep the copy small. A seed corpus is a set of regression cases, not a cache
# dump; FUZZ_COPY=8 keeps the newest few per target.
set -euo pipefail

cd "$(dirname "$0")/.."

time="${FUZZ_SEED:-${FUZZTIME:-15s}}"
copy="${FUZZ_COPY:-0}"

# One entry per target, as "module:package function": the checkout is five
# modules and a target runs in the module that owns its package.
#
# The rule for adding one: every package that parses bytes it did not write gets
# a target here. `message` and `skill` are documents and wire payloads from a
# provider, a checkpoint or a peer; `deploy` is a deployment document, and the
# strict per-implementation decode it drives is not covered by FuzzLoad, which
# stops at the layer merge.
targets=(
	"core:./internal/decode FuzzToJSON"
	"core:./config FuzzLoad"
	"core:./deploy FuzzProcessDocument"
	"core:./internal/search FuzzSearch"
	"core:./message FuzzContentRoundTrip"
	"core:./message FuzzPartRoundTrip"
	"core:./policy FuzzFilesNormalized"
	"core:./sandbox FuzzNarrowIsMonotoneAndIdempotent"
	"core:./skill FuzzParse"
	"tool:./middleware FuzzTruncateJSONResult"
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
	if [[ "${copy}" != 0 ]]; then
		cache="$(go env GOCACHE)/fuzz/$(cd "${module}" && go list "${package}")/${function}"
		destination="${module}/${package#./}/testdata/fuzz/${function}"
		mkdir -p "${destination}"
		# The cache is a set of hash names; the newest files are the ones the last
		# run found new coverage with.
		found=0
		while IFS= read -r file; do
			cp -n "${file}" "${destination}/"
			found=$((found + 1))
		done < <(ls -t "${cache}" 2>/dev/null | head -n "${copy}" | while read -r name; do echo "${cache}/${name}"; done)
		echo "    kept ${found} input(s) in ${destination}"
	fi
done
