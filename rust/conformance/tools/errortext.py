#!/usr/bin/env python3
"""Compare the *words* of the two sides' refusals, not just the fact of them.

`diff.py` treats a refusal as one observation -- "both sides refused" -- and
that is right for a refusal that quotes a library. But the refusals core
writes itself are a contract (`config utils: ...`, the unusable-key and
collapsing-key sentences), and go-yaml's own sentences are part of it where
the document alone decides them (a repeated key, a scalar a tag cannot decode,
a value JSON cannot write). The port reproduces those; this checker holds it
to the text.

Usage: errortext.py --go DUMP [--go DUMP ...] <rust.jsonl>
                    [--backend NAME] [--allowlist FILE] [--details N]
       (normally invoked by rust/conformance/run.sh)

Every file where both sides refused must have refused in the same words, or be
listed as `error-text <path>` in the allowlist with a reason: scanner wording
and the one message whose Go text is a map's iteration order are the two
classes there today. Entries are self-cleaning -- one whose file no longer
differs fails the run, the same rule the other categories follow.
"""

import allowlist
import collections
import json
import sys

# The categories another checker owns, which this one must not report as
# stale or unused.
OTHER_CATEGORIES = (
    "oracle-unstable",
    "VALUE-DIFF",
    "error-vs-value",
    "unparseable",
    "missing-record",
)


def load(path):
    """Return {file: record}; the oracle's dumps hold one record per file."""
    with open(path, encoding="utf-8") as handle:
        return {
            json.loads(line)["file"]: json.loads(line)
            for line in handle
            if line.strip()
        }


def load_all(path):
    """Return every record; the probe emits one per (file, backend)."""
    with open(path, encoding="utf-8") as handle:
        return [json.loads(line) for line in handle if line.strip()]


def main():
    args = sys.argv[1:]
    backend_filter = None
    allowlist_path = None
    detail_limit = 10
    go_paths = []
    rest = []
    index = 0
    while index < len(args):
        if args[index] == "--backend":
            backend_filter = args[index + 1]
            index += 2
        elif args[index] == "--allowlist":
            allowlist_path = args[index + 1]
            index += 2
        elif args[index] == "--go":
            go_paths.append(args[index + 1])
            index += 2
        elif args[index] == "--details":
            detail_limit = int(args[index + 1])
            index += 2
        else:
            rest.append(args[index])
            index += 1
    if not go_paths or len(rest) != 1:
        raise SystemExit(__doc__.strip().splitlines()[-1].strip())
    rust_path = rest[0]

    allowed = allowlist.load(allowlist_path) if allowlist_path else {}
    allowed_used = set()
    # The oracle's own dumps are compared against by name, first run only:
    # stability.py has already refused to let an unstable first run through.
    go = {}
    for path in go_paths:
        go.update(load(path))

    rust_by_file = collections.defaultdict(dict)
    for record in load_all(rust_path):
        rust_by_file[record["file"]][record["backend"]] = record

    compared = 0
    identical = 0
    exempted = 0
    differing = []
    for file in sorted(go):
        go_record = go[file]
        if "error" not in go_record:
            continue
        for backend, rust_record in rust_by_file.get(file, {}).items():
            if backend_filter and backend != backend_filter:
                continue
            # One side refusing where the other converted is not this
            # checker's finding: diff.py owns that category.
            if "error" not in rust_record:
                continue
            compared += 1
            if rust_record["error"] == go_record["error"]:
                identical += 1
            elif file in allowed.get("error-text", {}):
                allowed_used.add(("error-text", file))
                exempted += 1
            else:
                differing.append((file, go_record["error"], rust_record["error"]))

    print(
        f"error text: {compared} refusal(s) compared, {identical} identical, "
        f"{exempted} exempted"
    )
    for file, go_text, rust_text in differing[:detail_limit]:
        print(f"  {file}")
        print(f"      go   {go_text}")
        print(f"      rust {rust_text}")
    if allowlist_path:
        allowlist.check_stale(
            allowed, allowed_used, allowlist_path, ignore=OTHER_CATEGORIES
        )
    if differing:
        print(
            f"\n{len(differing)} refusal(s) are worded differently from the "
            "oracle; either reproduce the text or list the file as "
            "`error-text` with a reason:"
        )
        for file, _, _ in differing:
            print(f"  {file}")
        sys.exit(1)


if __name__ == "__main__":
    main()
