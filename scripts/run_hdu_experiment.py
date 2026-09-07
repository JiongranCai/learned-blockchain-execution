#!/usr/bin/env python3
"""Ordered H/D/U motifs in one block, P=8 and L=W; run on the experiment host."""

import argparse
import base64
import itertools
import json
from pathlib import Path
import random
import struct

from run_workload_prefix_experiment import matrix, run, summarize


def artifact(groups, order, units_per_ms, jitter, seed):
    rng = random.Random(seed)
    initial, transactions = {}, []

    def key(name):
        initial[name] = 0
        return base64.b64encode(name.encode()).decode()

    def compute(ms):
        return {"op": "compute", "compute_units": round(
            ms * units_per_ms * (1 + rng.uniform(-jitter, jitter)))}

    def transaction(name, instructions):
        for i, instruction in enumerate(instructions):
            instruction["id"] = f"{name}-op{i}"
        return {"id": name, "max_units": sum(1 + i.get("compute_units", 0) for i in instructions),
                "program": {"instructions": instructions}}

    register = {"base": {"kind": "register", "register": "r"}, "delta": 1}
    for group in range(groups):
        roles = {role: [] for role in "HDU"}
        # Generate the same transactions/costs before applying the order control.
        for i in range(4):
            name = f"g{group:04d}"
            hkey, dkey, ukey = (key(f"{name}-{role}{i}") for role in "hdu")
            read_h = {"op": "read", "key": hkey, "register": "r"}
            ret = {"op": "return", "expression": register}
            roles["H"].append(transaction(f"{name}-H{i}", [dict(read_h), compute(10),
                {"op": "write", "key": hkey, "expression": register}, dict(ret)]))
            roles["D"].append(transaction(f"{name}-D{i}", [compute(8), dict(read_h), compute(1),
                {"op": "write", "key": dkey, "expression": register}, dict(ret)]))
            roles["U"].append(transaction(f"{name}-U{i}", [
                {"op": "read", "key": ukey, "register": "r"}, compute(20),
                {"op": "write", "key": ukey, "expression": register}, dict(ret)]))
        transactions.extend(tx for role in order for tx in roles[role])
    return {"schema_version": "workload-artifact-v3",
            "generator": {"name": "ordered-hdu", "version": "v1", "seed": seed,
                          "config": {"groups": groups, "order": order, "units_per_ms": units_per_ms,
                                     "jitter": jitter, "seed": seed}},
            "initial_state": [{"key": base64.b64encode(k.encode()).decode(),
                               "value": base64.b64encode(struct.pack(">q", v)).decode()}
                              for k, v in sorted(initial.items())],
            "ordered_blocks": [{"id": "block-0", "height": 1, "transactions": transactions}],
            "logical_arrival_schedule": [{"sequence": i, "logical_time": 0,
                "block_id": "block-0", "transaction_id": tx["id"]} for i, tx in enumerate(transactions)],
            "engine_visible_metadata": []}


def configurations(base, args, stage_dir):
    (stage_dir / "workloads").mkdir()
    # Fixed-cost inputs are identical across seeds: one anchor, not pseudo-replication.
    cells = list(itertools.product((1, 8, 32, 128), ("HDU", "HUD"), (0,), (101,)))
    if args.stage == "repeated":
        cells += list(itertools.product((1, 128), ("HDU", "HUD"), (0.1,), (107, 10707, 1070707)))
    for groups, order, jitter, seed in cells:
        name = f"{order}_g{groups}_j{int(jitter * 100)}_s{seed}"
        workload = stage_dir / "workloads" / f"{name}.json"
        workload.write_text(json.dumps(artifact(groups, order, args.units_per_ms, jitter, seed)) + "\n")
        config = matrix(base, "single-hot", 1, 0, seed, stage_dir / name, args.stage, args.notes)
        config["workload"] = {"artifact_path": str(workload)}
        yield name, config


def describe(config, suite, name):
    params = json.loads(Path(config["workload"]["artifact_path"]).read_text())["generator"]["config"]
    return {"profile": f'{params["order"]}_g{params["groups"]}_j{int(params["jitter"] * 100)}',
            **params, "transactions": params["groups"] * 12}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_dir", type=Path)
    parser.add_argument("--units-per-ms", type=int, required=True, help="fixed CPU work from host calibration")
    parser.add_argument("--stage", choices=("pilot", "repeated"), default="pilot")
    parser.add_argument("--notes", default="")
    parser.add_argument("--summarize-only", action="store_true")
    args = parser.parse_args()
    if args.units_per_ms <= 0:
        parser.error("--units-per-ms must be positive")
    args.suite = "hdu"
    stage_dir = args.run_dir.resolve() / args.stage
    if args.summarize_only:
        summarize(stage_dir, args.suite, describe)
    else:
        run(args, stage_dir, configurations, describe)


if __name__ == "__main__":
    main()
