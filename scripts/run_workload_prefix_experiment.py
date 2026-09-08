#!/usr/bin/env python3
"""Compare workloads and speculation windows at P=8 on a Linux experiment host."""

import argparse
import copy
import itertools
import json
import os
from pathlib import Path
import random
import statistics
import subprocess

from analyze_cq3_ready_queue_experiment import exact_sign_test, holm_adjust, paired_bootstrap, write_csv


REPO = Path(__file__).resolve().parent.parent
PROFILES = ("single-hot", "hot8", "uniform-multikey", "zipf-mixed", "selective8")
STAGES = {"pilot": (1, 3, (42,)), "repeated": (3, 30, (43, 4243))}
CONTENTION_SEEDS = {"pilot": (71,), "repeated": (73, 7373, 737373)}
# Follower (prefix, suffix): shared anchor, fixed total, then fixed suffix.
FOLLOWER_COSTS = ((0, 100000), (50000, 50000), (90000, 10000),
                  (100000, 100000), (400000, 100000))


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


def contention_mixture(hot_keys, cold_units, head_units, prefix, suffix):
    def tx(weight, hot_probability, before, after):
        total = before + after
        return {"weight": weight, "template": "rmw", "read_keys": 1, "update_keys": 1,
                "access": {"kind": "hotspot", "hot_keys": hot_keys,
                           "hot_probability": hot_probability},
                "compute": {"min_units": total, "max_units": total,
                            "prefix_fraction": before / total if total else 0}}

    # Roles are sampled, not assigned to fixed positions. Cold read-only work
    # cannot add conflicts; changing its cost preserves the access skeleton.
    mix = [tx(0.05, 1, 0, head_units), tx(0.45, 1, prefix, suffix),
           tx(0.5, 0, 0, cold_units)]
    mix[2]["update_keys"] = 0
    return mix


def configurations(base, args, stage_dir):
    if args.suite == "speculation":
        for profile, seed in itertools.product(
                ("uniform-multikey", "single-hot", "zipf-mixed"), STAGES[args.stage][2]):
            name = f"{profile}_c100000_p50_s{seed}"
            config = matrix(base, profile, 100000, 0.5, seed,
                            stage_dir / name, args.stage, args.notes)
            keys = 65536 if profile == "uniform-multikey" else 1024
            config["workload"]["synthetic"].update(
                initial_keys=keys, key_space=keys, block_count=1, transactions_per_block=1536)
            config["cases"] = [dict(case, id=f'{case["id"]}-l{limit or "w"}', executors=8,
                                    max_speculative_inflight=limit)
                               for limit in (1, 8, 32, 0) for case in config["cases"]]
            yield name, config
        return
    if args.suite == "placement":
        cells = itertools.product(PROFILES, (1000, 100000), (0, 0.5, 1), STAGES[args.stage][2])
        for profile, units, prefix, seed in cells:
            name = f"{profile}_c{units}_p{int(prefix * 100)}_s{seed}"
            yield name, matrix(base, profile, units, prefix, seed, stage_dir / name, args.stage, args.notes)
        return
    if args.suite == "prefix-scaling":
        profiles = [(h, 0, 1000000) for h in (2, 4, 8)]
        costs = [(p, 100000) for p in (0, 400000, 800000, 1600000, 3200000)]
        seeds = {"pilot": (79,), "repeated": (83, 8383, 838383)}[args.stage]
    else:
        profiles = [(h, cold, 1000000) for h, cold in itertools.product((2, 4, 8), (0, 1000000))]
        profiles.append((4, 1000000, 100000))  # Short-head control with the same transactions.
        costs, seeds = FOLLOWER_COSTS, CONTENTION_SEEDS[args.stage]
    for (hot_keys, cold, head), (prefix, suffix), seed in itertools.product(
            profiles, costs, seeds):
        units = prefix + suffix
        name = f"hot{hot_keys}_cold{cold}_head{head}_c{units}_p{prefix}_s{seed}"
        config = matrix(base, "single-hot", units, prefix / units, seed,
                        stage_dir / name, args.stage, args.notes)
        config["workload"]["synthetic"]["mix"] = contention_mixture(hot_keys, cold, head, prefix, suffix)
        yield name, config


def describe_synthetic(config, suite, name):
    workload = config["workload"]["synthetic"]
    subject = workload["mix"][0 if suite in ("placement", "speculation") else 1]
    compute = subject["compute"]
    row = {"profile": name.rsplit("_c", 1)[0], "seed": workload["seed"],
           "compute_units": compute["max_units"], "prefix_fraction": compute["prefix_fraction"]}
    if suite not in ("placement", "speculation"):
        prefix = int(compute["max_units"] * compute["prefix_fraction"])
        row.update(hot_keys=subject["access"]["hot_keys"],
                   cold_fraction=workload["mix"][2]["weight"],
                   cold_units=workload["mix"][2]["compute"]["max_units"],
                   head_units=workload["mix"][0]["compute"]["max_units"],
                   prefix_units=prefix, suffix_units=compute["max_units"] - prefix)
    return row


def summarize(stage_dir, suite="placement", describe=describe_synthetic):
    rows = []
    comparisons = []
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
        description = describe(config, suite, path.stem)
        for case, case_records in by_case.items():
            policy = (case.rsplit("-p", 1)[0] if suite == "workers" else
                      case.rsplit("-l", 1)[0] if suite == "speculation" else case)
            suffix = case[len(policy):]
            reference = by_case["runtime" + suffix]
            ratios = [r["timing"]["execution_ns"] / b["timing"]["execution_ns"]
                      for r, b in zip(case_records, reference)]
            ratio, low, high = paired_bootstrap(ratios, description["seed"])
            med = statistics.median
            row = {"cell": path.stem, **description, "case": case,
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
            if suite == "smallbank":
                for key in ("transactions", "successful_transactions", "failed_transactions",
                            "final_read_operations", "committed_goodput_per_second"):
                    row[key] = med(r["metrics"][key] for r in case_records)
                row["static_read_keys"] = (med(r["metrics"]["dependency"]["static_read_keys"]
                    for r in case_records) if policy != "runtime" else None)
            if suite == "workers":
                row.update(policy=policy, workers=case_records[0]["case"]["executors"])
                hardware = case_records[0]["provenance"]["hardware"]
                row.update(gomaxprocs=hardware["gomaxprocs"], cpu_allowed_list=hardware["cpu_allowed_list"])
                for key in ("validation_events", "wait_events", "max_rss_bytes"):
                    row[key] = med(r["metrics"][key] for r in case_records)
                for key in ("estimate_suspends", "estimate_suspend_ns", "worker_yields", "idle_parks"):
                    row[key] = med(r["metrics"]["kernel_policy"][key] for r in case_records)
                row["peak_runnable_workers"] = max(r["metrics"]["kernel_policy"]["peak_runnable_workers"]
                                                    for r in case_records)
                row["dependency_wait_ns"] = med(r["metrics"]["dependency"]["wait_ns"] for r in case_records)
                ratios = [r["timing"]["execution_ns"] / b["timing"]["execution_ns"]
                          for r, b in zip(case_records, by_case[policy + "-p8"])]
                row["ratio_to_p8"], row["p8_ci_low"], row["p8_ci_high"] = paired_bootstrap(ratios, description["seed"])
            if suite == "speculation":
                first = case_records[0]
                available = first["metrics"]["speculation_telemetry_available"]
                row.update(policy=policy, workers=first["case"]["executors"],
                           max_speculative_inflight=first["case"]["max_speculative_inflight"],
                           effective_speculation_limit=first["metrics"]["effective_speculation_limit"],
                           speculation_telemetry_available=available,
                           gomaxprocs=first["provenance"]["hardware"]["gomaxprocs"],
                           cpu_allowed_list=first["provenance"]["hardware"]["cpu_allowed_list"])
                for key in ("peak_speculative_inflight", "admission_stall_events", "admission_stall_ns"):
                    row[key] = med(r["metrics"][key] for r in case_records) if available else None
                ratios = [r["timing"]["execution_ns"] / b["timing"]["execution_ns"]
                          for r, b in zip(case_records, by_case[policy + "-lw"])]
                row["ratio_to_lw"], row["lw_ci_low"], row["lw_ci_high"] = paired_bootstrap(ratios, description["seed"])
            rows.append(row)
            if policy == "direct-ready":
                direct_times = [r["timing"]["execution_ns"] for r in case_records]
                for other in ("runtime", "estimate-abort"):
                    other_times = [r["timing"]["execution_ns"] for r in by_case[other + suffix]]
                    pair = [a / b for a, b in zip(direct_times, other_times)]
                    effect, lower, upper = paired_bootstrap(pair, description["seed"])
                    comparisons.append({"cell": path.stem, "profile": row["profile"],
                                        "seed": description["seed"], "comparison_family": f"direct_vs_{other}",
                                        "ratio": effect, "ci_low": lower, "ci_high": upper,
                                        "p_value": exact_sign_test(other_times, direct_times)})
                    if suite == "workers":
                        comparisons[-1]["workers"] = row["workers"]
                    if suite == "speculation":
                        comparisons[-1]["max_speculative_inflight"] = row["max_speculative_inflight"]
    holm_adjust(comparisons)
    write_csv(stage_dir / "summary.csv", rows)
    write_csv(stage_dir / "comparisons.csv", comparisons)
    print(f"Summary: {stage_dir / 'summary.csv'}", flush=True)


def run(args, stage_dir, configuration_factory=configurations, describe=describe_synthetic):
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
    cells = list(configuration_factory(base, args, stage_dir))
    random.Random(20260907).shuffle(cells)
    for index, (name, config) in enumerate(cells, 1):
        cell = stage_dir / name
        cell.mkdir()
        path = stage_dir / "configs" / f"{name}.json"
        path.write_text(json.dumps(config, indent=2) + "\n")
        print(f"[{index}/{len(cells)}] {name}", flush=True)
        with (cell / "run.log").open("w") as log:
            subprocess.run([str(binary), "run", "-config", str(path)], env=env,
                           stdout=log, stderr=subprocess.STDOUT, check=True)
    summarize(stage_dir, args.suite, describe)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_dir", type=Path, help="server project results/runs/<run-id>")
    parser.add_argument("--suite", choices=("placement", "contention", "prefix-scaling", "speculation"), default="placement")
    parser.add_argument("--stage", choices=STAGES, default="pilot")
    parser.add_argument("--notes", default="")
    parser.add_argument("--summarize-only", action="store_true")
    args = parser.parse_args()
    stage_dir = args.run_dir.resolve() / args.stage
    if args.summarize_only:
        summarize(stage_dir, args.suite)
    else:
        run(args, stage_dir)


if __name__ == "__main__":
    main()
