#!/usr/bin/env bash
set -euo pipefail

# Structural validation: load a FlowCraft deployment through the same code the
# host runs (`deploy.Loader`, `graph.Parse`), without building anything. The
# validator pins the core module version in scripts/validator/go.mod, so it works
# from any directory; run it from the one the deployment lives in, because a
# graph's `settings.path` and a layer's `path` resolve against the working
# directory.
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
