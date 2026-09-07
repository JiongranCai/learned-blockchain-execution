#!/usr/bin/env python3
"""Compare compute placement at P=8, L=W; run on a Linux experiment host."""

import argparse
import copy
import csv
import itertools
import json
import os
from pathlib import Path
import random
import statistics
import subprocess

from analyze_cq3_ready_queue_experiment import paired_bootstrap


REPO = Path(__file__).resolve().parent.parent
PROFILES = ("single-hot", "hot8", "uniform-multikey", "zipf-mixed", "selective8")
STAGES = {"pilot": (1, 3, (42,)), "repeated": (3, 30, (43, 4243))}


def mixture(profile, units, prefix):
    rmw = {"weight": 1, "template": "rmw", "read_keys": 1, "update_keys": 1}
    if profile in ("single-hot", "hot8"):
        rmw["access"] = {"kind": "hotspot", "hot_keys": 1 if profile == "single-hot" else 8,
                         "hot_probability": 0.99 if profile == "single-hot" else 0.9}
        mix = [rmw]
    elif profile == "uniform-multikey":
        mix = [dict(rmw, read_keys=4, update_keys=2, access={"kind": "uniform"})]
    elif profile == "zipf-mixed":
        mix = [dict(rmw, weight=4), dict(rmw, weight=4, read_keys=4, update_keys=2),
               dict(rmw, weight=2, read_keys=4, update_keys=0)]
        for tx in mix:
            tx["access"] = {"kind": "zipf", "theta": 0.9}
    elif profile == "selective8":
        mix = [{"weight": 1, "template": "selective_read_set", "candidate_keys": 8}]
    else:
        raise ValueError(profile)
    for tx in mix:
        tx["compute"] = {"min_units": units, "max_units": units, "prefix_fraction": prefix}
    return mix


def matrix(base, profile, units, prefix, seed, cell, stage, notes):
    config = copy.deepcopy(base)
    warmup, measurement, _ = STAGES[stage]
    # Repeated exploration is not a formal ranking test or a hardware comparison.
    config.update(run_class="smoke", warmup_rounds=warmup, measurement_rounds=measurement,
                  order_seed=20260907 + seed)
    config["workload"]["synthetic"] = {
        "seed": seed, "initial_keys": 2048, "key_space": 1024,
        "block_count": 2, "transactions_per_block": 512,
        "mix": mixture(profile, units, prefix),
    }
    config["environment"]["affinity"] = "inherited CPU binding; see recorded cpu_allowed_list"
    config["environment"]["numa_policy"] = "inherited memory binding; see recorded memory_allowed_list"
    config["environment"]["page_cache"] = "OS cache retained; in-memory workload. " + notes
    config["output"] = {key: str(cell / name) for key, name in (
        ("validation_records", "validation.jsonl"), ("run_records", "runs.jsonl"),
        ("action_traces", "action-traces.jsonl"))}
    return config


def summarize(stage_dir):
    rows = []
    for path in sorted((stage_dir / "configs").glob("*.json")):
        config = json.loads(path.read_text())
        records = [json.loads(line) for line in Path(config["output"]["run_records"]).read_text().splitlines()]
        if any(r["status"] != "success" or r["censored"] or not r["canonical_match"] for r in records):
            raise ValueError(f"unsuccessful execution: {path}")
        measured = [r for r in records if r["phase"] == "measurement"]
        by_case = {c["id"]: sorted((r for r in measured if r["case"]["id"] == c["id"]),
                                    key=lambda r: r["round"]) for c in config["cases"]}
        expected = list(range(config["measurement_rounds"]))
        # Pair rounds explicitly; never silently summarize an incomplete cell.
        for case_records in by_case.values():
            if [r["round"] for r in case_records] != expected:
                raise ValueError(f"incomplete measurement rounds: {path}")
        workload = config["workload"]["synthetic"]
        compute = workload["mix"][0]["compute"]
        reference = by_case["runtime"]
        for case, case_records in by_case.items():
            ratios = [r["timing"]["execution_ns"] / b["timing"]["execution_ns"]
                      for r, b in zip(case_records, reference)]
            ratio, low, high = paired_bootstrap(ratios, workload["seed"])
            med = lambda values: statistics.median(list(values))
            row = {"cell": path.stem, "profile": path.stem.split("_c")[0],
                   "seed": workload["seed"], "compute_units": compute["max_units"],
                   "prefix_fraction": compute["prefix_fraction"], "case": case,
                   "measurements": len(case_records),
                   "median_ms": med(r["timing"]["execution_ns"] / 1e6 for r in case_records),
                   "ratio_to_runtime": ratio, "ratio_ci_low": low, "ratio_ci_high": high}
            for key in ("execution_attempts", "reexecution_attempts", "discarded_execution_units",
                        "reexecuted_execution_units", "useful_execution_units", "validation_failures"):
                row[key] = med(r["metrics"][key] for r in case_records)
            for key in ("estimate_aborts", "dispatch_deferrals"):
                row[key] = med(r["metrics"]["kernel_policy"][key] for r in case_records)
            row["planning_ms"] = med((r["metrics"]["dependency"]["acquisition_ns"] +
                                      r["metrics"]["dependency"]["representation_ns"]) / 1e6
                                     for r in case_records)
            rows.append(row)
    with (stage_dir / "summary.csv").open("w", newline="") as output:
        writer = csv.DictWriter(output, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)
    print(f"Summary: {stage_dir / 'summary.csv'}", flush=True)


def run(args, stage_dir):
    stage_dir.mkdir(parents=True)
    (stage_dir / "configs").mkdir()
    os.chdir(REPO)
    base = json.loads((REPO / "configs/experiments/workload/standard-smoke.json").read_text())
    env = dict(os.environ, GOMAXPROCS="8", GOTOOLCHAIN="local")
    binary = stage_dir / "bench"
    subprocess.run(["go", "build", "-trimpath", "-o", str(binary), "./cmd/bench"], env=env, check=True)
    with (stage_dir / "environment.txt").open("w") as output:
        output.write(args.notes + "\n")
        output.flush()
        for command in (["date", "-Is"], ["git", "rev-parse", "HEAD"], ["go", "version"],
                        ["uname", "-a"], ["lscpu"], ["cat", "/proc/mdstat"]):
            subprocess.run(command, stdout=output, stderr=subprocess.STDOUT, check=True)
    cells = list(itertools.product(PROFILES, (1000, 100000), (0, 0.5, 1), STAGES[args.stage][2]))
    random.Random(20260907).shuffle(cells)
    for index, (profile, units, prefix, seed) in enumerate(cells, 1):
        name = f"{profile}_c{units}_p{int(prefix * 100)}_s{seed}"
        cell = stage_dir / name
        cell.mkdir()
        config = matrix(base, profile, units, prefix, seed, cell, args.stage, args.notes)
        path = stage_dir / "configs" / f"{name}.json"
        path.write_text(json.dumps(config, indent=2) + "\n")
        print(f"[{index}/{len(cells)}] {name}", flush=True)
        with (cell / "run.log").open("w") as log:
            subprocess.run([str(binary), "run", "-config", str(path)], env=env,
                           stdout=log, stderr=subprocess.STDOUT, check=True)
    summarize(stage_dir)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_dir", type=Path, help="separate results directory on the server")
    parser.add_argument("--stage", choices=STAGES, default="pilot")
    parser.add_argument("--notes", default="")
    parser.add_argument("--summarize-only", action="store_true")
    args = parser.parse_args()
    stage_dir = args.run_dir.resolve() / args.stage
    if args.summarize_only:
        summarize(stage_dir)
    else:
        run(args, stage_dir)


if __name__ == "__main__":
    main()
