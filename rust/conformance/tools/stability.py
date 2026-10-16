#!/usr/bin/env python3
"""Check that the Go oracle is reproducible, and that it refuses what it must.

Nondeterminism on the Go side is a finding about core itself, not noise to be
tolerated: a differential gate anchored on an unstable oracle cannot gate
anything. Three directions are checked, because a collapse the oracle fails to
refuse, a stale excuse and silent instability are all failures:

  evidence the oracle reports mappings where two distinct keys collapse onto
           one JSON key (`key_collisions`). Such a document has no defined
           conversion result; core refuses it, and the oracle names the
           colliding keys from a single run. Every file so named must therefore
           be refused, and the refusal must be the collapse refusal -- this
           needs neither sampling nor luck, and such a file must not be listed
           as `oracle-unstable` at all.
  broad    the oracle is run N times over the whole corpus; any file whose
           record differs between runs must be listed as `oracle-unstable`.
  targeted every file listed as `oracle-unstable` is re-run on its own, many
           times; if it now looks reproducible the exemption is stale.

The broad and targeted passes are the backstop for nondeterminism this gate
cannot enumerate. They are statistical, and the numbers say so: the minority
outcome of a collapsing file showed up in 47 of 400 runs, so 25 corpus runs
miss it in about 4% of invocations. The collapsing class itself is now
enumerated as evidence and refused outright, so it no longer depends on the
number at all.

Usage: stability.py --allowlist FILE --oracle BIN [--runs N] [--verify-runs M] \\
                    <corpus args...>
"""

import argparse
import json
import os
import subprocess
import sys

import allowlist


def run(oracle, args):
    """Return ({file: record}, raw output) for one oracle invocation."""
    proc = subprocess.run([oracle, *args], capture_output=True, text=True)
    if proc.returncode != 0:
        raise SystemExit(f"{oracle} failed: {proc.stderr.strip()}")
    records = {}
    for line in proc.stdout.splitlines():
        if line.strip():
            record = json.loads(line)
            records[record["file"]] = record
    return records, proc.stdout


def canonical(record):
    """One record as text, for comparisons that must notice any difference."""
    return json.dumps(record, sort_keys=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--allowlist", required=True)
    parser.add_argument("--oracle", required=True)
    # Defaults are deliberately above the handful that look sufficient: the
    # minority outcome of a collapsing file shows up in ~12% of runs, so five
    # runs miss it about half the time.
    parser.add_argument("--runs", type=int, default=25)
    parser.add_argument("--verify-runs", type=int, default=100)
    parser.add_argument(
        "--dump-dir",
        help="keep every corpus run here as go.<n>.jsonl, for diff.py to reuse",
    )
    parser.add_argument("corpus", nargs="+")
    args = parser.parse_args()

    listed = allowlist.load(args.allowlist).get("oracle-unstable", {})

    runs = []
    collisions = {}
    refusals = {}
    for index in range(1, args.runs + 1):
        records, raw = run(args.oracle, args.corpus)
        runs.append(records)
        for name, record in records.items():
            keys = record.get("key_collisions") or []
            if keys:
                collisions.setdefault(name, set()).update(keys)
            if "error" in record:
                refusals[name] = record["error"]
        if args.dump_dir:
            with open(os.path.join(args.dump_dir, f"go.{index}.jsonl"), "w") as handle:
                handle.write(raw)
    files = sorted(set().union(*[set(r) for r in runs]))

    unstable = {}
    for name in files:
        seen = {canonical(r[name]) if name in r else "<missing>" for r in runs}
        if len(seen) > 1:
            unstable[name] = seen

    failures = []

    # Direction 1: evidence of a document with no defined conversion result.
    # The evidence is a claim about the answer, so the oracle must refuse the
    # file, and the refusal must be the collapse refusal -- not a value, and
    # not some other error.
    unrefused = sorted(
        name
        for name in collisions
        if "convert to the same JSON key" not in refusals.get(name, "")
    )
    if unrefused:
        failures.append(
            "The oracle reports colliding JSON keys but does not refuse them "
            "with the collapse refusal:"
        )
        failures += [
            f"  {name}  (keys: {', '.join(sorted(collisions[name]))})  "
            f"-> {refusals.get(name, '(no refusal)')[:120]}"
            for name in unrefused
        ]
        failures.append(
            "  The collapse must be refused on both sides, not resolved."
        )
    mislisted = sorted(set(collisions) & set(listed))
    if mislisted:
        failures.append(
            "Listed as oracle-unstable, but the oracle refuses these with a "
            "defined answer (the collapse refusal); drop the exemption:"
        )
        failures += [f"  {name}" for name in mislisted]

    # Direction 2: instability the list does not know about.
    unlisted = sorted(set(unstable) - set(listed))
    if unlisted:
        failures.append("UNEXPECTED nondeterminism in the Go oracle:")
        failures += [
            f"  {name}  ({len(unstable[name])} different results in {args.runs} runs)"
            for name in unlisted
        ]

    # Direction 3: exemptions that are no longer true. A file with evidence is
    # not skipped: it must not be listed at all (Direction 1 says so), and if it
    # is, its refusal makes it look reproducible and the entry is stale.
    stale = []
    for name in sorted(listed):
        seen = set()
        for _ in range(args.verify_runs):
            records = run(args.oracle, [name])[0]
            seen.add(canonical(records[name]) if name in records else "<missing>")
            if len(seen) > 1:
                break
        if len(seen) == 1:
            stale.append(f"  {name}  (stable over {args.verify_runs} runs)")
    if stale:
        failures.append(
            "Listed as oracle-unstable but reproducible now -- re-measure and "
            "drop the exemption:"
        )
        failures += stale

    print(
        f"oracle stability: {args.runs} corpus runs over {len(files)} files, "
        f"{len(listed)} allowlisted file(s) verified with {args.verify_runs} runs each"
    )
    if collisions:
        print(
            f"  {len(collisions)} file(s) collapse distinct keys onto one JSON "
            f"key and are refused (evidence, single run):"
        )
        for name in sorted(collisions):
            print(f"    {name}  keys: {', '.join(sorted(collisions[name]))}")
    if unstable:
        print(f"  {len(unstable)} file(s) differ between runs")
    if failures:
        print()
        print("\n".join(failures))
        sys.exit(1)
    print("  ok")


if __name__ == "__main__":
    main()
