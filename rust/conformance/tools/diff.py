#!/usr/bin/env python3
"""Diff the Rust probe output against the Go oracle.

Usage: diff.py --go DUMP [--go DUMP ...] <rust.jsonl>
                 [--backend NAME] [--allowlist FILE] [--details N]
       (normally invoked by rust/conformance/run.sh)

Several oracle dumps are passed on purpose. When they disagree on a file the
Go side is not reproducible there, and demanding equality would gate on a coin
flip: the port must then reproduce *one of the observed* results instead.

Categories, per (file, backend):
  exact                 byte-identical to the Go output
  value-equal           same JSON value; numbers or escaping differ in form
  both-error            both sides refused the document
  error-vs-value        one side refused, the other returned a value
  VALUE-DIFF            both returned values, and they differ
  unparseable           a side returned something that is not JSON
  missing-record        the probe produced no record for a file
  oracle-unstable-hit   the oracle itself disagrees with itself here, and the
                        port landed on one of the results it produced
  oracle-unstable-miss  ... and the port landed somewhere else

Exit status is non-zero when any failing category shows up, so the script
doubles as the CI gate.
"""

import allowlist
import collections
import json
import sys
from decimal import Decimal

# Categories that mean "the port does not reproduce the oracle". Anything
# else (exact, value-equal, both-error, oracle-unstable-hit) is agreement.
FAILING = (
    "error-vs-value",
    "VALUE-DIFF",
    "unparseable",
    "missing-record",
    "oracle-unstable-miss",
)

ORDER = [
    "exact",
    "value-equal",
    "both-error",
    "error-vs-value",
    "VALUE-DIFF",
    "unparseable",
    "missing-record",
    "oracle-unstable-hit",
    "oracle-unstable-miss",
    "allowlisted",
]


class Num:
    __slots__ = ("kind", "value")

    def __init__(self, kind, value):
        self.kind = kind
        self.value = value

    def __repr__(self):
        return f"{self.kind}:{self.value}"

    def __eq__(self, other):
        return (
            isinstance(other, Num)
            and self.kind == other.kind
            and self.value == other.value
        )

    def __hash__(self):
        return hash((self.kind, self.value))


def load(path):
    """Return {file: record}; only for dumps holding one record per file."""
    with open(path, encoding="utf-8") as handle:
        return {json.loads(line)["file"]: json.loads(line) for line in handle if line.strip()}


def load_all(path):
    """Return every record; the probe emits one per (file, backend)."""
    with open(path, encoding="utf-8") as handle:
        return [json.loads(line) for line in handle if line.strip()]


def observable(record):
    """The part of a record the two sides must agree on: an error, or JSON."""
    return ("error",) if "error" in record else ("json", record["json"])


def parse_json(text):
    return json.loads(
        text,
        parse_int=lambda s: Num("int", Decimal(s)),
        parse_float=lambda s: Num("float", Decimal(s)),
    )


def canon(node, keep_kind):
    """Comparable form; numbers compare by value, optionally by kind too."""
    if isinstance(node, Num):
        return ("num", node.kind if keep_kind else "", node.value)
    if isinstance(node, dict):
        return {k: canon(v, keep_kind) for k, v in node.items()}
    if isinstance(node, list):
        return [canon(v, keep_kind) for v in node]
    return node


def diffs(a, b, path="$", out=None, limit=5):
    """Collect up to `limit` path-level differences."""
    if out is None:
        out = []
    if len(out) >= limit:
        return out
    if isinstance(a, dict) and isinstance(b, dict):
        for key in sorted(set(a) | set(b)):
            if key not in a:
                out.append(f"{path}.{key}: missing on go side")
            elif key not in b:
                out.append(f"{path}.{key}: missing on rust side")
            else:
                diffs(a[key], b[key], f"{path}.{key}", out, limit)
    elif isinstance(a, list) and isinstance(b, list):
        if len(a) != len(b):
            out.append(f"{path}: length {len(a)} vs {len(b)}")
        for i, (x, y) in enumerate(zip(a, b)):
            diffs(x, y, f"{path}[{i}]", out, limit)
    elif a != b:
        out.append(f"{path}: go={a!r} rust={b!r}")
    return out


def classify(go_obs, rust_obs):
    """Category for a file the oracle reproduces consistently."""
    if go_obs == rust_obs:
        return "exact" if go_obs[0] == "json" else "both-error"
    if go_obs[0] == "error" and rust_obs[0] == "error":
        return "both-error"
    if go_obs[0] != rust_obs[0]:
        return "error-vs-value"
    try:
        a, b = parse_json(go_obs[1]), parse_json(rust_obs[1])
    except Exception:  # noqa: BLE001
        return "unparseable"
    if canon(a, False) == canon(b, False) or canon(a, True) == canon(b, True):
        return "value-equal"
    return "VALUE-DIFF"


def describe(obs):
    return obs[1][:160] if obs[0] == "json" else "(refused)"


def main():
    args = sys.argv[1:]
    backend_filter = None
    allowlist_path = None
    detail_limit = 10
    go_paths = []
    rest = []
    i = 0
    while i < len(args):
        if args[i] == "--backend":
            backend_filter = args[i + 1]
            i += 2
        elif args[i] == "--allowlist":
            allowlist_path = args[i + 1]
            i += 2
        elif args[i] == "--go":
            go_paths.append(args[i + 1])
            i += 2
        elif args[i] == "--details":
            detail_limit = int(args[i + 1])
            i += 2
        else:
            rest.append(args[i])
            i += 1
    if not go_paths or len(rest) != 1:
        raise SystemExit(__doc__.strip().splitlines()[2].strip())
    rust_path = rest[0]

    allowed = allowlist.load(allowlist_path) if allowlist_path else {}
    allowed_used = set()

    go_runs = [load(path) for path in go_paths]
    files = sorted(set().union(*[set(run) for run in go_runs]))

    rust_by_file = collections.defaultdict(dict)
    for record in load_all(rust_path):
        rust_by_file[record["file"]][record["backend"]] = record

    backends = sorted({b for per in rust_by_file.values() for b in per})
    stats = {b: collections.Counter() for b in backends}
    fixture_stats = {b: collections.Counter() for b in backends}
    details = {b: [] for b in backends}

    def record(backend, file, category, go_view, rust_view, is_fixture, extra=None):
        if category in FAILING and file in allowed.get(category, {}):
            allowed_used.add((category, file))
            category = "allowlisted"
        stats[backend][category] += 1
        if is_fixture:
            fixture_stats[backend][category] += 1
        if category != "exact" and len(details[backend]) < detail_limit:
            entry = [file, category, go_view, rust_view]
            if extra:
                entry.append(extra)
            details[backend].append(entry)

    for file in files:
        is_fixture = "/fixtures/" in file
        # Every distinct result the oracle produced for this file. More than
        # one means it is not reproducible, which stability.py must already
        # have recorded; demanding equality would gate on a coin flip.
        variants = {observable(run[file]) for run in go_runs if file in run}
        per_backend = rust_by_file.get(file, {})
        for backend, rr in per_backend.items():
            if backend_filter and backend != backend_filter:
                continue
            rust_obs = observable(rr)
            if len(variants) > 1:
                if rust_obs in variants:
                    category = "oracle-unstable-hit"
                else:
                    category = "oracle-unstable-miss"
                go_view = " | ".join(sorted(describe(v) for v in variants))
            else:
                (go_obs,) = variants
                category = classify(go_obs, rust_obs)
                go_view = describe(go_obs)
            extra = None
            if category in ("VALUE-DIFF", "oracle-unstable-miss"):
                reference = go_obs if category == "VALUE-DIFF" else min(variants)
                if reference[0] != "json" or rust_obs[0] != "json":
                    reference = None
            else:
                reference = None
            if reference:
                try:
                    extra = "; ".join(
                        diffs(parse_json(reference[1]), parse_json(rust_obs[1]))
                    )
                except Exception as exc:  # noqa: BLE001
                    extra = f"(diff failed: {exc})"
            record(
                backend, file, category, go_view, describe(rust_obs), is_fixture, extra
            )

        # A record that never arrived is a failure, not a silent skip: a probe
        # that dies halfway through the corpus must not look like a pass.
        absent = [b for b in backends if b not in per_backend]
        if backend_filter:
            absent = [b for b in absent if b == backend_filter]
        for backend in absent:
            record(
                backend,
                file,
                "missing-record",
                " | ".join(sorted(describe(v) for v in variants)),
                "(no record)",
                is_fixture,
            )

    print(f"oracle runs: {len(go_runs)}   files: {len(files)}")
    header = f"{'backend':12} {'files':>6} " + " ".join(f"{c:>14}" for c in ORDER)
    print(header)
    for backend in backends:
        if backend_filter and backend != backend_filter:
            continue
        s, f = stats[backend], fixture_stats[backend]
        total = sum(s.values())
        print(
            f"{backend:12} {total:>6} "
            + " ".join(f"{s[c]:>14}" for c in ORDER)
        )
        print(
            f"{'  (fixtures)':12} {sum(f.values()):>6} "
            + " ".join(f"{f[c]:>14}" for c in ORDER)
        )

    for backend in backends:
        if backend_filter and backend != backend_filter:
            continue
        if not details[backend]:
            continue
        print(f"\n=== {backend}: first {len(details[backend])} non-exact")
        for file, category, go_view, rust_view, *extra in details[backend]:
            print(f"  [{category}] {file}")
            print(f"      go   {go_view}")
            print(f"      rust {rust_view}")
            if extra:
                print(f"      {extra[0]}")

    mismatches = sum(
        count
        for backend, counts in stats.items()
        if not backend_filter or backend == backend_filter
        for category, count in counts.items()
        if category in FAILING
    )
    if allowlist_path:
        # oracle-unstable entries belong to stability.py; only the diff
        # categories are diff.py's to keep honest.
        allowlist.check_stale(
            allowed, allowed_used, allowlist_path, ignore=("oracle-unstable",)
        )
    print(f"\nmismatches: {mismatches}")
    sys.exit(1 if mismatches else 0)


if __name__ == "__main__":
    main()
