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

echo "==> bm25 contract"
# core/utils/bm25: the tokenizer's script decisions (every range boundary the
# port spells out), the scoring, and the refusals, replayed by
# crates/flowcraft-utils/tests/bm25_contract.rs. Same rule as the other halves
# -- the file moves only when core's behaviour moves.
"$out/oracle" -bm25-contract > "$out/bm25-contract.json"
if ! cmp -s "$out/bm25-contract.json" "$conf/bm25-contract.json"; then
  echo "rust/conformance/bm25-contract.json no longer matches core:" >&2
  diff "$conf/bm25-contract.json" "$out/bm25-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -bm25-contract) > rust/conformance/bm25-contract.json" >&2
  exit 1
fi

echo "==> pathsafe contract"
# core/utils/pathsafe: containment, the relative form, the reference guard and
# the lexical clean behind them, replayed by
# crates/flowcraft-utils/tests/pathsafe_contract.rs. The filesystem half
# (RealWithin/RealDir) depends on a tree, so it is tested where the tree is
# built rather than here.
"$out/oracle" -pathsafe-contract > "$out/pathsafe-contract.json"
if ! cmp -s "$out/pathsafe-contract.json" "$conf/pathsafe-contract.json"; then
  echo "rust/conformance/pathsafe-contract.json no longer matches core:" >&2
  diff "$conf/pathsafe-contract.json" "$out/pathsafe-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -pathsafe-contract) > rust/conformance/pathsafe-contract.json" >&2
  exit 1
fi

echo "==> filetype contract"
# core/utils/filetype: the WHATWG sniffer core leans on (every signature, mask
# and the 512-byte cutoff) and the ordered rule with a nameless payload, replayed
# by crates/flowcraft-utils/tests/filetype_contract.rs. Name-derived answers are
# host-dependent -- Go's table is builtin + the machine's mime.types -- so they
# are not in the file; the port's parsers are tested against fixture text.
"$out/oracle" -filetype-contract > "$out/filetype-contract.json"
if ! cmp -s "$out/filetype-contract.json" "$conf/filetype-contract.json"; then
  echo "rust/conformance/filetype-contract.json no longer matches core:" >&2
  diff "$conf/filetype-contract.json" "$out/filetype-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -filetype-contract) > rust/conformance/filetype-contract.json" >&2
  exit 1
fi

echo "==> net contract"
# core/utils/net: host patterns (domain-and-descendants, leading *. wildcards,
# IP literals, CIDR prefixes, IDNA on both sides), Set, and the Matcher's
# verdicts over NetPolicy rules, replayed by
# crates/flowcraft-utils/tests/net_contract.rs. The other half of the package
# -- the proxy and SOCKS5 listener -- is sockets, and lands in its own slice.
"$out/oracle" -net-contract > "$out/net-contract.json"
if ! cmp -s "$out/net-contract.json" "$conf/net-contract.json"; then
  echo "rust/conformance/net-contract.json no longer matches core:" >&2
  diff "$conf/net-contract.json" "$out/net-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -net-contract) > rust/conformance/net-contract.json" >&2
  exit 1
fi

echo "==> proxy contract"
# The proxy's policy half: the url.Parse grammar NetPolicy.Validate reads (with
# its error spelling), Validate's refusals, splitHostPort's two fallbacks, and
# Proxy.Decide's verdicts -- replayed by
# crates/flowcraft-utils/tests/proxy_contract.rs. Every row is answerable
# without a socket; the wire and the TLS engine are the next slice.
"$out/oracle" -proxy-contract > "$out/proxy-contract.json"
if ! cmp -s "$out/proxy-contract.json" "$conf/proxy-contract.json"; then
  echo "rust/conformance/proxy-contract.json no longer matches core:" >&2
  diff "$conf/proxy-contract.json" "$out/proxy-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -proxy-contract) > rust/conformance/proxy-contract.json" >&2
  exit 1
fi

echo "==> wire contract"
# The proxy's other half: what a SOCKS5 or HTTP CONNECT session answers, the
# audit records it produces, and what an upstream receives and returns --
# replayed by crates/flowcraft-utils/tests/wire_contract.rs. The rows are not
# pure functions of their input, so the dump is what core's sockets said: the
# bind address is masked (it reports an ephemeral local port) and nothing else
# is. Regenerating it is a deliberate act, like every other half.
"$out/oracle" -wire-contract > "$out/wire-contract.json"
if ! cmp -s "$out/wire-contract.json" "$conf/wire-contract.json"; then
  echo "rust/conformance/wire-contract.json no longer matches core:" >&2
  diff "$conf/wire-contract.json" "$out/wire-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -wire-contract) > rust/conformance/wire-contract.json" >&2
  exit 1
fi

echo "==> mitm contract"
# The layer under a terminated tunnel: the handshake the engine offers, what the
# decrypted traffic looks like to the hooks, and what the client and the target
# each get -- the handshake rows replayed by
# crates/flowcraft-utils/tests/mitm_contract.rs and the sessions by
# crates/flowcraft-utils/tests/mitm_sessions.rs. The rows are sockets, so the
# dump is what core's engine said; `date`, `connection` and the harness's own
# filler header are masked, and no port is recorded at all.
"$out/oracle" -mitm-contract > "$out/mitm-contract.json"
if ! cmp -s "$out/mitm-contract.json" "$conf/mitm-contract.json"; then
  echo "rust/conformance/mitm-contract.json no longer matches core:" >&2
  diff "$conf/mitm-contract.json" "$out/mitm-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -mitm-contract) > rust/conformance/mitm-contract.json" >&2
  exit 1
fi

echo "==> hooks contract"
# core/hooks, both halves: what a hooks.json loads as (the closed vocabulary,
# sorted keys, groups that survive only if a hook in them survives, one sentence
# per way of getting it wrong) and what firing an event does to the commands it
# names (one JSON object on stdin, the source's directory, the trust rule, a
# warning instead of an error), replayed by
# crates/flowcraft-hooks/tests/hooks_contract.rs. The rows are processes, so the
# harness's own directory is masked out of every string, and a matcher's or a
# JSON document's own error text is masked after the sentence that classifies
# it: Go and Rust word those differently, and pinning the wording would pin the
# library. Everything else -- the message, the event, the command, the output --
# is compared exactly.
"$out/oracle" -hooks-contract > "$out/hooks-contract.json"
if ! cmp -s "$out/hooks-contract.json" "$conf/hooks-contract.json"; then
  echo "rust/conformance/hooks-contract.json no longer matches core:" >&2
  diff "$conf/hooks-contract.json" "$out/hooks-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -hooks-contract) > rust/conformance/hooks-contract.json" >&2
  exit 1
fi

echo "==> skill contract"
# core/skill's parse half: every SKILL.md its parser is asked about, and the
# answer it gives -- the lenient half (a BOM, CRLF, extension fields, a name that
# disagrees with its directory, a scalar coerced to its spelling) and the strict
# half (one name shape, a required description, a cap on every string) as rows,
# replayed by crates/flowcraft-skill/tests/parse_contract.rs. The rows are files,
# so the harness's own directory is masked out of every string, an operating
# system's sentence after the verb and path core wrote, and a YAML or JSON
# library's own error text after the sentence that classifies it.
"$out/oracle" -skill-contract > "$out/skill-contract.json"
if ! cmp -s "$out/skill-contract.json" "$conf/skill-contract.json"; then
  echo "rust/conformance/skill-contract.json no longer matches core:" >&2
  diff "$conf/skill-contract.json" "$out/skill-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -skill-contract) > rust/conformance/skill-contract.json" >&2
  exit 1
fi

echo "==> skill registry contract"
# core/skill's registry half: the trees discovery is asked about and what it
# answers -- which symlinks are followed, which are refused, which directories
# are walked once, how duplicate names and repeated roots are listed -- plus
# what a Service ranks, resolves as a mention, and returns as a body, replayed
# by crates/flowcraft-skill/tests/registry_contract.rs. The rows are
# filesystems, so the harness's own directory is masked out of every string in
# both its given and its canonical form, an operating system's sentence after
# the verb and path core wrote, and a YAML library's error text after the
# sentence that classifies it. Scores are compared with the same 1e-12 relative
# tolerance `bm25-contract.json` uses: the two sides sum their postings in their
# own order, and any drift that could reorder two skills would still fail.
"$out/oracle" -skill-registry-contract > "$out/skill-registry-contract.json"
if ! cmp -s "$out/skill-registry-contract.json" "$conf/skill-registry-contract.json"; then
  echo "rust/conformance/skill-registry-contract.json no longer matches core:" >&2
  diff "$conf/skill-registry-contract.json" "$out/skill-registry-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -skill-registry-contract) > rust/conformance/skill-registry-contract.json" >&2
  exit 1
fi

echo "==> workspace contract"
# core/workspace's rules around *any* backend: traversal, the scope's two
# directions and the mandatory rule in front of them, the prefixed view and its
# own root, walk order and a skipped subtree, the four glob forms, the two
# sentences a bounded read can fail with, and the temp name atomic publish uses
# and cleans up -- replayed by
# crates/flowcraft-workspace/tests/workspace_contract.rs. Core ships no in-memory
# backend, so the medium is a map of path to bytes written twice, once here and
# once in the port. That is what makes the rows comparable: nothing in this file
# depends on a temporary directory, a clock or platform sorting, so it masks
# nothing and the replay compares every string.
"$out/oracle" -workspace-contract > "$out/workspace-contract.json"
if ! cmp -s "$out/workspace-contract.json" "$conf/workspace-contract.json"; then
  echo "rust/conformance/workspace-contract.json no longer matches core:" >&2
  diff "$conf/workspace-contract.json" "$out/workspace-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -workspace-contract) > rust/conformance/workspace-contract.json" >&2
  exit 1
fi

echo "==> secret contract"
# The ${secret:...} grammar: what the scheme emits for a path (a marker, not a
# value), what resolving one through a store answers, and what a Secret value is
# on the wire -- replayed by crates/flowcraft-secret/tests/grammar_contract.rs.
# Every store in the file is a canned answer table, so nothing depends on the
# process environment, a clock or a filesystem; the one mask is the escaping
# spelling of the wire form, which is `json.Marshal`'s HTML escaping on core's
# side and absent on the port's, and both decode to the string "<secret>".
"$out/oracle" -secret-contract > "$out/secret-contract.json"
if ! cmp -s "$out/secret-contract.json" "$conf/secret-contract.json"; then
  echo "rust/conformance/secret-contract.json no longer matches core:" >&2
  diff "$conf/secret-contract.json" "$out/secret-contract.json" | head -40 >&2 || true
  echo "regenerate: (cd tools/conformance-oracle && GOWORK=off go run . -secret-contract) > rust/conformance/secret-contract.json" >&2
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
