#!/usr/bin/env bash
set -euo pipefail

# L2 validation: dry-build a FlowCraft deployment through the real
# sdkx/deploy assembly layer. Run from anywhere in the repo.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
validator_dir="$repo_root/skills/flowcraft-config/scripts/validator"
bin_dir="$(mktemp -d)"
trap 'rm -rf "$bin_dir"' EXIT
(
	cd "$validator_dir"
	GOWORK=off go build -o "$bin_dir/validate-config" .
)
# Run from the caller's working directory so relative paths behave
# exactly as the user wrote them.
exec "$bin_dir/validate-config" "$@"
