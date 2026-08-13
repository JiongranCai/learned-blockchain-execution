#!/usr/bin/env python3
"""Generate frozen CQ3 structure-sensitive pilot and formal matrices."""

import argparse
import copy
import json
import pathlib
import re
import subprocess


REPO = pathlib.Path("/home/ubuntu/project/learned-blockchain-execution")
ZERO_HASH = "0" * 64
SEEDS = (20260831, 20260832)
COMPUTE_LEVELS = (1_000, 100_000)
PROFILES = (
    {"kind": "selective", "name": "selective02", "width": 2},
    {"kind": "selective", "name": "selective08", "width": 8},
    {"kind": "selective", "name": "selective32", "width": 32},
    {"kind": "fanout", "name": "fanout08", "width": 8},
    {"kind": "fanout", "name": "fanout32", "width": 32},
    {"kind": "fanout", "name": "fanout128", "width": 128},
)


def blockstm_case(
    case_id,
    limit,
    source,
    representation,
    builder,
    wait="none",
    estimates="disabled",
):
    return {
        "id": case_id,
        "engine": "blockstm",
        "policy": "blockstm_preset",
        "executors": 8,
        "max_speculative_inflight": limit,
        "dependency_mode": "mvcc_runtime",
        "dependency_source": source,
        "dependency_representation": representation,
        "dependency_representation_builder": builder,
        "dependency_wait_policy": wait,
        "dependency_estimate_injection": estimates,
        "trace_mode": "counters",
    }


def static_case(case_id, limit, plan, estimates="disabled"):
    if plan == "direct":
        return blockstm_case(
            case_id,
            limit,
            "static_program",
            "raw_last_writer",
            "indexed_by_key",
            wait="direct_predecessor_wait",
            estimates=estimates,
        )
    if plan == "estimate":
        return blockstm_case(
            case_id,
            limit,
            "static_program",
            "version_only",
            "none",
            estimates="write_estimates",
        )
    if plan == "summary":
        return blockstm_case(
            case_id,
            limit,
            "static_program",
            "max_raw_predecessor",
            "indexed_by_key",
            wait="contiguous_frontier_wait",
            estimates=estimates,
        )
    raise ValueError(plan)


def cases():
    values = [
        {
            "id": "serial-oracle",
            "engine": "serial",
            "policy": "serial_preset",
            "executors": 1,
            "max_speculative_inflight": 1,
            "dependency_mode": "mvcc_runtime",
            "dependency_source": "runtime_observed",
            "dependency_representation": "version_only",
            "dependency_representation_builder": "none",
            "dependency_wait_policy": "none",
            "dependency_estimate_injection": "disabled",
            "trace_mode": "counters",
        },
        blockstm_case("runtime-l1", 1, "runtime_observed", "version_only", "none"),
    ]
    for window_name, limit in (("l8", 8), ("lw", 0)):
        values.extend(
            [
                blockstm_case(
                    f"runtime-{window_name}",
                    limit,
                    "runtime_observed",
                    "version_only",
                    "none",
                ),
                static_case(f"direct-{window_name}", limit, "direct"),
                static_case(f"estimate-{window_name}", limit, "estimate"),
                static_case(f"direct-estimate-{window_name}", limit, "direct", "write_estimates"),
                static_case(f"summary-{window_name}", limit, "summary"),
                static_case(f"summary-estimate-{window_name}", limit, "summary", "write_estimates"),
            ]
        )
    return values


CASES = cases()


def compute_label(compute_units):
    return f"c{compute_units // 1000:03d}k"


def workload_label(profile, compute_units, seed):
    return f"{profile['name']}-{compute_label(compute_units)}-s{seed % 100:02d}"


def synthetic_workload(profile, compute_units, seed):
    common = {
        "seed": seed,
        "block_count": 2,
        "transactions_per_block": 512,
        "max_compute_units": compute_units,
        "min_compute_units": compute_units,
        "failure_every": 0,
    }
    if profile["kind"] == "selective":
        common.update(
            {
                "initial_keys": 2048,
                "key_space": 64,
                "transaction_max_units": compute_units + profile["width"] + 5,
                "program_shape": "selective_read_set",
                "branch_read_candidates": profile["width"],
            }
        )
    else:
        common.update(
            {
                "initial_keys": 1024,
                "key_space": 1024,
                "transaction_max_units": compute_units + profile["width"] + 3,
                "program_shape": "fan_in_fan_out",
                "fan_in": profile["width"],
            }
        )
    return common


def workload(profile, compute_units, seed, expected_hash):
    return {
        "synthetic": synthetic_workload(profile, compute_units, seed),
        "expected_hash": expected_hash,
    }


def matrix(run_id, run_class, profile, compute_units, seed, expected_hash, selected_cases=None):
    phase = "pilot" if run_class == "smoke" else "formal"
    label = workload_label(profile, compute_units, seed)
    artifact_dir = f"results/runs/{run_id}/{phase}/artifacts/{profile['kind']}/{label}"
    warmups, measurements = (1, 3) if run_class == "smoke" else (3, 30)
    profile_index = next(index for index, value in enumerate(PROFILES) if value["name"] == profile["name"])
    value = {
        "schema_version": "experiment-matrix-v6",
        "run_class": run_class,
        "workload": workload(profile, compute_units, seed, expected_hash),
        "statistical_protocol": "configs/statistical/protocol-v1.json",
        "warmup_rounds": warmups,
        "measurement_rounds": measurements,
        "order_seed": 202608130000 + profile_index * 10_000 + (compute_units // 1000) * 10 + seed % 10,
        "timeout": "3m",
        "environment": {
            "affinity": "physical_cpus_0-7",
            "numa_policy": "physcpubind_0-7_membind_0",
            "state_reset": "fresh_state_from_frozen_artifact",
            "page_cache": "unchanged_no_drop",
            "process_reuse": "fresh_process_per_run",
        },
        "cases": copy.deepcopy(selected_cases if selected_cases is not None else CASES),
        "output": {
            "validation_bundle": f"{artifact_dir}/validation-bundle.json",
            "validation_records": f"{artifact_dir}/validation.jsonl",
            "run_records": f"{artifact_dir}/runs.jsonl",
            "action_traces": f"{artifact_dir}/action-traces.jsonl",
        },
    }
    return label, value


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")


def discover_hash(run_id, bench, profile, compute_units, seed):
    probe_case = [blockstm_case("runtime-lw", 0, "runtime_observed", "version_only", "none")]
    label, probe = matrix(run_id, "smoke", profile, compute_units, seed, ZERO_HASH, probe_case)
    probe_path = REPO / "results" / "runs" / run_id / "configs" / "hash-probes" / f"{label}.json"
    write_json(probe_path, probe)
    result = subprocess.run(
        [str(bench), "validate", "-config", str(probe_path)],
        cwd=REPO,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )
    match = re.search(r"got ([0-9a-f]{64}), want " + ZERO_HASH, result.stdout)
    if result.returncode == 0 or not match:
        raise RuntimeError(f"could not discover hash for {label}: {result.stdout}")
    return match.group(1)


def design_markdown():
    return """# CQ3 structure-sensitive experiment

This run targets the workloads needed to distinguish the current CQ3 choices.

## Workloads

- `selective_read_set` (2/8/32 candidates): the program exposes all candidate
  reads, executes one state-selected read, and has an exact concrete write key.
  This isolates false static read dependencies versus write estimates.
- `fan_in_fan_out` (8/32/128 producers): one producer prefix feeds every
  remaining independent consumer. The max-RAW prefix is an exact shared
  barrier; summary stores one predecessor per consumer where Direct stores N.
- Fixed compute costs: 1,000 and 100,000 units; two seeds; two 512-tx blocks.

## Controls and plans

- Serial oracle and runtime MVCC L=1 are near-serial controls.
- At L=8 and L=W: runtime MVCC, Direct, estimates, Direct+estimates, summary,
  and summary+estimates.
- `full_graph` is excluded: the current adapter only provides an
  all-predecessors entry wait. It has no ready-queue/wave/reordering consumer,
  so extra WAR/WAW edges cannot unlock parallel execution in this stage.

Pilot uses 1+3 rounds. Formal uses 3+30 rounds under statistical-protocol-v1.
"""


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--bench", type=pathlib.Path, required=True)
    args = parser.parse_args()
    run_dir = REPO / "results" / "runs" / args.run_id

    hashes = {}
    for profile in PROFILES:
        for compute_units in COMPUTE_LEVELS:
            for seed in SEEDS:
                label = workload_label(profile, compute_units, seed)
                hashes[label] = discover_hash(args.run_id, args.bench, profile, compute_units, seed)

    for run_class in ("smoke", "formal"):
        phase = "pilot" if run_class == "smoke" else "formal"
        for profile in PROFILES:
            for compute_units in COMPUTE_LEVELS:
                for seed in SEEDS:
                    label = workload_label(profile, compute_units, seed)
                    _, value = matrix(args.run_id, run_class, profile, compute_units, seed, hashes[label])
                    path = run_dir / "configs" / phase / profile["kind"] / f"{label}.json"
                    write_json(path, value)
                    (REPO / value["output"]["run_records"]).parent.mkdir(parents=True, exist_ok=True)

    manifest = {
        "run_id": args.run_id,
        "status": "generated",
        "host_label": "aws-metal",
        "repository": str(REPO),
        "design": "cq3-structure-specialization-v1",
        "profiles": list(PROFILES),
        "compute_units": list(COMPUTE_LEVELS),
        "seeds": list(SEEDS),
        "blocks_per_artifact": 2,
        "transactions_per_block": 512,
        "windows": {"l1": 1, "l8": 8, "lw": 512},
        "plans": ["runtime", "direct", "estimate", "direct-estimate", "summary", "summary-estimate"],
        "excluded": {"full_graph": "no enabling consumer in current adapter"},
        "case_count_per_matrix": len(CASES),
        "matrix_count_per_stage": len(PROFILES) * len(COMPUTE_LEVELS) * len(SEEDS),
        "workload_hashes": hashes,
        "pilot_rounds": {"warmup": 1, "measurement": 3},
        "formal_rounds": {"warmup": 3, "measurement": 30},
        "cpu_affinity": "0-7",
        "numa_node": 0,
        "gomaxprocs": 8,
    }
    write_json(run_dir / "manifest.json", manifest)
    (run_dir / "DESIGN.md").write_text(design_markdown(), encoding="utf-8")
    for key in sorted(hashes):
        print(f"{key} hash={hashes[key]}")
    print(f"generated {2 * len(hashes)} matrices with {len(CASES)} cases each")


if __name__ == "__main__":
    main()
