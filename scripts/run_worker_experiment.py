#!/usr/bin/env python3
"""Compare worker counts at GOMAXPROCS=8 and L=W on a bound Linux host."""

import argparse
import itertools
import json
from pathlib import Path

from run_hdu_experiment import artifact
from run_workload_prefix_experiment import STAGES, matrix, run, summarize


WORKERS = (1, 4, 8, 16, 32, 64)


def configurations(base, args, stage_dir):
    cells = []
    for profile, seed in itertools.product(("uniform-multikey", "single-hot"), STAGES[args.stage][2]):
        name = f"{profile}_s{seed}"
        config = matrix(base, profile, 100000, 0.5, seed, stage_dir / name, args.stage, args.notes)
        workload = config["workload"]["synthetic"]
        # Same block size and CPU cost; a large uniform key space is the
        # low-contention control for the single-key hotspot.
        keys = 65536 if profile == "uniform-multikey" else 1024
        workload.update(initial_keys=keys, key_space=keys, block_count=1, transactions_per_block=1536)
        cells.append((name, config))

    (stage_dir / "workloads").mkdir()
    for groups, order in itertools.product((1, 128), ("HDU", "HUD")):
        name = f"{order}_g{groups}_s101"
        path = stage_dir / "workloads" / f"{name}.json"
        path.write_text(json.dumps(artifact(groups, order, args.units_per_ms, 0, 101)) + "\n")
        config = matrix(base, "single-hot", 1, 0, 101, stage_dir / name, args.stage, args.notes)
        config["workload"] = {"artifact_path": str(path)}
        cells.append((name, config))

    for name, config in cells:
        # All worker counts share one input and one round schedule. The runner
        # interleaves the 18 cases and starts a fresh process for each case.
        config["cases"] = [dict(case, id=f'{case["id"]}-p{workers}', executors=workers,
                                max_speculative_inflight=0)
                           for workers in WORKERS for case in config["cases"]]
        yield name, config


def describe(config, suite, name):
    workload = config["workload"]
    if "synthetic" in workload:
        params = workload["synthetic"]
        count = params["block_count"] * params["transactions_per_block"]
    else:
        params = json.loads(Path(workload["artifact_path"]).read_text())["generator"]["config"]
        count = params["groups"] * 12
    return {"profile": name.rsplit("_s", 1)[0], "seed": params["seed"], "transactions": count}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_dir", type=Path, help="server project results/runs/<run-id>")
    parser.add_argument("--units-per-ms", type=int, required=True, help="HDU CPU work from host calibration")
    parser.add_argument("--stage", choices=STAGES, default="pilot")
    parser.add_argument("--notes", default="")
    parser.add_argument("--summarize-only", action="store_true")
    args = parser.parse_args()
    if args.units_per_ms <= 0:
        parser.error("--units-per-ms must be positive")
    args.suite = "workers"
    stage_dir = args.run_dir.resolve() / args.stage
    if args.summarize_only:
        summarize(stage_dir, args.suite, describe)
    else:
        run(args, stage_dir, configurations, describe)


if __name__ == "__main__":
    main()
