#!/usr/bin/env bash
set -euo pipefail

# L2 validation: parse a FlowCraft deployment through the real
# core/deploy assembly layer. The validator pins the FlowCraft core module
# versions declared in scripts/validator/go.mod, so it works standalone
# from any working directory.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
validator_dir="$script_dir/validator"
bin_dir="$(mktemp -d)"
trap 'rm -rf "$bin_dir"' EXIT
(
	cd "$validator_dir"
	GOWORK=off go build -o "$bin_dir/validate-config" .
)
# Run from the caller's working directory so relative paths behave
# exactly as the user wrote them.
exec "$bin_dir/validate-config" "$@"
