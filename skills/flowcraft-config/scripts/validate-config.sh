#!/usr/bin/env bash
set -euo pipefail

# L2 validation: dry-build a FlowCraft deployment through the real
# sdkx/deploy assembly layer. Run from anywhere in a FlowCraft checkout,
# or set FLOWCRAFT_ROOT when the skill is installed elsewhere.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

find_root() {
	local dir="$1"
	while [[ "$dir" != "/" ]]; do
		if [[ -f "$dir/go.work" && -d "$dir/sdk" && -d "$dir/sdkx" ]]; then
			echo "$dir"
			return 0
		fi
		dir="$(dirname "$dir")"
	done
	return 1
}

repo_root=""
# When this script still lives inside the FlowCraft repo, use that checkout.
if [[ -f "$script_dir/../../../go.work" && -d "$script_dir/../../../sdk" ]]; then
	repo_root="$(cd "$script_dir/../../.." && pwd)"
fi
# Installed copies fall back to FLOWCRAFT_ROOT, then to the caller's
# working directory ancestors.
if [[ -z "$repo_root" && -n "${FLOWCRAFT_ROOT:-}" && -f "$FLOWCRAFT_ROOT/go.work" && -d "$FLOWCRAFT_ROOT/sdk" ]]; then
	repo_root="$FLOWCRAFT_ROOT"
fi
if [[ -z "$repo_root" ]]; then
	repo_root="$(find_root "$(pwd)" || true)"
fi
if [[ -z "$repo_root" ]]; then
	echo "validate-config: cannot find the FlowCraft checkout (go.work + sdk/). Run inside the repo or set FLOWCRAFT_ROOT." >&2
	exit 2
fi

validator_dir="$repo_root/skills/flowcraft-config/scripts/validator"
if [[ ! -f "$validator_dir/go.mod" ]]; then
	echo "validate-config: $repo_root does not contain skills/flowcraft-config; fetch the latest main." >&2
	exit 2
fi

bin_dir="$(mktemp -d)"
trap 'rm -rf "$bin_dir"' EXIT
(
	cd "$validator_dir"
	GOWORK=off go build -o "$bin_dir/validate-config" .
)
# Run from the caller's working directory so relative paths behave
# exactly as the user wrote them.
exec "$bin_dir/validate-config" "$@"
