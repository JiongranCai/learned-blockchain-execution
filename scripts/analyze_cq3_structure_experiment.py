#!/usr/bin/env python3
"""Gate and analyze the CQ3 structure-sensitive experiment."""

from __future__ import annotations

import argparse
import csv
import hashlib
import json
import math
import random
import re
import statistics
import sys
from collections import defaultdict
from datetime import datetime, timezone
from pathlib import Path


PROFILES = ("fanout08", "fanout32", "fanout128", "selective02", "selective08", "selective32")
COMPUTE_LEVELS = (1_000, 100_000)
SEEDS = (20260831, 20260832)
WINDOWS = ("l8", "lw")
PLANS = ("runtime", "direct", "estimate", "direct-estimate", "summary", "summary-estimate")
EXPECTED_CASES = ("serial-oracle", "runtime-l1") + tuple(
    f"{plan}-{window}" for window in WINDOWS for plan in PLANS
)
EFFECTIVE_LIMITS = {"l1": 1, "l8": 8, "lw": 512}
ROUNDS = {"pilot": (1, 3), "formal": (3, 30)}
LABEL_RE = re.compile(r"^(?P<profile>.+)-c(?P<compute_k>\d+)k-s(?P<seed_suffix>\d+)$")
EXPECTED_CODE_COMMIT = "03eb7898067393a95f1cb52778074539816e106c"
EXPECTED_BINARY_SHA256 = "6c804d096a9a639a677670ae2bdd1e02ea34fb4ff08fa481d0defd1e8524581c"


def median(values):
    return statistics.median(values) if values else 0.0


def percentile(sorted_values, probability):
    position = (len(sorted_values) - 1) * probability
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return sorted_values[lower]
    fraction = position - lower
    return sorted_values[lower] * (1 - fraction) + sorted_values[upper] * fraction


def stable_seed(*parts):
    digest = hashlib.sha256("\0".join(parts).encode()).digest()
    return int.from_bytes(digest[:8], "big")


def exact_sign_test(reference, treatment):
    differences = [treatment_value - reference_value for reference_value, treatment_value in zip(reference, treatment)]
    positives = sum(value > 0 for value in differences)
    negatives = sum(value < 0 for value in differences)
    count = positives + negatives
    if count == 0:
        return 1.0
    tail = min(positives, negatives)
    probability = sum(math.comb(count, value) for value in range(tail + 1)) / (2**count)
    return min(1.0, 2 * probability)


def paired_bootstrap(reference, treatment, seed, resamples=10_000):
    ratios = [treatment_value / reference_value for reference_value, treatment_value in zip(reference, treatment)]
    differences = [treatment_value - reference_value for reference_value, treatment_value in zip(reference, treatment)]
    rng = random.Random(seed)
    count = len(ratios)
    ratio_samples = []
    difference_samples = []
    for _ in range(resamples):
        indices = [rng.randrange(count) for _ in range(count)]
        ratio_samples.append(median([ratios[index] for index in indices]))
        difference_samples.append(median([differences[index] for index in indices]))
    ratio_samples.sort()
    difference_samples.sort()
    return {
        "ratio": median(ratios),
        "ratio_ci_low": percentile(ratio_samples, 0.025),
        "ratio_ci_high": percentile(ratio_samples, 0.975),
        "difference_ns": median(differences),
        "difference_ci_low_ns": percentile(difference_samples, 0.025),
        "difference_ci_high_ns": percentile(difference_samples, 0.975),
    }


def holm_adjust(rows):
    grouped = defaultdict(list)
    for index, row in enumerate(rows):
        grouped[row["comparison_family"]].append((index, row["p_value"]))
    for values in grouped.values():
        ordered = sorted(values, key=lambda value: value[1])
        running = 0.0
        count = len(ordered)
        for rank, (index, p_value) in enumerate(ordered):
            running = max(running, min(1.0, (count - rank) * p_value))
            rows[index]["holm_p_value"] = running


def case_parts(case_id):
    if case_id == "serial-oracle":
        return "serial", "serial"
    plan, window = case_id.rsplit("-", 1)
    return plan, window


def parse_label(label):
    match = LABEL_RE.match(label)
    if not match:
        raise ValueError(f"invalid workload label {label}")
    profile = match.group("profile")
    compute_units = int(match.group("compute_k")) * 1000
    seed = 20260800 + int(match.group("seed_suffix"))
    return profile, compute_units, seed


def load_records(run_root, stage):
    files = sorted((run_root / stage / "artifacts").glob("*/*/runs.jsonl"))
    records = []
    for path in files:
        label = path.parent.name
        profile, compute_units, seed = parse_label(label)
        with path.open() as handle:
            for line_number, line in enumerate(handle, 1):
                if not line.strip():
                    continue
                record = json.loads(line)
                record["_label"] = label
                record["_profile"] = profile
                record["_compute_units"] = compute_units
                record["_seed"] = seed
                record["_path"] = str(path.relative_to(run_root))
                record["_line"] = line_number
                records.append(record)
    return files, records


def add_error(errors, record, message):
    errors.append(
        {
            "file": record.get("_path", ""),
            "line": record.get("_line", 0),
            "case": record.get("case", {}).get("id", ""),
            "round": record.get("round"),
            "message": message,
        }
    )


def require_equal(errors, record, actual, expected, label):
    if actual != expected:
        add_error(errors, record, f"{label}: expected {expected!r}, got {actual!r}")


def require_zero(errors, record, dependency, fields):
    for field in fields:
        if dependency.get(field, 0) != 0:
            add_error(errors, record, f"dependency.{field}: expected zero, got {dependency.get(field)!r}")


def require_positive(errors, record, actual, label):
    if not isinstance(actual, (int, float)) or actual <= 0:
        add_error(errors, record, f"{label}: expected positive value, got {actual!r}")


def gate(files, records, stage):
    warmups, measurements = ROUNDS[stage]
    errors = []
    expected_files = len(PROFILES) * len(COMPUTE_LEVELS) * len(SEEDS)
    if len(files) != expected_files:
        errors.append({"message": f"expected {expected_files} files, found {len(files)}"})

    grouped = defaultdict(list)
    hashes = defaultdict(set)
    for record in records:
        case_id = record.get("case", {}).get("id")
        grouped[(record["_label"], case_id, record.get("phase"))].append(record)
        hashes[record["_label"]].add(record.get("provenance", {}).get("workload_hash"))
        if record["_profile"] not in PROFILES or record["_compute_units"] not in COMPUTE_LEVELS or record["_seed"] not in SEEDS:
            add_error(errors, record, "unexpected workload axes")
        if case_id not in EXPECTED_CASES:
            add_error(errors, record, "unexpected case")
            continue
        require_equal(errors, record, record.get("status"), "success", "status")
        require_equal(errors, record, record.get("censored"), False, "censored")
        require_equal(errors, record, record.get("canonical_match"), True, "canonical_match")
        provenance = record.get("provenance", {})
        require_equal(errors, record, provenance.get("code_commit"), EXPECTED_CODE_COMMIT, "code_commit")
        require_equal(errors, record, provenance.get("code_modified"), False, "code_modified")
        require_equal(errors, record, provenance.get("binary_sha256"), EXPECTED_BINARY_SHA256, "binary_sha256")
        require_equal(errors, record, provenance.get("generator_version"), "synthetic-v3", "generator_version")
        require_equal(errors, record, provenance.get("generator_seed"), record["_seed"], "generator_seed")
        hardware = provenance.get("hardware", {})
        require_equal(errors, record, hardware.get("logical_cpus"), 8, "logical_cpus")
        require_equal(errors, record, hardware.get("cpu_allowed_list"), "0-7", "cpu_allowed_list")
        require_equal(errors, record, hardware.get("cpu_governor"), "performance", "cpu_governor")
        require_equal(errors, record, record.get("metrics", {}).get("blocks"), 2, "metrics.blocks")
        require_equal(errors, record, record.get("metrics", {}).get("transactions"), 1024, "metrics.transactions")

        plan, window = case_parts(case_id)
        metrics = record.get("metrics", {})
        dependency = metrics.get("dependency", {})
        if plan == "serial":
            require_equal(errors, record, record["case"].get("engine"), "serial", "engine")
            require_equal(errors, record, metrics.get("effective_speculation_limit"), 1, "effective_speculation_limit")
            require_equal(errors, record, metrics.get("speculation_limit_applied"), False, "speculation_limit_applied")
            require_equal(errors, record, metrics.get("reexecution_attempts"), 0, "serial reexecution_attempts")
        else:
            require_equal(errors, record, record["case"].get("engine"), "blockstm", "engine")
            require_equal(errors, record, metrics.get("effective_speculation_limit"), EFFECTIVE_LIMITS[window], "effective_speculation_limit")
            require_equal(errors, record, metrics.get("speculation_limit_applied"), window != "lw", "speculation_limit_applied")
            if window == "l1":
                require_equal(errors, record, metrics.get("reexecution_attempts"), 0, "L=1 reexecution_attempts")

        zero_consumer = (
            "resolution_ns",
            "plan_lookups",
            "traversal_steps",
            "wait_events",
            "wait_ns",
            "estimate_build_ns",
            "estimated_write_locations",
            "estimated_write_key_bytes",
        )
        if plan in ("serial", "runtime"):
            require_equal(errors, record, dependency.get("acquisition_disposition"), "runtime_kernel", "acquisition_disposition")
            require_equal(errors, record, dependency.get("acquisition_measured"), False, "acquisition_measured")
            require_equal(errors, record, dependency.get("representation_measured"), False, "representation_measured")
            require_equal(errors, record, dependency.get("resolution_measured"), False, "resolution_measured")
            require_zero(errors, record, dependency, ("acquisition_ns", "representation_ns") + zero_consumer)
        elif plan == "estimate":
            require_equal(errors, record, dependency.get("acquisition_disposition"), "acquisition_consumed", "acquisition_disposition")
            require_equal(errors, record, dependency.get("acquisition_measured"), True, "acquisition_measured")
            require_equal(errors, record, dependency.get("representation_measured"), False, "representation_measured")
            require_equal(errors, record, dependency.get("resolution_measured"), False, "resolution_measured")
            require_equal(errors, record, dependency.get("estimated_write_locations"), 1024, "estimated_write_locations")
            require_positive(errors, record, dependency.get("estimate_build_ns"), "dependency.estimate_build_ns")
            require_zero(errors, record, dependency, ("representation_ns", "resolution_ns", "plan_lookups", "traversal_steps", "wait_events", "wait_ns"))
        else:
            require_equal(errors, record, dependency.get("acquisition_disposition"), "representation_consumed", "acquisition_disposition")
            require_equal(errors, record, dependency.get("acquisition_measured"), True, "acquisition_measured")
            require_equal(errors, record, dependency.get("representation_measured"), True, "representation_measured")
            require_equal(errors, record, dependency.get("resolution_measured"), True, "resolution_measured")
            if plan in ("direct", "summary"):
                require_zero(errors, record, dependency, ("estimate_build_ns", "estimated_write_locations", "estimated_write_key_bytes"))
            else:
                require_equal(errors, record, dependency.get("estimated_write_locations"), 1024, "estimated_write_locations")
                require_positive(errors, record, dependency.get("estimate_build_ns"), "dependency.estimate_build_ns")

        case = record["case"]
        if plan in ("serial", "runtime"):
            require_equal(errors, record, case.get("dependency_source"), "runtime_observed", "dependency_source")
            require_equal(errors, record, case.get("dependency_representation"), "version_only", "dependency_representation")
            require_equal(errors, record, case.get("dependency_wait_policy"), "none", "dependency_wait_policy")
            require_equal(errors, record, case.get("dependency_estimate_injection"), "none", "dependency_estimate_injection")
        elif plan == "estimate":
            require_equal(errors, record, case.get("dependency_source"), "static_program", "dependency_source")
            require_equal(errors, record, case.get("dependency_representation"), "version_only", "dependency_representation")
            require_equal(errors, record, case.get("dependency_wait_policy"), "none", "dependency_wait_policy")
            require_equal(errors, record, case.get("dependency_estimate_injection"), "write_estimates", "dependency_estimate_injection")
        else:
            base_plan = plan.removesuffix("-estimate")
            expected_representation = "raw_last_writer" if base_plan == "direct" else "max_raw_predecessor"
            expected_wait = "direct_predecessor_wait" if base_plan == "direct" else "contiguous_frontier_wait"
            expected_estimates = "write_estimates" if plan.endswith("-estimate") else "none"
            require_equal(errors, record, case.get("dependency_source"), "static_program", "dependency_source")
            require_equal(errors, record, case.get("dependency_representation"), expected_representation, "dependency_representation")
            require_equal(errors, record, case.get("dependency_wait_policy"), expected_wait, "dependency_wait_policy")
            require_equal(errors, record, case.get("dependency_estimate_injection"), expected_estimates, "dependency_estimate_injection")

    labels = [
        f"{profile}-c{compute_units // 1000:03d}k-s{seed % 100:02d}"
        for profile in PROFILES
        for compute_units in COMPUTE_LEVELS
        for seed in SEEDS
    ]
    for label in labels:
        for case_id in EXPECTED_CASES:
            for phase, expected in (("warmup", warmups), ("measurement", measurements)):
                actual = len(grouped[(label, case_id, phase)])
                if actual != expected:
                    errors.append({"message": f"{label}/{case_id}/{phase}: expected {expected}, got {actual}"})
        if len(hashes[label]) != 1 or None in hashes[label]:
            errors.append({"message": f"{label}: expected one non-null workload hash, got {sorted(str(value) for value in hashes[label])}"})
    distinct_hashes = {next(iter(value)) for value in hashes.values() if len(value) == 1}
    if len(distinct_hashes) != expected_files:
        errors.append({"message": f"expected {expected_files} distinct workload hashes, got {len(distinct_hashes)}"})

    expected_records = expected_files * len(EXPECTED_CASES) * (warmups + measurements)
    return {
        "status": "PASS" if not errors else "FAIL",
        "stage": stage,
        "files": len(files),
        "records": len(records),
        "expected_records": expected_records,
        "measurement_records": sum(record.get("phase") == "measurement" for record in records),
        "errors": errors,
    }


def usable_records(records):
    return [
        record
        for record in records
        if record.get("phase") == "measurement"
        and record.get("status") == "success"
        and not record.get("censored")
        and record.get("canonical_match")
    ]


def summarize(records):
    grouped = defaultdict(list)
    for record in usable_records(records):
        grouped[(record["_label"], record["case"]["id"])].append(record)
    rows = []
    for (label, case_id), values in sorted(grouped.items()):
        first = values[0]
        plan, window = case_parts(case_id)
        metrics = [record["metrics"] for record in values]
        dependency = [value["dependency"] for value in metrics]
        execution = [record["timing"]["execution_ns"] for record in values]
        rows.append(
            {
                "workload": label,
                "profile": first["_profile"],
                "compute_units": first["_compute_units"],
                "seed": first["_seed"],
                "case": case_id,
                "plan": plan,
                "window": window,
                "n": len(values),
                "median_execution_ms": median(execution) / 1e6,
                "median_tps": median([value["completed_transactions_per_second"] for value in metrics]),
                "median_reexecution_attempts": median([value["reexecution_attempts"] for value in metrics]),
                "median_reexecuted_units": median([value["reexecuted_execution_units"] for value in metrics]),
                "median_acquisition_ms": median([value.get("acquisition_ns", 0) for value in dependency]) / 1e6,
                "median_representation_ms": median([value.get("representation_ns", 0) for value in dependency]) / 1e6,
                "median_resolution_worker_ms": median([value.get("resolution_ns", 0) for value in dependency]) / 1e6,
                "median_wait_worker_ms": median([value.get("wait_ns", 0) for value in dependency]) / 1e6,
                "median_wait_events": median([value.get("wait_events", 0) for value in dependency]),
                "median_dependency_edges": median([value.get("dependency_edges", 0) for value in dependency]),
                "median_max_fan_in": median([value.get("representation_max_fan_in", 0) for value in dependency]),
                "median_estimate_build_ms": median([value.get("estimate_build_ns", 0) for value in dependency]) / 1e6,
            }
        )
    return rows


def comparison_definitions():
    definitions = []
    for window in WINDOWS:
        runtime = f"runtime-{window}"
        definitions.extend(
            [
                ("use", f"direct_vs_runtime_{window}", runtime, f"direct-{window}"),
                ("use", f"summary_vs_runtime_{window}", runtime, f"summary-{window}"),
                ("acquisition", f"estimate_vs_runtime_{window}", runtime, f"estimate-{window}"),
                ("source-choice", f"estimate_vs_direct_{window}", f"direct-{window}", f"estimate-{window}"),
                ("representation", f"summary_vs_direct_{window}", f"direct-{window}", f"summary-{window}"),
                (
                    "representation",
                    f"summary_estimate_vs_direct_estimate_{window}",
                    f"direct-estimate-{window}",
                    f"summary-estimate-{window}",
                ),
                (
                    "acquisition-addon",
                    f"direct_estimate_vs_direct_{window}",
                    f"direct-{window}",
                    f"direct-estimate-{window}",
                ),
                (
                    "acquisition-addon",
                    f"summary_estimate_vs_summary_{window}",
                    f"summary-{window}",
                    f"summary-estimate-{window}",
                ),
                (
                    "use-addon",
                    f"direct_estimate_vs_estimate_{window}",
                    f"estimate-{window}",
                    f"direct-estimate-{window}",
                ),
                (
                    "use-addon",
                    f"summary_estimate_vs_estimate_{window}",
                    f"estimate-{window}",
                    f"summary-estimate-{window}",
                ),
                (
                    "combined-vs-runtime",
                    f"direct_estimate_vs_runtime_{window}",
                    runtime,
                    f"direct-estimate-{window}",
                ),
                (
                    "combined-vs-runtime",
                    f"summary_estimate_vs_runtime_{window}",
                    runtime,
                    f"summary-estimate-{window}",
                ),
            ]
        )
    definitions.extend(
        [
            ("serial-control", "runtime_l1_vs_serial", "serial-oracle", "runtime-l1"),
            ("serial-control", "runtime_l8_vs_l1", "runtime-l1", "runtime-l8"),
            ("serial-control", "runtime_lw_vs_l1", "runtime-l1", "runtime-lw"),
            ("runtime-window", "runtime_l8_vs_lw", "runtime-lw", "runtime-l8"),
        ]
    )
    return definitions


def compare(records):
    indexed = {}
    for record in usable_records(records):
        indexed[(record["_label"], record["case"]["id"], record["round"])] = record
    labels = sorted({record["_label"] for record in records})
    rows = []
    for label in labels:
        profile, compute_units, seed = parse_label(label)
        for family, comparison, reference, treatment in comparison_definitions():
            reference_rounds = {
                round_number for current_label, case_id, round_number in indexed if current_label == label and case_id == reference
            }
            treatment_rounds = {
                round_number for current_label, case_id, round_number in indexed if current_label == label and case_id == treatment
            }
            rounds = sorted(reference_rounds & treatment_rounds)
            reference_records = [indexed[(label, reference, round_number)] for round_number in rounds]
            treatment_records = [indexed[(label, treatment, round_number)] for round_number in rounds]
            reference_times = [record["timing"]["execution_ns"] for record in reference_records]
            treatment_times = [record["timing"]["execution_ns"] for record in treatment_records]
            bootstrap = paired_bootstrap(reference_times, treatment_times, stable_seed(label, comparison))
            reference_reexecution = [record["metrics"]["reexecution_attempts"] for record in reference_records]
            treatment_reexecution = [record["metrics"]["reexecution_attempts"] for record in treatment_records]
            rows.append(
                {
                    "comparison_family": family,
                    "workload": label,
                    "profile": profile,
                    "compute_units": compute_units,
                    "seed": seed,
                    "comparison": comparison,
                    "reference": reference,
                    "treatment": treatment,
                    "n_pairs": len(rounds),
                    "reference_median_ms": median(reference_times) / 1e6,
                    "treatment_median_ms": median(treatment_times) / 1e6,
                    "paired_median_ratio": bootstrap["ratio"],
                    "paired_median_delta_pct": (bootstrap["ratio"] - 1) * 100,
                    "delta_pct_ci_low": (bootstrap["ratio_ci_low"] - 1) * 100,
                    "delta_pct_ci_high": (bootstrap["ratio_ci_high"] - 1) * 100,
                    "paired_median_difference_ms": bootstrap["difference_ns"] / 1e6,
                    "difference_ci_low_ms": bootstrap["difference_ci_low_ns"] / 1e6,
                    "difference_ci_high_ms": bootstrap["difference_ci_high_ns"] / 1e6,
                    "p_value": exact_sign_test(reference_times, treatment_times),
                    "holm_p_value": math.nan,
                    "material_5pct": abs(bootstrap["ratio"] - 1) >= 0.05,
                    "ratio_ci_excludes_1": bootstrap["ratio_ci_low"] > 1 or bootstrap["ratio_ci_high"] < 1,
                    "paired_median_reexecution_saved": median(
                        [reference_value - treatment_value for reference_value, treatment_value in zip(reference_reexecution, treatment_reexecution)]
                    ),
                }
            )
    holm_adjust(rows)
    for row in rows:
        row["holm_significant_0_05"] = row["holm_p_value"] < 0.05
    return rows


def rankings(summary_rows):
    grouped = defaultdict(list)
    for row in summary_rows:
        grouped[row["workload"]].append(row)
    rows = []
    for label, values in sorted(grouped.items()):
        ordered = sorted(values, key=lambda value: value["median_execution_ms"])
        best_runtime = min(
            (value for value in values if value["plan"] == "runtime"),
            key=lambda value: value["median_execution_ms"],
        )
        runtime_lw = next(value for value in values if value["case"] == "runtime-lw")
        for rank, value in enumerate(ordered, 1):
            rows.append(
                {
                    "workload": label,
                    "profile": value["profile"],
                    "compute_units": value["compute_units"],
                    "seed": value["seed"],
                    "rank": rank,
                    "case": value["case"],
                    "plan": value["plan"],
                    "window": value["window"],
                    "median_execution_ms": value["median_execution_ms"],
                    "ratio_to_runtime_lw": value["median_execution_ms"] / runtime_lw["median_execution_ms"],
                    "best_runtime_case": best_runtime["case"],
                    "ratio_to_best_runtime": value["median_execution_ms"] / best_runtime["median_execution_ms"],
                }
            )
    return rows


def seed_consistency(comparison_rows):
    grouped = defaultdict(list)
    for row in comparison_rows:
        key = (row["comparison_family"], row["profile"], row["compute_units"], row["comparison"])
        grouped[key].append(row)
    rows = []
    for (family, profile, compute_units, comparison), values in sorted(grouped.items()):
        values = sorted(values, key=lambda value: value["seed"])
        ratios = [value["paired_median_ratio"] for value in values]
        rows.append(
            {
                "comparison_family": family,
                "profile": profile,
                "compute_units": compute_units,
                "comparison": comparison,
                "reference": values[0]["reference"],
                "treatment": values[0]["treatment"],
                "seed_count": len(values),
                "seed_ratios": ";".join(f"{value['seed']}:{value['paired_median_ratio']:.6f}" for value in values),
                "geometric_mean_ratio": math.exp(sum(math.log(value) for value in ratios) / len(ratios)),
                "same_direction": all(value > 1 for value in ratios) or all(value < 1 for value in ratios),
                "all_material": all(abs(value - 1) >= 0.05 for value in ratios),
                "all_ci_exclude_1": all(value["ratio_ci_excludes_1"] for value in values),
                "all_holm_significant": all(value["holm_significant_0_05"] for value in values),
            }
        )
    return rows


def primary_effects(consistency_rows):
    rows = []
    for row in consistency_rows:
        profile = row["profile"]
        comparison = row["comparison"]
        is_primary = (
            (profile.startswith("fanout") and comparison.startswith("summary_vs_direct_"))
            or (profile.startswith("fanout") and comparison.startswith("summary_estimate_vs_direct_estimate_"))
            or (profile.startswith("selective") and comparison.startswith("estimate_vs_direct_"))
            or (profile.startswith("selective") and comparison.startswith("estimate_vs_runtime_"))
            or (profile.startswith("selective") and comparison.startswith("direct_estimate_vs_direct_"))
            or (profile.startswith("selective") and comparison.startswith("summary_estimate_vs_summary_"))
        )
        if is_primary:
            rows.append(row)
    return rows


def write_csv(path, rows):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="") as handle:
        if not rows:
            return
        writer = csv.DictWriter(handle, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)


def strong_win(row):
    return (
        row["geometric_mean_ratio"] < 0.95
        and row["same_direction"]
        and row["all_ci_exclude_1"]
        and row["all_holm_significant"]
    )


def strong_loss(row):
    return (
        row["geometric_mean_ratio"] > 1.05
        and row["same_direction"]
        and row["all_ci_exclude_1"]
        and row["all_holm_significant"]
    )


def effect_label(row):
    if strong_win(row):
        return "treatment wins"
    if strong_loss(row):
        return "treatment loses"
    return "mixed / <5%"


def write_report(path, stage, gate_result, ranking_rows, consistency_rows):
    top = [row for row in ranking_rows if row["rank"] <= 3]
    lines = [
        f"# CQ3 structure-sensitive experiment — {stage}",
        "",
        f"- Gate: **{gate_result['status']}**",
        f"- Records: {gate_result['records']} ({gate_result['measurement_records']} measurement)",
        "- Positive ratios/deltas mean the treatment is slower.",
        "- Formal intervals use 10,000-resample paired percentile bootstrap; exact paired sign tests receive Holm correction by preregistered family.",
        "",
        "## Top three cases per workload artifact",
        "",
        "| Workload | Rank | Case | Median ms | vs runtime-LW | Best runtime | vs best runtime |",
        "|---|---:|---|---:|---:|---|---:|",
    ]
    for row in top:
        lines.append(
            f"| {row['workload']} | {row['rank']} | {row['case']} | {row['median_execution_ms']:.2f} | "
            f"{(row['ratio_to_runtime_lw'] - 1) * 100:+.1f}% | {row['best_runtime_case']} | "
            f"{(row['ratio_to_best_runtime'] - 1) * 100:+.1f}% |"
        )
    consistent_wins = [row for row in consistency_rows if strong_win(row)]
    lines.extend(
        [
            "",
            "## Seed-consistent material treatment wins",
            "",
            "These rows require both seeds to favor the treatment, both paired intervals to exclude ratio 1, both Holm-adjusted sign tests to pass, and a geometric-mean effect of at least 5%.",
            "",
            "| Family | Profile | Compute | Comparison | Geomean delta | Seed ratios |",
            "|---|---|---:|---|---:|---|",
        ]
    )
    for row in consistent_wins:
        lines.append(
            f"| {row['comparison_family']} | {row['profile']} | {row['compute_units']} | {row['comparison']} | "
            f"{(row['geometric_mean_ratio'] - 1) * 100:+.1f}% | {row['seed_ratios']} |"
        )
    lines.extend(
        [
            "",
            "Machine-readable files: `gate.json`, `summary.csv`, `paired-comparisons.csv`, `rankings.csv`, `seed-consistency.csv`, and `primary-effects.csv`.",
            "",
        ]
    )
    path.write_text("\n".join(lines), encoding="utf-8")


def write_synthesis(path, gate_result, ranking_rows, consistency_rows):
    fanout = sorted(
        (
            row
            for row in consistency_rows
            if row["profile"].startswith("fanout") and row["comparison"].startswith("summary_vs_direct_")
        ),
        key=lambda row: (int(row["profile"].removeprefix("fanout")), row["compute_units"], row["comparison"]),
    )
    selective = sorted(
        (
            row
            for row in consistency_rows
            if row["profile"].startswith("selective") and row["comparison"].startswith("estimate_vs_direct_")
        ),
        key=lambda row: (int(row["profile"].removeprefix("selective")), row["compute_units"], row["comparison"]),
    )
    selective_runtime = sorted(
        (
            row
            for row in consistency_rows
            if row["profile"].startswith("selective") and row["comparison"].startswith("estimate_vs_runtime_")
        ),
        key=lambda row: (int(row["profile"].removeprefix("selective")), row["compute_units"], row["comparison"]),
    )
    winner_counts = defaultdict(int)
    for row in ranking_rows:
        if row["rank"] == 1:
            winner_counts[row["case"]] += 1
    winner_text = ", ".join(
        f"{case}={count}" for case, count in sorted(winner_counts.items(), key=lambda item: (-item[1], item[0]))
    )

    lines = [
        "# CQ3 structure-sensitive experiment — synthesis",
        "",
        f"- Gate: **{gate_result['status']}**; {gate_result['measurement_records']} formal measurements.",
        f"- Fan-out summary vs direct: {sum(strong_win(row) for row in fanout)}/{len(fanout)} seed-consistent material wins.",
        f"- Selective estimate vs direct: {sum(strong_win(row) for row in selective)}/{len(selective)} seed-consistent material wins.",
        f"- Selective estimate vs runtime MVCC: {sum(strong_win(row) for row in selective_runtime)}/{len(selective_runtime)} wins; this separates lower static-guidance overhead from beating the runtime baseline.",
        f"- Rank-1 cases across 24 workload artifacts: {winner_text}.",
        "- `full_graph` is excluded: the current adapter has no ready-queue, wave, reordering, or other consumer that can exploit the complete graph beyond entry-time all-predecessor waits.",
        "- Ratios below 1 favor the treatment; each row aggregates two seeds. “treatment wins/loses” also requires >=5%, both CIs excluding 1, and Holm-adjusted p<0.05 for both seeds.",
        "",
        "## Representation: shared fan-out barrier",
        "",
        "| F | Compute | Window | Summary/direct | Decision |",
        "|---:|---:|---|---:|---|",
    ]
    for row in fanout:
        lines.append(
            f"| {int(row['profile'].removeprefix('fanout'))} | {row['compute_units']} | {row['comparison'].rsplit('_', 1)[1].upper()} | "
            f"{row['geometric_mean_ratio']:.3f} ({(row['geometric_mean_ratio'] - 1) * 100:+.1f}%) | {effect_label(row)} |"
        )
    lines.extend(
        [
            "",
            "## Acquisition choice: selective concrete access",
            "",
            "| Candidates | Compute | Window | Estimate/direct | Estimate/runtime |",
            "|---:|---:|---|---:|---:|",
        ]
    )
    runtime_index = {
        (row["profile"], row["compute_units"], row["comparison"].rsplit("_", 1)[1]): row
        for row in selective_runtime
    }
    for row in selective:
        window = row["comparison"].rsplit("_", 1)[1]
        runtime_row = runtime_index[(row["profile"], row["compute_units"], window)]
        lines.append(
            f"| {int(row['profile'].removeprefix('selective'))} | {row['compute_units']} | {window.upper()} | "
            f"{row['geometric_mean_ratio']:.3f} ({effect_label(row)}) | "
            f"{runtime_row['geometric_mean_ratio']:.3f} ({effect_label(runtime_row)}) |"
        )
    lines.extend(
        [
            "",
            "## Reading the result",
            "",
            "- The representation claim is supported only where one frontier entry replaces many equivalent direct predecessors; small F is the overhead boundary.",
            "- The selective claim shows why conservative static dependency acquisition should not be forced onto a program with many candidate reads. It is not automatically evidence that write estimates beat runtime MVCC.",
            "- Combination effects remain in `primary-effects.csv`; no combination is credited unless its matched contrast passes the same two-seed rule.",
            "",
        ]
    )
    path.write_text("\n".join(lines), encoding="utf-8")


def finalize_manifest(run_root, gate_result):
    manifest_path = run_root / "manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    manifest["status"] = "completed"
    manifest["completed_at_utc"] = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    manifest["formal_gate"] = {
        "status": gate_result["status"],
        "files": gate_result["files"],
        "records": gate_result["records"],
        "measurement_records": gate_result["measurement_records"],
    }
    pilot_gate_path = run_root / "pilot" / "analysis" / "gate.json"
    if pilot_gate_path.exists():
        pilot_gate = json.loads(pilot_gate_path.read_text(encoding="utf-8"))
        manifest["pilot_gate"] = {
            "status": pilot_gate["status"],
            "files": pilot_gate["files"],
            "records": pilot_gate["records"],
            "measurement_records": pilot_gate["measurement_records"],
        }
    manifest["analysis_outputs"] = [
        "formal/analysis/gate.json",
        "formal/analysis/summary.csv",
        "formal/analysis/paired-comparisons.csv",
        "formal/analysis/rankings.csv",
        "formal/analysis/seed-consistency.csv",
        "formal/analysis/primary-effects.csv",
        "formal/analysis/REPORT.md",
        "formal/analysis/SYNTHESIS.md",
    ]
    manifest["excluded_mechanisms"] = {
        "full_graph": "No current ready-queue, wave, reordering, or other consumer can exploit a complete graph beyond entry-time all-predecessor waits."
    }
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("run_root", type=Path)
    parser.add_argument("stage", choices=ROUNDS)
    args = parser.parse_args()

    files, records = load_records(args.run_root, args.stage)
    gate_result = gate(files, records, args.stage)
    analysis_dir = args.run_root / args.stage / "analysis"
    analysis_dir.mkdir(parents=True, exist_ok=True)
    (analysis_dir / "gate.json").write_text(
        json.dumps(gate_result, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    if gate_result["status"] != "PASS":
        print(json.dumps({"stage": args.stage, "gate": "FAIL", "records": gate_result["records"], "errors": len(gate_result["errors"])}))
        return 2
    summary_rows = summarize(records)
    comparison_rows = compare(records)
    ranking_rows = rankings(summary_rows)
    consistency_rows = seed_consistency(comparison_rows)
    primary_rows = primary_effects(consistency_rows)
    write_csv(analysis_dir / "summary.csv", summary_rows)
    write_csv(analysis_dir / "paired-comparisons.csv", comparison_rows)
    write_csv(analysis_dir / "rankings.csv", ranking_rows)
    write_csv(analysis_dir / "seed-consistency.csv", consistency_rows)
    write_csv(analysis_dir / "primary-effects.csv", primary_rows)
    write_report(analysis_dir / "REPORT.md", args.stage, gate_result, ranking_rows, consistency_rows)
    write_synthesis(analysis_dir / "SYNTHESIS.md", gate_result, ranking_rows, consistency_rows)
    if args.stage == "formal":
        finalize_manifest(args.run_root, gate_result)
    print(json.dumps({"stage": args.stage, "gate": gate_result["status"], "records": gate_result["records"], "errors": len(gate_result["errors"])}))
    return 0 if gate_result["status"] == "PASS" else 2


if __name__ == "__main__":
    sys.exit(main())
