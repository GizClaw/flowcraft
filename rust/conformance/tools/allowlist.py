#!/usr/bin/env python3
"""Shared reader for rust/conformance/known-divergences.txt.

The file records the cases where the two implementations are *allowed* to
disagree, each with a reason. It is deliberately explicit and self-cleaning:
an entry that no longer matches anything is an error, so the list cannot rot
into a blanket excuse.

Format: one entry per line, `<category> <path>`, then `--` and a reason.

  oracle-unstable <path>    the Go oracle is not reproducible on this input
  VALUE-DIFF <path>         the two sides return different values, on purpose
  error-vs-value <path>     one side refuses, the other returns a value
"""

import collections


def load(path):
    """Return {category: {path: reason}} plus the raw (category, path) pairs."""
    entries = collections.defaultdict(dict)
    with open(path, encoding="utf-8") as handle:
        for lineno, raw in enumerate(handle, 1):
            line = raw.split("#", 1)[0].strip()
            if not line:
                continue
            comment = raw.split("#", 1)[1].strip() if "#" in raw else ""
            parts = line.split(None, 2)
            if len(parts) < 2:
                raise SystemExit(f"{path}:{lineno}: expected `<category> <path>`")
            category, target = parts[0], parts[1]
            why = parts[2].strip(" -—") if len(parts) > 2 else comment
            entries[category][target] = why
    return entries


def check_stale(entries, used, path, ignore=()):
    """Fail when an entry never matched: a stale excuse is worse than none.

    `ignore` names categories another checker owns, so two tools sharing one
    file do not each report the other's entries as unused.
    """
    stale = [
        f"  {category} {target}"
        for category, targets in entries.items()
        if category not in ignore
        for target in targets
        if (category, target) not in used
    ]
    if stale:
        raise SystemExit(
            f"{path}: {len(stale)} entr(ies) no longer match anything; "
            "remove them:\n" + "\n".join(stale)
        )
