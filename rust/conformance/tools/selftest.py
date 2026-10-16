#!/usr/bin/env python3
"""Negative self-tests for the gate's own failure modes.

A gate that cannot fail is not a gate, so every failure mode the two checkers
claim to catch is exercised here against a deliberately broken input -- and
the accepted case beside it. Nothing here needs the real oracle: each case
builds a tiny one (a Python script that prints baked records) and a corpus of
one file under a temporary directory.

    python3 rust/conformance/tools/selftest.py     # one line per case

Exits non-zero if any case behaves differently from what it asserts.
"""

import json
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
STABILITY = os.path.join(HERE, "stability.py")
DIFF = os.path.join(HERE, "diff.py")
# The shape core uses for a collapse refusal; stability.py keys on the tail.
COLLAPSE = (
    "config utils: convert YAML to JSON: "
    'YAML keys !!int 1, !!str "1" convert to the same JSON key "1"'
)


def write(path, text):
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(text)
    return path


def write_oracle(directory, records, alternate=None):
    """An executable oracle: prints RECORDS[arg] per argument, or a default.

    `alternate`, when given, is emitted on every second invocation (a state
    file tells the invocations apart), so stability.py's sampling direction
    has something to see.
    """
    state = os.path.join(directory, "oracle.state")
    if os.path.exists(state):
        os.remove(state)
    source = (
        "#!/usr/bin/env python3\n"
        "import json, os, sys\n"
        f"RECORDS = {records!r}\n"
        f"ALTERNATE = {alternate!r}\n"
        f"STATE = {state!r}\n"
        "n = 0\n"
        "if os.path.exists(STATE):\n"
        "    n = int(open(STATE).read() or 0)\n"
        "open(STATE, 'w').write(str(n + 1))\n"
        "for arg in sys.argv[1:]:\n"
        "    record = RECORDS.get(arg)\n"
        "    if record is None:\n"
        "        record = {'file': arg, 'format': 'yaml', 'json': '{}'}\n"
        "    if ALTERNATE is not None and n % 2 == 1:\n"
        "        record = ALTERNATE\n"
        "    print(json.dumps(record))\n"
    )
    path = write(os.path.join(directory, "oracle.py"), source)
    os.chmod(path, 0o755)
    return path


def stability(oracle, allowlist, corpus, runs, verify_runs):
    proc = subprocess.run(
        [
            sys.executable, STABILITY,
            "--allowlist", allowlist, "--oracle", oracle,
            "--runs", str(runs), "--verify-runs", str(verify_runs), corpus,
        ],
        capture_output=True,
        text=True,
    )
    return proc.returncode, proc.stdout + proc.stderr


def diff(go, rust, allowlist=None, backend=None):
    argv = [sys.executable, DIFF, "--go", go, rust]
    if allowlist:
        argv += ["--allowlist", allowlist]
    if backend:
        argv += ["--backend", backend]
    proc = subprocess.run(argv, capture_output=True, text=True)
    return proc.returncode, proc.stdout + proc.stderr


def jsonl(path, records):
    lines = "".join(json.dumps(record) + "\n" for record in records)
    return write(path, lines)


def main():
    failures = []

    def check(name, code, output, want=0):
        ok = code == want
        print(f"  [{'ok' if ok else 'FAIL'}] {name}")
        if not ok:
            failures.append((name, code, output))

    with tempfile.TemporaryDirectory(prefix="conformance-selftest-") as directory:
        corpus = write(os.path.join(directory, "case.yaml"), '1: a\n"1": b\n')
        empty = write(os.path.join(directory, "empty.txt"), "# no entries\n")
        listed = write(
            os.path.join(directory, "listed.txt"),
            f"oracle-unstable {corpus} -- listed by the self-test\n",
        )

        refused = {
            "file": corpus, "format": "yaml",
            "error": COLLAPSE, "key_collisions": ["1"],
        }
        converted = {
            "file": corpus, "format": "yaml",
            "json": '{"1":"b"}', "key_collisions": ["1"],
        }
        other = {
            "file": corpus, "format": "yaml",
            "error": "some other error", "key_collisions": ["1"],
        }
        stable = {"file": corpus, "format": "yaml", "json": "{}"}
        variant = {"file": corpus, "format": "yaml", "json": '{"1":"a"}'}

        def stability_case(name, records, allowlist, runs=1, verify_runs=1, want=0):
            oracle = write_oracle(directory, records, records.get("alternate"))
            code, output = stability(oracle, allowlist, corpus, runs, verify_runs)
            check(name, code, output, want)

        # --- stability.py -------------------------------------------------
        stability_case(
            "stability accepts an evidenced collapse that is refused",
            {corpus: refused}, empty, want=0,
        )
        stability_case(
            "stability rejects an evidenced collapse that is not refused",
            {corpus: converted}, empty, want=1,
        )
        stability_case(
            "stability rejects an evidenced collapse refused for another reason",
            {corpus: other}, empty, want=1,
        )
        stability_case(
            "stability rejects an evidenced file listed as oracle-unstable",
            {corpus: refused}, listed, want=1,
        )
        stability_case(
            "stability rejects a stale oracle-unstable entry",
            {corpus: stable}, listed, want=1,
        )
        stability_case(
            "stability rejects unlisted nondeterminism",
            {corpus: stable, "alternate": variant}, empty, runs=2, want=1,
        )

        # --- diff.py ------------------------------------------------------
        exact = {"file": corpus, "format": "yaml", "json": "{}"}

        code, output = diff(
            jsonl(os.path.join(directory, "go.exact.jsonl"), [exact]),
            jsonl(os.path.join(directory, "rust.exact.jsonl"),
                  [{"file": corpus, "backend": "saphyr-go", "format": "yaml", "json": "{}"}]),
            allowlist=empty, backend="saphyr-go",
        )
        check("diff accepts identical records", code, output, want=0)

        code, output = diff(
            jsonl(os.path.join(directory, "go.error.jsonl"),
                  [{"file": corpus, "format": "yaml", "error": "boom"}]),
            jsonl(os.path.join(directory, "rust.error.jsonl"),
                  [{"file": corpus, "backend": "saphyr-go", "format": "yaml", "json": "{}"}]),
            allowlist=empty, backend="saphyr-go",
        )
        check("diff rejects error-vs-value", code, output, want=1)

        code, output = diff(
            jsonl(os.path.join(directory, "go.missing.jsonl"), [exact]),
            jsonl(os.path.join(directory, "rust.missing.jsonl"),
                  [{"file": "other.yaml", "backend": "saphyr-go", "format": "yaml", "json": "{}"}]),
            allowlist=empty, backend="saphyr-go",
        )
        check("diff rejects a missing record", code, output, want=1)

        stale_entry = write(
            os.path.join(directory, "stale.txt"),
            f"VALUE-DIFF {corpus} -- listed by the self-test\n",
        )
        code, output = diff(
            jsonl(os.path.join(directory, "go.stale.jsonl"), [exact]),
            jsonl(os.path.join(directory, "rust.stale.jsonl"),
                  [{"file": corpus, "backend": "saphyr-go", "format": "yaml", "json": "{}"}]),
            allowlist=stale_entry, backend="saphyr-go",
        )
        check("diff rejects a stale allowlist entry", code, output, want=1)

    print()
    if failures:
        for name, code, output in failures:
            print(f"FAIL {name} (exit {code})")
            print(output.rstrip())
        sys.exit(1)
    print("selftest: ok")


if __name__ == "__main__":
    main()
