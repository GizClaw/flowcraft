#!/usr/bin/env python3
"""Generate the Go stubs and copy them next to their Go packages.

`buf generate` writes into `.proto-gen/go` with `paths=source_relative`; each
file is then copied to the directory named by its `option go_package`, so Go
import paths stay stable while the proto tree is language-neutral.

Run with `make proto-gen`; `make proto-check` additionally fails on drift.
"""

from __future__ import annotations

import os
import pathlib
import re
import shutil
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
PROTO = ROOT / "proto"
OUT = ROOT / ".proto-gen" / "go"

GO_PACKAGE = re.compile(r'option\s+go_package\s*=\s*"([^";]+)(?:;[^"]*)?";')


def main() -> int:
    buf = os.environ.get("BUF") or shutil.which("buf")
    if buf is None:
        print("buf is required: https://buf.build/docs/installation", file=sys.stderr)
        return 1

    subprocess.run([buf, "generate"], cwd=PROTO, check=True)

    copied = 0
    for proto in sorted((PROTO / "flowcraft").rglob("*.proto")):
        text = proto.read_text(encoding="utf-8")
        match = GO_PACKAGE.search(text)
        if match is None:
            print(f"{proto.relative_to(PROTO)}: no go_package, skipped")
            continue
        import_path = match.group(1)
        marker = "flowcraft/"
        if marker not in import_path:
            print(f"{proto.relative_to(PROTO)}: {import_path} is outside this repo, skipped")
            continue
        target_dir = ROOT / import_path.split(marker, 1)[1]
        target_dir.mkdir(parents=True, exist_ok=True)

        generated_dir = OUT / proto.relative_to(PROTO).parent
        stem = proto.stem
        for suffix in (f"{stem}.pb.go", f"{stem}_grpc.pb.go"):
            source = generated_dir / suffix
            if not source.exists():
                continue
            target = target_dir / suffix
            before = target.read_bytes() if target.exists() else None
            after = source.read_bytes()
            if before != after:
                target.write_bytes(after)
                copied += 1
                print(f"{target.relative_to(ROOT)}: updated")
    print(f"{copied} generated file(s) changed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
