#!/usr/bin/env bash
# Conformance gate.
#
# The Go implementation is the oracle; the Rust port must reproduce its
# observable output byte for byte. This script builds both sides, runs them
# over the corpus, and diffs the two dumps. Any mismatch is a failure.
#
# Usage: rust/conformance/run.sh [--details N]
#
# The Go oracle lives in tools/conformance-oracle and builds with GOWORK=off,
# so a change to core's observable behavior breaks this gate rather than
# silently diverging.
#
# Only the adopted backend gates: CONFORMANCE_BACKEND (default saphyr-go).
# The rejected candidates still run into the same dump, so `--backend norway`
# reproduces the table in rust/conformance/README.md, and they are compared
# raw -- see the allowlist below. Those two backends live behind the probe's
# non-default `candidates` feature, which this script turns on for them (the
# manifest says why it is not default).
#
# Before the gate trusts its checkers, their own failure modes are exercised
# by tools/selftest.py: a gate that cannot fail is not a gate.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
conf="$root/rust/conformance"
out="${CONFORMANCE_OUT:-$conf/out}"
backend="${CONFORMANCE_BACKEND:-saphyr-go}"
# How many times the oracle runs over the corpus. More than one because a gate
# anchored on a non-reproducible oracle gates nothing; see tools/stability.py.
# 25 was calibrated against the measured ~12% minority share of a collapsing
# file: five runs miss it in roughly half of all invocations (measured 9/20),
# which would let the exact class of bug this gate was built to catch slip
# through. That class is now enumerated as evidence AND refused, so the runs no
# longer carry it -- they back the nondeterminism the gate cannot enumerate.
# One corpus run is ~0.2s, so the honest number is also the cheap one.
runs="${CONFORMANCE_ORACLE_RUNS:-25}"
# Runs spent confirming one listed exemption is still true. The check stops at
# the first disagreement, so a true exemption costs almost nothing here.
verify_runs="${CONFORMANCE_VERIFY_RUNS:-100}"

# Relative to the repo root on purpose: both dumps record these paths, so the
# diff keys line up and the output stays readable. Every path here is
# committed: a directory that only exists in a local checkout would make CI
# and local runs disagree about which files even exist.
corpus=(
  "rust/conformance/fixtures"
  "examples"
  "skills"
  "docs/guides"
  "docs/migrations"
  ".release"
  ".github"
)

mkdir -p "$out"

echo "==> selftest (checker failure modes)"
python3 "$conf/tools/selftest.py"

echo "==> oracle (Go)"
(cd "$root/tools/conformance-oracle" && GOWORK=off go build -o "$out/oracle" .)

echo "==> error contract"
# The committed dump is the error half of the contract: errdefs' tables and
# classifier samples, dumped from core, replayed by the port
# (crates/flowcraft-contract/tests/error_contract.rs). Regenerating it is a
# deliberate act -- the file moves only when core's classification moves, and
# when it does the port has to move with it.
"$out/oracle" -error-contract > "$out/error-contract.json"
if ! cmp -s "$out/error-contract.json" "$conf/error-contract.json"; then
  echo "rust/conformance/error-contract.json no longer matches core:" >&2
  # `|| true`: diff exits non-zero when it finds the difference we already
  # know about, and this file runs under `set -e -o pipefail` -- without it
  # the hint below would never print.
  diff "$conf/error-contract.json" "$out/error-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -error-contract) > rust/conformance/error-contract.json" >&2
  exit 1
fi

echo "==> message contract"
# The other half of the contract: the media DTOs of core/message, dumped the
# same way and replayed by the port
# (crates/flowcraft-contract/tests/message_contract.rs). Same rule as the
# error half -- the file moves only when core's media rules move.
"$out/oracle" -message-contract > "$out/message-contract.json"
if ! cmp -s "$out/message-contract.json" "$conf/message-contract.json"; then
  echo "rust/conformance/message-contract.json no longer matches core:" >&2
  diff "$conf/message-contract.json" "$out/message-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -message-contract) > rust/conformance/message-contract.json" >&2
  exit 1
fi

echo "==> event contract"
# The routing half of core/event: subjects, patterns, the matcher and the
# envelope wire, replayed by
# crates/flowcraft-event/tests/event_contract.rs. The bus itself is a runtime
# and lands with the slice that picks the port's concurrency model.
"$out/oracle" -event-contract > "$out/event-contract.json"
if ! cmp -s "$out/event-contract.json" "$conf/event-contract.json"; then
  echo "rust/conformance/event-contract.json no longer matches core:" >&2
  diff "$conf/event-contract.json" "$out/event-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -event-contract) > rust/conformance/event-contract.json" >&2
  exit 1
fi

echo "==> probe (Rust)"
case "$backend" in
  norway | ng)
    (cd "$root/rust" && cargo build --quiet -p yamlprobe --features candidates)
    ;;
  *)
    (cd "$root/rust" && cargo build --quiet -p yamlprobe)
    ;;
esac

cd "$root"
probe="$root/rust/target/debug/yamlprobe"
"$probe" "${corpus[@]}" > "$out/rust.jsonl"

# The port is checked for reproducibility too. The Go side can only be sampled
# (its randomization is inside the process), but a Rust map is seeded per
# process, so a second run is enough to catch an unordered collection leaking
# into the output -- in either language the gate has to be anchored on
# something reproducible.
"$probe" "${corpus[@]}" > "$out/rust.repeat.jsonl"
if ! cmp -s "$out/rust.jsonl" "$out/rust.repeat.jsonl"; then
  echo "probe output is not reproducible:" >&2
  diff "$out/rust.jsonl" "$out/rust.repeat.jsonl" | head -20 >&2
  exit 1
fi

echo "==> oracle stability"
python3 "$conf/tools/stability.py" \
  --allowlist "$conf/known-divergences.txt" --oracle "$out/oracle" \
  --runs "$runs" --verify-runs "$verify_runs" --dump-dir "$out" "${corpus[@]}"

echo "==> diff"
# `known-divergences.txt` records where the *adopted* port is allowed to
# disagree; a candidate is not that port, and its divergences are what running
# it is for. So only the adopted backend is diffed against the list: a
# candidate run reports every disagreement and exits 1 -- the answer being
# asked for, not a failure. (`${allow[@]+...}` because macOS ships bash 3.2,
# where expanding an empty array is an unbound variable under `set -u`.)
go_args=()
for run in $(seq "$runs"); do
  go_args+=(--go "$out/go.$run.jsonl")
done
allow=()
if [ "$backend" = "saphyr-go" ]; then
  allow=(--allowlist "$conf/known-divergences.txt")
fi
python3 "$conf/tools/diff.py" "${go_args[@]}" "$out/rust.jsonl" \
  --backend "$backend" ${allow[@]+"${allow[@]}"} "$@"

echo "===> error text"
python3 "$conf/tools/errortext.py" --go "$out/go.1.jsonl" "$out/rust.jsonl" \
  --backend "$backend" ${allow[@]+"${allow[@]}"} "$@"
