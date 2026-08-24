#!/usr/bin/env python3
"""Gate and summarize the CQ3 ready-queue Linux experiment."""

from __future__ import annotations

import argparse
import csv
import hashlib
import json
import math
import random
import statistics
from collections import defaultdict
from datetime import datetime, timezone
from pathlib import Path


REPO = Path(__file__).resolve().parent.parent
ROUNDS = {"pilot": (1, 3), "formal": (3, 30)}
COMPARISONS = (
    ("acquisition", "static_only_vs_runtime", "runtime", "static-only"),
    ("raw-representation", "raw_none_vs_static_only", "static-only", "raw-none"),
    ("direct-consumer", "direct_ready_vs_raw_none", "raw-none", "direct-ready"),
    ("estimate-consumer", "estimate_abort_vs_raw_none", "raw-none", "estimate-abort"),
    ("summary-representation", "summary_none_vs_static_only", "static-only", "summary-none"),
    ("frontier-consumer", "frontier_ready_vs_summary_none", "summary-none", "frontier-ready"),
    ("full-representation", "full_none_vs_static_only", "static-only", "full-none"),
    ("full-consumer", "full_ready_vs_full_none", "full-none", "full-ready"),
    ("plan-choice", "estimate_abort_vs_direct_ready", "direct-ready", "estimate-abort"),
    ("plan-choice", "frontier_ready_vs_direct_ready", "direct-ready", "frontier-ready"),
    ("plan-choice", "full_ready_vs_direct_ready", "direct-ready", "full-ready"),
    ("end-to-end", "direct_ready_vs_runtime", "runtime", "direct-ready"),
    ("end-to-end", "estimate_abort_vs_runtime", "runtime", "estimate-abort"),
    ("end-to-end", "frontier_ready_vs_runtime", "runtime", "frontier-ready"),
    ("end-to-end", "full_ready_vs_runtime", "runtime", "full-ready"),
)


def median(values):
    return statistics.median(values) if values else 0.0


def percentile(values, probability):
    ordered = sorted(values)
    position = (len(ordered) - 1) * probability
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    fraction = position - lower
    return ordered[lower] * (1 - fraction) + ordered[upper] * fraction


def stable_seed(*parts):
    digest = hashlib.sha256("\0".join(str(part) for part in parts).encode()).digest()
    return int.from_bytes(digest[:8], "big")


def paired_bootstrap(ratios, seed, resamples=10_000):
    rng = random.Random(seed)
    count = len(ratios)
    samples = []
    for _ in range(resamples):
        samples.append(median([ratios[rng.randrange(count)] for _ in range(count)]))
    return median(ratios), percentile(samples, 0.025), percentile(samples, 0.975)


def exact_sign_test(reference, treatment):
    differences = [right - left for left, right in zip(reference, treatment)]
    positives = sum(value > 0 for value in differences)
    negatives = sum(value < 0 for value in differences)
    count = positives + negatives
    if count == 0:
        return 1.0
    tail = min(positives, negatives)
    probability = sum(math.comb(count, value) for value in range(tail + 1)) / (2**count)
    return min(1.0, 2 * probability)


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


def write_csv(path, rows):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="", encoding="utf-8") as handle:
        if not rows:
            return
        writer = csv.DictWriter(handle, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)


def add_error(errors, label, message):
    errors.append({"workload": label, "message": message})


def load_stage(run_root, stage):
    configs = []
    records = []
    validations = []
    config_paths = sorted((run_root / "configs" / stage).glob("*/*.json"))
    for config_path in config_paths:
        config = json.loads(config_path.read_text(encoding="utf-8"))
        label = config_path.stem
        kind = config_path.parent.name
        synthetic = config["workload"]["synthetic"]
        metadata = {
            "label": label,
            "kind": kind,
            "profile": label.split("-c", 1)[0],
            "compute_units": synthetic["max_compute_units"],
            "seed": synthetic["seed"],
            "config": config,
        }
        configs.append(metadata)
        for output_key, target in (("run_records", records), ("validation_records", validations)):
            output_path = REPO / config["output"][output_key]
            if not output_path.is_file():
                continue
            with output_path.open(encoding="utf-8") as handle:
                for line_number, line in enumerate(handle, 1):
                    if not line.strip():
                        continue
                    record = json.loads(line)
                    record.update(
                        {
                            "_label": label,
                            "_kind": kind,
                            "_profile": metadata["profile"],
                            "_compute_units": metadata["compute_units"],
                            "_seed": metadata["seed"],
                            "_line": line_number,
                        }
                    )
                    target.append(record)
    return configs, records, validations


def gate_stage(run_root, stage, configs, records, validations):
    warmups, measurements = ROUNDS[stage]
    errors = []
    expected_config_count = json.loads((run_root / "manifest.json").read_text(encoding="utf-8"))[
        "matrix_count_per_stage"
    ]
    if len(configs) != expected_config_count:
        add_error(errors, "", f"expected {expected_config_count} configs, found {len(configs)}")

    records_by_key = defaultdict(list)
    validations_by_label = defaultdict(list)
    expected_records = 0
    expected_measurements = 0
    workload_hashes = defaultdict(set)
    code_commits = set()
    binary_hashes = set()
    mechanism_totals = defaultdict(int)

    config_by_label = {value["label"]: value for value in configs}
    for value in configs:
        case_count = len(value["config"]["cases"])
        expected_records += case_count * (warmups + measurements)
        expected_measurements += case_count * measurements

    for record in validations:
        validations_by_label[record["_label"]].append(record)
        if record.get("status") != "success" or not record.get("canonical_match"):
            add_error(errors, record["_label"], f"validation failed for {record.get('case', {}).get('id')}")

    for record in records:
        label = record["_label"]
        case = record.get("case", {})
        case_id = case.get("id")
        phase = record.get("phase")
        records_by_key[(label, case_id, phase)].append(record)
        if label not in config_by_label:
            add_error(errors, label, "record has no matching config")
            continue
        expected_cases = {item["id"]: item for item in config_by_label[label]["config"]["cases"]}
        if case_id not in expected_cases:
            add_error(errors, label, f"unexpected case {case_id}")
            continue
        if record.get("schema_version") != "benchmark-run-v7":
            add_error(errors, label, f"unexpected record schema {record.get('schema_version')}")
        if record.get("status") != "success" or record.get("censored") or not record.get("canonical_match"):
            add_error(errors, label, f"unsuccessful record {case_id}/{phase}/{record.get('round')}")

        provenance = record.get("provenance", {})
        code_commits.add(provenance.get("code_commit"))
        binary_hashes.add(provenance.get("binary_sha256"))
        workload_hashes[label].add(provenance.get("workload_hash"))
        if provenance.get("code_modified") is not False:
            add_error(errors, label, f"dirty binary provenance for {case_id}")
        hardware = provenance.get("hardware", {})
        for field, expected in (
            ("logical_cpus", 8),
            ("gomaxprocs", 8),
            ("cpu_allowed_list", "0-7"),
            ("cpu_governor", "performance"),
        ):
            if hardware.get(field) != expected:
                add_error(errors, label, f"hardware.{field}: expected {expected!r}, got {hardware.get(field)!r}")

        metrics = record.get("metrics", {})
        if metrics.get("blocks") != 2 or metrics.get("transactions") != 1024:
            add_error(errors, label, f"unexpected block/transaction count for {case_id}")
        if case.get("max_speculative_inflight") != 0:
            add_error(errors, label, f"{case_id} does not use L=W")
        if case.get("estimate_read_policy") != "abort_and_reschedule" or case.get("idle_wait_policy") != "park":
            add_error(errors, label, f"{case_id} does not use the frozen CQ3 kernel recipe")
        expected_dispatch = "ready_queue" if case.get("dependency_wait_policy") != "none" else "index_order"
        if case.get("dependency_dispatch") != expected_dispatch:
            add_error(errors, label, f"{case_id} has dispatch {case.get('dependency_dispatch')}, want {expected_dispatch}")

        kernel = metrics.get("kernel_policy", {})
        if not kernel.get("applied"):
            add_error(errors, label, f"kernel policy was not applied for {case_id}")
        for field, expected in (
            ("estimate_read", case.get("estimate_read_policy")),
            ("idle_wait", case.get("idle_wait_policy")),
            ("dispatch", case.get("dependency_dispatch")),
        ):
            if kernel.get(field) != expected:
                add_error(errors, label, f"kernel_policy.{field}: expected {expected!r}, got {kernel.get(field)!r}")
        if phase == "measurement":
            for field in (
                "dispatch_deferrals",
                "ready_queue_dispatches",
                "estimate_aborts",
                "idle_parks",
            ):
                mechanism_totals[(case_id, field)] += kernel.get(field, 0)

        dependency = metrics.get("dependency", {})
        if case.get("dependency_source") == "runtime_observed":
            if dependency.get("acquisition_measured") or dependency.get("acquisition_ns") != 0:
                add_error(errors, label, f"runtime acquisition unexpectedly measured for {case_id}")
        elif not dependency.get("acquisition_measured") or dependency.get("acquisition_ns", 0) <= 0:
            add_error(errors, label, f"static acquisition missing for {case_id}")
        if case.get("dependency_representation") != "version_only":
            if not dependency.get("representation_measured") or dependency.get("representation_ns", 0) <= 0:
                add_error(errors, label, f"representation missing for {case_id}")
        if expected_dispatch == "ready_queue":
            if dependency.get("wait_events", 0) != 0 or dependency.get("wait_ns", 0) != 0:
                add_error(errors, label, f"ready-queue case {case_id} entered the callback wait gate")
        if case.get("dependency_estimate_injection") == "write_estimates":
            if dependency.get("estimate_build_ns", 0) <= 0 or dependency.get("estimated_write_locations", 0) <= 0:
                add_error(errors, label, f"write-estimate build missing for {case_id}")

    for value in configs:
        label = value["label"]
        config_cases = value["config"]["cases"]
        if len(validations_by_label[label]) != len(config_cases):
            add_error(
                errors,
                label,
                f"expected {len(config_cases)} validation records, got {len(validations_by_label[label])}",
            )
        for case in config_cases:
            for phase, expected in (("warmup", warmups), ("measurement", measurements)):
                actual = len(records_by_key[(label, case["id"], phase)])
                if actual != expected:
                    add_error(errors, label, f"{case['id']}/{phase}: expected {expected}, got {actual}")
        if len(workload_hashes[label]) != 1 or None in workload_hashes[label]:
            add_error(errors, label, "expected one non-null workload hash")

    distinct_workload_hashes = {next(iter(values)) for values in workload_hashes.values() if len(values) == 1}
    if len(distinct_workload_hashes) != len(configs):
        add_error(errors, "", f"expected {len(configs)} distinct workload hashes, got {len(distinct_workload_hashes)}")
    if len(code_commits) != 1 or None in code_commits:
        add_error(errors, "", f"expected one code commit, got {sorted(str(value) for value in code_commits)}")
    if len(binary_hashes) != 1 or None in binary_hashes:
        add_error(errors, "", f"expected one binary hash, got {sorted(str(value) for value in binary_hashes)}")
    measurement_count = sum(record.get("phase") == "measurement" for record in records)
    if len(records) != expected_records:
        add_error(errors, "", f"expected {expected_records} records, got {len(records)}")
    if measurement_count != expected_measurements:
        add_error(errors, "", f"expected {expected_measurements} measurements, got {measurement_count}")
    for case_id in ("direct-ready", "frontier-ready", "full-ready"):
        if any(record.get("case", {}).get("id") == case_id for record in records):
            if mechanism_totals[(case_id, "dispatch_deferrals")] == 0:
                add_error(errors, "", f"{case_id} recorded no dispatch deferrals")
    if any(record.get("case", {}).get("id") == "estimate-abort" for record in records):
        if mechanism_totals[("estimate-abort", "estimate_aborts")] == 0:
            add_error(errors, "", "estimate-abort recorded no ESTIMATE aborts")

    return {
        "status": "PASS" if not errors else "FAIL",
        "stage": stage,
        "configs": len(configs),
        "records": len(records),
        "expected_records": expected_records,
        "measurement_records": measurement_count,
        "expected_measurement_records": expected_measurements,
        "validation_records": len(validations),
        "code_commit": next(iter(code_commits)) if len(code_commits) == 1 else "",
        "binary_sha256": next(iter(binary_hashes)) if len(binary_hashes) == 1 else "",
        "mechanism_totals": {
            f"{case_id}.{field}": value for (case_id, field), value in sorted(mechanism_totals.items())
        },
        "errors": errors,
    }


def measurement_records(records):
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
    for record in measurement_records(records):
        grouped[(record["_label"], record["case"]["id"])].append(record)
    rows = []
    for (label, case_id), values in sorted(grouped.items()):
        first = values[0]
        metrics = [record["metrics"] for record in values]
        dependency = [value["dependency"] for value in metrics]
        kernel = [value["kernel_policy"] for value in metrics]
        rows.append(
            {
                "workload": label,
                "kind": first["_kind"],
                "profile": first["_profile"],
                "compute_units": first["_compute_units"],
                "seed": first["_seed"],
                "case": case_id,
                "n": len(values),
                "median_execution_ms": median([record["timing"]["execution_ns"] for record in values]) / 1e6,
                "median_reexecution_attempts": median([value["reexecution_attempts"] for value in metrics]),
                "median_reexecuted_units": median([value["reexecuted_execution_units"] for value in metrics]),
                "median_discarded_units": median([value["discarded_execution_units"] for value in metrics]),
                "median_acquisition_ms": median([value.get("acquisition_ns", 0) for value in dependency]) / 1e6,
                "median_representation_ms": median([value.get("representation_ns", 0) for value in dependency]) / 1e6,
                "median_estimate_build_ms": median([value.get("estimate_build_ns", 0) for value in dependency]) / 1e6,
                "median_dependency_entries": median([value.get("representation_entries", 0) for value in dependency]),
                "median_dispatch_deferrals": median([value.get("dispatch_deferrals", 0) for value in kernel]),
                "median_estimate_aborts": median([value.get("estimate_aborts", 0) for value in kernel]),
                "median_idle_parks": median([value.get("idle_parks", 0) for value in kernel]),
            }
        )
    return rows


def compare(records):
    indexed = {}
    cases_by_axis = defaultdict(set)
    seeds_by_axis = defaultdict(set)
    for record in measurement_records(records):
        axis = (record["_profile"], record["_compute_units"])
        key = axis + (record["_seed"], record["case"]["id"], record["round"])
        indexed[key] = record
        cases_by_axis[axis].add(record["case"]["id"])
        seeds_by_axis[axis].add(record["_seed"])

    rows = []
    for axis in sorted(cases_by_axis):
        profile, compute_units = axis
        available = cases_by_axis[axis]
        seeds = sorted(seeds_by_axis[axis])
        for family, comparison, reference_case, treatment_case in COMPARISONS:
            if reference_case not in available or treatment_case not in available:
                continue
            reference = []
            treatment = []
            seed_ratios = []
            for seed in seeds:
                seed_reference = []
                seed_treatment = []
                rounds = sorted(
                    round_number
                    for current_profile, current_compute, current_seed, current_case, round_number in indexed
                    if (current_profile, current_compute, current_seed, current_case)
                    == (profile, compute_units, seed, reference_case)
                    and (profile, compute_units, seed, treatment_case, round_number) in indexed
                )
                for round_number in rounds:
                    seed_reference.append(
                        indexed[(profile, compute_units, seed, reference_case, round_number)]["timing"]["execution_ns"]
                    )
                    seed_treatment.append(
                        indexed[(profile, compute_units, seed, treatment_case, round_number)]["timing"]["execution_ns"]
                    )
                reference.extend(seed_reference)
                treatment.extend(seed_treatment)
                seed_ratios.append(median([right / left for left, right in zip(seed_reference, seed_treatment)]))
            ratios = [right / left for left, right in zip(reference, treatment)]
            ratio, ci_low, ci_high = paired_bootstrap(
                ratios, stable_seed(profile, compute_units, comparison)
            )
            rows.append(
                {
                    "comparison_family": family,
                    "comparison": comparison,
                    "profile": profile,
                    "compute_units": compute_units,
                    "reference": reference_case,
                    "treatment": treatment_case,
                    "seed_count": len(seeds),
                    "n_pairs": len(ratios),
                    "reference_median_ms": median(reference) / 1e6,
                    "treatment_median_ms": median(treatment) / 1e6,
                    "paired_median_ratio": ratio,
                    "paired_delta_pct": (ratio - 1) * 100,
                    "ratio_ci_low": ci_low,
                    "ratio_ci_high": ci_high,
                    "p_value": exact_sign_test(reference, treatment),
                    "holm_p_value": math.nan,
                    "seed_ratios": ";".join(f"{seed}:{value:.6f}" for seed, value in zip(seeds, seed_ratios)),
                    "same_direction": all(value < 1 for value in seed_ratios)
                    or all(value > 1 for value in seed_ratios),
                    "decision": "pending",
                }
            )
    holm_adjust(rows)
    for row in rows:
        if (
            row["paired_median_ratio"] <= 0.95
            and row["ratio_ci_high"] < 1
            and row["holm_p_value"] < 0.05
            and row["same_direction"]
        ):
            row["decision"] = "treatment_wins"
        elif (
            row["paired_median_ratio"] >= 1.05
            and row["ratio_ci_low"] > 1
            and row["holm_p_value"] < 0.05
            and row["same_direction"]
        ):
            row["decision"] = "treatment_loses"
        else:
            row["decision"] = "mixed_or_below_5pct"
    return rows


def mechanism_summary(summary_rows):
    grouped = defaultdict(list)
    for row in summary_rows:
        grouped[row["case"]].append(row)
    rows = []
    for case_id, values in sorted(grouped.items()):
        rows.append(
            {
                "case": case_id,
                "workload_artifacts": len(values),
                "median_execution_ms": median([value["median_execution_ms"] for value in values]),
                "median_reexecution_attempts": median(
                    [value["median_reexecution_attempts"] for value in values]
                ),
                "median_dispatch_deferrals": median(
                    [value["median_dispatch_deferrals"] for value in values]
                ),
                "median_estimate_aborts": median([value["median_estimate_aborts"] for value in values]),
                "median_idle_parks": median([value["median_idle_parks"] for value in values]),
            }
        )
    return rows


def write_report(path, stage, gate, effects, mechanisms):
    grouped = defaultdict(list)
    for row in effects:
        grouped[row["comparison"]].append(row)
    lines = [
        f"# CQ3 ready-queue Linux experiment — {stage}",
        "",
        f"- Gate: **{gate['status']}**",
        f"- Records: {gate['records']} ({gate['measurement_records']} measurement); validations: {gate['validation_records']}",
        f"- Code commit: `{gate['code_commit']}`",
        "- Ratios below 1 favor the treatment. Formal intervals use a 10,000-resample paired bootstrap; exact paired sign tests receive Holm correction by family.",
        "",
        "## Comparison decisions",
        "",
        "| Comparison | Rows | Wins | Losses | Mixed / <5% |",
        "|---|---:|---:|---:|---:|",
    ]
    for comparison, rows in sorted(grouped.items()):
        lines.append(
            f"| {comparison} | {len(rows)} | "
            f"{sum(row['decision'] == 'treatment_wins' for row in rows)} | "
            f"{sum(row['decision'] == 'treatment_loses' for row in rows)} | "
            f"{sum(row['decision'] == 'mixed_or_below_5pct' for row in rows)} |"
        )
    decisive = [row for row in effects if row["decision"] != "mixed_or_below_5pct"]
    lines.extend(
        [
            "",
            "## Seed-consistent material effects",
            "",
            "| Profile | Compute | Comparison | Ratio [95% CI] | Decision |",
            "|---|---:|---|---:|---|",
        ]
    )
    for row in decisive:
        lines.append(
            f"| {row['profile']} | {row['compute_units']} | {row['comparison']} | "
            f"{row['paired_median_ratio']:.3f} [{row['ratio_ci_low']:.3f}, {row['ratio_ci_high']:.3f}] | "
            f"{row['decision']} |"
        )
    lines.extend(
        [
            "",
            "## Mechanism summary",
            "",
            "| Case | Artifacts | Median reexec | Median deferrals | Median ESTIMATE aborts | Median idle parks |",
            "|---|---:|---:|---:|---:|---:|",
        ]
    )
    for row in mechanisms:
        lines.append(
            f"| {row['case']} | {row['workload_artifacts']} | {row['median_reexecution_attempts']:.1f} | "
            f"{row['median_dispatch_deferrals']:.1f} | {row['median_estimate_aborts']:.1f} | "
            f"{row['median_idle_parks']:.1f} |"
        )
    lines.extend(
        [
            "",
            "Complete per-artifact medians and all paired effects are in `summary.csv` and `effects.csv`.",
            "",
        ]
    )
    path.write_text("\n".join(lines), encoding="utf-8")


def update_manifest(run_root, stage, gate):
    manifest_path = run_root / "manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    manifest[f"{stage}_gate"] = {
        "status": gate["status"],
        "records": gate["records"],
        "measurement_records": gate["measurement_records"],
        "validation_records": gate["validation_records"],
    }
    manifest["code_commit"] = gate["code_commit"]
    manifest["binary_sha256"] = gate["binary_sha256"]
    if stage == "formal" and gate["status"] == "PASS":
        manifest["status"] = "completed"
        manifest["completed_at_utc"] = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
        manifest["analysis_outputs"] = [
            "formal/analysis/gate.json",
            "formal/analysis/summary.csv",
            "formal/analysis/effects.csv",
            "formal/analysis/mechanisms.csv",
            "formal/analysis/REPORT.md",
        ]
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("run_root", type=Path)
    parser.add_argument("stage", choices=ROUNDS)
    args = parser.parse_args()
    run_root = args.run_root.resolve()
    configs, records, validations = load_stage(run_root, args.stage)
    gate = gate_stage(run_root, args.stage, configs, records, validations)
    analysis_dir = run_root / args.stage / "analysis"
    analysis_dir.mkdir(parents=True, exist_ok=True)
    (analysis_dir / "gate.json").write_text(
        json.dumps(gate, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    if gate["status"] != "PASS":
        print(json.dumps({"stage": args.stage, "gate": "FAIL", "errors": len(gate["errors"])}))
        raise SystemExit(2)
    summary_rows = summarize(records)
    effects = compare(records)
    mechanisms = mechanism_summary(summary_rows)
    write_csv(analysis_dir / "summary.csv", summary_rows)
    write_csv(analysis_dir / "effects.csv", effects)
    write_csv(analysis_dir / "mechanisms.csv", mechanisms)
    write_report(analysis_dir / "REPORT.md", args.stage, gate, effects, mechanisms)
    update_manifest(run_root, args.stage, gate)
    print(
        json.dumps(
            {
                "stage": args.stage,
                "gate": "PASS",
                "records": gate["records"],
                "measurements": gate["measurement_records"],
                "effects": len(effects),
            }
        )
    )


if __name__ == "__main__":
    main()
