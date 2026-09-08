#!/usr/bin/env python3
"""Prepare/run SmallBank CQ3 comparisons at P=8, L=W on the experiment host."""

import argparse
import copy
import itertools
from pathlib import Path

from run_workload_prefix_experiment import matrix, run, summarize


SEEDS = {"pilot": (131,), "repeated": (137, 13737, 1373737)}
STANDARD = (("balance", 15), ("deposit_checking", 15), ("transact_savings", 15),
            ("send_payment", 25), ("amalgamate", 15), ("write_check", 15))


def transaction(kind, weight, access, prefix=0, suffix=100000, amount=1):
    total = prefix + suffix
    tx = {"type": kind, "weight": weight, "access": access,
          "compute": {"min_units": total, "max_units": total,
                      "prefix_fraction": prefix / total if total else 0}}
    if kind not in ("balance", "amalgamate"):
        tx["amount"] = amount
    return tx


def hotspot(hot, probability=1):
    return {"kind": "hotspot", "hot_accounts": hot, "hot_probability": probability}


def profiles():
    # Full six-transaction reference mix, fixed total work, varying placement.
    for name, access in (("uniform", {"kind": "uniform"}), ("hot8", hotspot(8, 0.95))):
        for prefix in (0, 90000):
            yield f"standard-{name}-p{prefix}", [
                transaction(kind, weight, access, prefix, 100000-prefix)
                for kind, weight in STANDARD], {"min": 1000000, "max": 1000000}

    # A single-account banking subset isolates the earlier costly-prefix
    # mechanism. Roles are independently sampled; no forced predecessor order.
    for hot, (prefix, suffix) in itertools.product((2, 8), ((0, 100000), (90000, 10000), (400000, 100000))):
        mix = [transaction("deposit_checking", 5, hotspot(hot), 0, 1000000),
               transaction("deposit_checking", 45, hotspot(hot), prefix, suffix),
               transaction("balance", 50, hotspot(hot, 0), 0, 100000)]
        yield f"contention-hot{hot}-p{prefix}-s{suffix}", mix, {"min": 1000000, "max": 1000000}

    # Checking is never modified here. Savings writers create potential false
    # predecessors; thresholds produce skip/all/mixed reads without tx failure.
    for amount, prefix in itertools.product((50, 150, 200), (0, 90000)):
        mix = [transaction("transact_savings", 25, hotspot(8), 0, 1000000),
               transaction("check_funds", 75, hotspot(8), prefix, 100000-prefix, amount)]
        yield f"selective-a{amount}-p{prefix}", mix, {"min": 100, "max": 199}


def configurations(base, args, stage_dir):
    for (profile, mix, checking), seed in itertools.product(profiles(), SEEDS[args.stage]):
        name = f"{profile}_s{seed}"
        config = matrix(base, "single-hot", 1, 0, seed, stage_dir / name, args.stage, args.notes)
        config["workload"] = {"smallbank": {
            "seed": seed, "accounts": 10000, "initial_checking": dict(checking),
            "initial_savings": {"min": 1000000, "max": 1000000},
            "block_count": 4, "transactions_per_block": 1536, "mix": copy.deepcopy(mix)}}
        config["cases"] = [dict(case, executors=8, max_speculative_inflight=0) for case in config["cases"]]
        yield name, config


def describe(config, suite, name):
    bank = config["workload"]["smallbank"]
    total = sum(tx["weight"] for tx in bank["mix"])
    writes = sum(tx["weight"] for tx in bank["mix"] if tx["type"] not in ("balance", "check_funds"))
    return {"profile": name.rsplit("_s", 1)[0], "seed": bank["seed"],
            "accounts": bank["accounts"], "blocks": bank["block_count"],
            "block_size": bank["transactions_per_block"], "write_transaction_fraction": writes / total}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_dir", type=Path, help="server repository results/runs/<run-id>")
    parser.add_argument("--stage", choices=SEEDS, default="pilot")
    parser.add_argument("--notes", default="")
    parser.add_argument("--summarize-only", action="store_true")
    args = parser.parse_args()
    args.suite = "smallbank"
    stage_dir = args.run_dir.resolve() / args.stage
    if args.summarize_only:
        summarize(stage_dir, args.suite, describe)
    else:
        run(args, stage_dir, configurations, describe)


if __name__ == "__main__":
    main()
