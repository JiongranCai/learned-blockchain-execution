#!/usr/bin/env python3
"""Generate the CQ3 ready-queue Linux pilot and formal matrices."""

from __future__ import annotations

import argparse
import copy
import json
import pathlib


REPO = pathlib.Path(__file__).resolve().parent.parent
COMPUTE_LEVELS = (1_000, 100_000)
PROFILES = (
    {
        "kind": "conflict",
        "name": "single99",
        "seeds": (20260821, 20260822),
        "hot_key_count": 1,
        "hot_probability": 0.99,
        "same_key_probability": 1.0,
    },
    {
        "kind": "conflict",
        "name": "dual99",
        "seeds": (20260821, 20260822),
        "hot_key_count": 2,
        "hot_probability": 0.99,
        "same_key_probability": 0.95,
    },
    {
        "kind": "conflict",
        "name": "hot8p99",
        "seeds": (20260821, 20260822),
        "hot_key_count": 8,
        "hot_probability": 0.99,
        "same_key_probability": 0.75,
    },
    {
        "kind": "conflict",
        "name": "diff32p50",
        "seeds": (20260821, 20260822),
        "hot_key_count": 32,
        "hot_probability": 0.50,
        "same_key_probability": 0.25,
    },
    {"kind": "fanout", "name": "fanout08", "seeds": (20260831, 20260832), "width": 8},
    {"kind": "fanout", "name": "fanout32", "seeds": (20260831, 20260832), "width": 32},
    {"kind": "fanout", "name": "fanout128", "seeds": (20260831, 20260832), "width": 128},
    {"kind": "selective", "name": "selective02", "seeds": (20260831, 20260832), "width": 2},
    {"kind": "selective", "name": "selective08", "seeds": (20260831, 20260832), "width": 8},
    {"kind": "selective", "name": "selective32", "seeds": (20260831, 20260832), "width": 32},
)


def blockstm_case(
    case_id: str,
    source: str,
    representation: str,
    builder: str,
    *,
    wait: str = "none",
    estimates: str = "disabled",
) -> dict:
    return {
        "id": case_id,
        "engine": "blockstm",
        "policy": "blockstm_preset",
        "executors": 8,
        "max_speculative_inflight": 0,
        "dependency_mode": "mvcc_runtime",
        "dependency_source": source,
        "dependency_representation": representation,
        "dependency_representation_builder": builder,
        "dependency_wait_policy": wait,
        "dependency_estimate_injection": estimates,
        "dependency_dispatch": "ready_queue" if wait != "none" else "index_order",
        "estimate_read_policy": "abort_and_reschedule",
        "idle_wait_policy": "park",
        "trace_mode": "counters",
    }


ALL_CASES = {
    "runtime": blockstm_case("runtime", "runtime_observed", "version_only", "none"),
    "static-only": blockstm_case("static-only", "static_program", "version_only", "none"),
    "raw-none": blockstm_case("raw-none", "static_program", "raw_last_writer", "indexed_by_key"),
    "direct-ready": blockstm_case(
        "direct-ready",
        "static_program",
        "raw_last_writer",
        "indexed_by_key",
        wait="direct_predecessor_wait",
    ),
    "estimate-abort": blockstm_case(
        "estimate-abort",
        "static_program",
        "raw_last_writer",
        "indexed_by_key",
        estimates="write_estimates",
    ),
    "summary-none": blockstm_case(
        "summary-none", "static_program", "max_raw_predecessor", "indexed_by_key"
    ),
    "frontier-ready": blockstm_case(
        "frontier-ready",
        "static_program",
        "max_raw_predecessor",
        "indexed_by_key",
        wait="contiguous_frontier_wait",
    ),
    "full-none": blockstm_case(
        "full-none", "static_program", "full_conflict_graph", "indexed_by_key"
    ),
    "full-ready": blockstm_case(
        "full-ready",
        "static_program",
        "full_conflict_graph",
        "indexed_by_key",
        wait="all_predecessors_wait",
    ),
}

CASE_IDS = {
    "conflict": tuple(ALL_CASES),
    "fanout": (
        "runtime",
        "static-only",
        "raw-none",
        "direct-ready",
        "summary-none",
        "frontier-ready",
    ),
    "selective": (
        "runtime",
        "static-only",
        "raw-none",
        "direct-ready",
        "estimate-abort",
        "summary-none",
        "frontier-ready",
    ),
}


def cases(profile: dict) -> list[dict]:
    return [copy.deepcopy(ALL_CASES[case_id]) for case_id in CASE_IDS[profile["kind"]]]


def workload_label(profile: dict, compute_units: int, seed: int) -> str:
    return f"{profile['name']}-c{compute_units // 1000:03d}k-s{seed % 100:02d}"


def synthetic_workload(profile: dict, compute_units: int, seed: int) -> dict:
    value = {
        "seed": seed,
        "block_count": 2,
        "transactions_per_block": 512,
        "max_compute_units": compute_units,
        "min_compute_units": compute_units,
        "failure_every": 0,
    }
    if profile["kind"] == "conflict":
        value.update(
            {
                "initial_keys": 8192,
                "key_space": 8192,
                "transaction_max_units": compute_units + 4,
                "access_distribution": {
                    "kind": "hotspot",
                    "hot_key_count": profile["hot_key_count"],
                    "hot_access_probability": profile["hot_probability"],
                    "read_write_same_key_probability": profile["same_key_probability"],
                },
            }
        )
    elif profile["kind"] == "fanout":
        value.update(
            {
                "initial_keys": 1024,
                "key_space": 1024,
                "transaction_max_units": compute_units + profile["width"] + 3,
                "program_shape": "fan_in_fan_out",
                "fan_in": profile["width"],
            }
        )
    else:
        value.update(
            {
                "initial_keys": 2048,
                "key_space": 64,
                "transaction_max_units": compute_units + profile["width"] + 5,
                "program_shape": "selective_read_set",
                "branch_read_candidates": profile["width"],
            }
        )
    return value


def matrix(
    run_id: str,
    stage: str,
    profile: dict,
    compute_units: int,
    seed: int,
) -> tuple[str, dict]:
    label = workload_label(profile, compute_units, seed)
    run_class = "smoke" if stage == "pilot" else "formal"
    warmups, measurements = (1, 3) if stage == "pilot" else (3, 30)
    profile_index = next(index for index, item in enumerate(PROFILES) if item["name"] == profile["name"])
    artifact_dir = f"results/runs/{run_id}/{stage}/artifacts/{profile['kind']}/{label}"
    return label, {
        "schema_version": "experiment-matrix-v8",
        "run_class": run_class,
        "workload": {
            "synthetic": synthetic_workload(profile, compute_units, seed),
        },
        "statistical_protocol": "configs/statistical/protocol-v1.json",
        "warmup_rounds": warmups,
        "measurement_rounds": measurements,
        "order_seed": 202608220000 + profile_index * 1_000 + (compute_units // 1_000) * 10 + seed % 10,
        "timeout": "3m",
        "environment": {
            "affinity": "physical_cpus_0-7",
            "numa_policy": "physcpubind_0-7_membind_0",
            "state_reset": "fresh_state_from_frozen_artifact",
            "page_cache": "unchanged_no_drop",
            "process_reuse": "fresh_process_per_run",
        },
        "cases": cases(profile),
        "output": {
            "validation_records": f"{artifact_dir}/validation.jsonl",
            "run_records": f"{artifact_dir}/runs.jsonl",
            "action_traces": f"{artifact_dir}/action-traces.jsonl",
        },
    }


def write_json(path: pathlib.Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")


def preflight_config(run_id: str) -> dict:
    source = REPO / "configs" / "experiments" / "kernel-policy" / "selective-read-set-smoke.json"
    value = json.loads(source.read_text(encoding="utf-8"))
    value["measurement_rounds"] = 3
    value["environment"] = {
        "affinity": "physical_cpus_0-7",
        "numa_policy": "physcpubind_0-7_membind_0",
        "state_reset": "fresh_state_from_frozen_artifact",
        "page_cache": "unchanged_no_drop",
        "process_reuse": "fresh_process_per_run",
    }
    artifact_dir = f"results/runs/{run_id}/preflight/artifacts/kernel-policy"
    value["output"] = {
        "validation_records": f"{artifact_dir}/validation.jsonl",
        "run_records": f"{artifact_dir}/runs.jsonl",
        "action_traces": f"{artifact_dir}/action-traces.jsonl",
    }
    return value


def design_markdown() -> str:
    return """# CQ3 ready-queue Linux experiment

This run studies CQ3 dependency acquisition, representation, and consumers.
Kernel policy is infrastructure, not an experimental axis: every primary
Block-STM case uses P=8, L=W, abort-and-reschedule ESTIMATE reads, parked idle
workers, and ready-queue dispatch whenever a dependency wait consumer exists.

The 9-cell conflict-width core separates static acquisition, representation,
and use for Direct, write estimates, summary/frontier, and the full conflict
graph. Fan-out and selective matrices retain only the cells needed for their
matched structural questions. Workloads use two seeds, two 512-transaction
blocks, and fixed compute costs of 1,000 or 100,000 units.

The kernel-policy preflight is a 1+3 mechanism check only. It is excluded from
the CQ3 formal analysis. Pilot matrices use 1+3 rounds; formal matrices use
3+30 rounds under statistical-protocol-v1.
"""


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--run-id", required=True)
    args = parser.parse_args()
    run_dir = REPO / "results" / "runs" / args.run_id
    matrix_specs = []
    for profile in PROFILES:
        for compute_units in COMPUTE_LEVELS:
            for seed in profile["seeds"]:
                label = workload_label(profile, compute_units, seed)
                matrix_specs.append((profile, compute_units, seed, label))

    write_json(run_dir / "configs" / "preflight" / "kernel-policy.json", preflight_config(args.run_id))
    for stage in ("pilot", "formal"):
        for profile, compute_units, seed, label in matrix_specs:
            _, value = matrix(args.run_id, stage, profile, compute_units, seed)
            write_json(run_dir / "configs" / stage / profile["kind"] / f"{label}.json", value)

    formal_records = sum(len(cases(profile)) * 33 for profile, _, _, _ in matrix_specs)
    formal_measurements = sum(len(cases(profile)) * 30 for profile, _, _, _ in matrix_specs)
    manifest = {
        "run_id": args.run_id,
        "status": "generated",
        "host_label": "aws-metal",
        "repository": str(REPO),
        "design": "cq3-ready-queue-v2",
        "matrix_count_per_stage": len(matrix_specs),
        "profiles": list(PROFILES),
        "compute_units": list(COMPUTE_LEVELS),
        "blocks_per_artifact": 2,
        "transactions_per_block": 512,
        "kernel_recipe": {
            "executors": 8,
            "max_speculative_inflight": "W",
            "estimate_read_policy": "abort_and_reschedule",
            "idle_wait_policy": "park",
            "wait_consumer_dispatch": "ready_queue",
        },
        "cases_by_workload_kind": {key: list(value) for key, value in CASE_IDS.items()},
        "pilot_rounds": {"warmup": 1, "measurement": 3},
        "formal_rounds": {"warmup": 3, "measurement": 30},
        "expected_formal_records": formal_records,
        "expected_formal_measurements": formal_measurements,
        "cpu_affinity": "0-7",
        "numa_node": 0,
        "gomaxprocs": 8,
    }
    write_json(run_dir / "manifest.json", manifest)
    (run_dir / "DESIGN.md").write_text(design_markdown(), encoding="utf-8")
    print(
        f"generated {len(matrix_specs)} pilot and {len(matrix_specs)} formal matrices; "
        f"expected formal records={formal_records}, measurements={formal_measurements}"
    )


if __name__ == "__main__":
    main()
