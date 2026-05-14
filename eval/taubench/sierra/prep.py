#!/usr/bin/env python3
# prep.py — one-shot converter from Sierra τ-bench's Python fixtures to
# the JSON shapes eval/taubench expects.
#
# Why a Python script (and not Go): Sierra ships `tasks_test.py` as
# Python `Task(...)` literals defined via pydantic models. Re-typing
# 165 tasks (each with ~5-15 actions, each action with arbitrary
# kwargs) by hand in Go is bug-bait. Loading the .py files via
# importlib gives us the canonical bytes at Sierra commit precision
# and serialises them via pydantic's own json.
#
# Usage:
#   python3 eval/taubench/sierra/prep.py \
#       --sierra /tmp/tau-bench \
#       --out    /var/lib/flowcraft-eval/datasets/taubench
#
# Outputs (per domain in retail|airline):
#   <out>/<domain>/initial_state.json   merged Sierra data/*.json
#   <out>/<domain>/tasks_test.json      Sierra TASKS_TEST exported as
#                                       the UpstreamTask wire shape
#                                       (eval/taubench/upstream.go).
#
# The script intentionally bypasses tau_bench's top-level __init__
# (which imports litellm). Only pydantic is required at runtime.

import argparse
import importlib.util
import json
import sys
import types as _t
from pathlib import Path


def _bypass_tau_bench_init(sierra_root: Path) -> None:
    """Make tau_bench.types importable without triggering the
    litellm-pulling __init__ chain. We register synthetic empty
    parents in sys.modules and then load tau_bench/types.py directly.
    Without this Sierra's package import path crashes on a missing
    `litellm` (see tau_bench/envs/user.py)."""
    for name in (
        "tau_bench",
        "tau_bench.envs",
        "tau_bench.envs.retail",
        "tau_bench.envs.airline",
    ):
        sys.modules[name] = _t.ModuleType(name)

    types_path = sierra_root / "tau_bench" / "types.py"
    spec = importlib.util.spec_from_file_location("tau_bench.types", types_path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"could not load {types_path}")
    mod = importlib.util.module_from_spec(spec)
    sys.modules["tau_bench.types"] = mod
    spec.loader.exec_module(mod)


def _load_tasks_module(sierra_root: Path, domain: str) -> object:
    """Load tau_bench/envs/<domain>/tasks_test.py and return the loaded
    module. The variable name holding the task list differs between
    domains (retail=TASKS_TEST, airline=TASKS); the caller picks."""
    path = sierra_root / "tau_bench" / "envs" / domain / "tasks_test.py"
    spec = importlib.util.spec_from_file_location(f"tasks_{domain}", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"could not load {path}")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _export_tasks(mod: object, candidate_attrs: list) -> list:
    """Pick the first non-empty list-of-Task attribute from `mod` and
    return [Task.model_dump()] for each. The dump drops pydantic
    private state but keeps every documented field on Task and
    Action — i.e. exactly the UpstreamTask wire shape consumed by
    eval/taubench/upstream.go LoadUpstreamTasks."""
    for attr in candidate_attrs:
        if not hasattr(mod, attr):
            continue
        tasks = getattr(mod, attr)
        if not tasks:
            continue
        return [t.model_dump() for t in tasks]
    raise RuntimeError(
        f"none of {candidate_attrs} found / non-empty in {mod.__name__}"
    )


def _merge_data(sierra_root: Path, domain: str) -> dict:
    """Sierra's per-domain data/ folder holds N top-level JSON files
    (users.json, orders.json, products.json for retail; users.json,
    flights.json, reservations.json for airline). The Python tools
    access them as data["users"], data["orders"], etc. — i.e. a
    single merged map. Construct it here so initial_state.json is one
    file that LoadInitialState (eval/taubench/upstream.go) can read
    in one os.ReadFile + json.Unmarshal."""
    data_dir = sierra_root / "tau_bench" / "envs" / domain / "data"
    merged: dict = {}
    for json_path in sorted(data_dir.glob("*.json")):
        key = json_path.stem  # users, orders, products, flights, reservations
        with json_path.open() as f:
            merged[key] = json.load(f)
    if not merged:
        raise RuntimeError(f"no JSON files under {data_dir}")
    return merged


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sierra", required=True, type=Path,
                        help="Path to a sierra-research/tau-bench clone")
    parser.add_argument("--out", required=True, type=Path,
                        help="Output root (will create <domain>/ subdirs)")
    parser.add_argument("--domains", nargs="+", default=["retail", "airline"],
                        choices=["retail", "airline"])
    args = parser.parse_args()

    if not args.sierra.is_dir():
        raise SystemExit(f"--sierra {args.sierra} is not a directory")

    _bypass_tau_bench_init(args.sierra)

    # Sierra uses TASKS_TEST for retail, TASKS for airline; tolerate
    # either so future Sierra revisions that rename do not break us.
    candidate_attrs = ["TASKS_TEST", "TASKS", "tasks"]

    args.out.mkdir(parents=True, exist_ok=True)

    for domain in args.domains:
        out_dir = args.out / domain
        out_dir.mkdir(parents=True, exist_ok=True)

        initial_state = _merge_data(args.sierra, domain)
        (out_dir / "initial_state.json").write_text(
            json.dumps(initial_state, indent=2, sort_keys=True) + "\n"
        )

        mod = _load_tasks_module(args.sierra, domain)
        tasks = _export_tasks(mod, candidate_attrs)
        (out_dir / "tasks_test.json").write_text(
            json.dumps(tasks, indent=2, sort_keys=False) + "\n"
        )

        print(
            f"{domain}: state-keys={list(initial_state)}, tasks={len(tasks)}, "
            f"out={out_dir}",
            file=sys.stderr,
        )


if __name__ == "__main__":
    main()
