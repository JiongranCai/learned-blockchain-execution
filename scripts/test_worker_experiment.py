import copy
import csv
import json
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
import unittest

from run_worker_experiment import WORKERS, configurations, describe
from run_workload_prefix_experiment import REPO, summarize


class WorkerExperimentTest(unittest.TestCase):
    def test_matrices_hold_workload_and_policies_fixed(self):
        base = json.loads((REPO / "configs/experiments/workload/standard-smoke.json").read_text())
        original = copy.deepcopy(base)
        for stage, expected in (("pilot", 6), ("repeated", 8)):
            with self.subTest(stage=stage), TemporaryDirectory() as tmp:
                root = Path(tmp)
                args = SimpleNamespace(stage=stage, units_per_ms=10, notes="test")
                cells = list(configurations(base, args, root))
                self.assertEqual(len(cells), expected)
                self.assertEqual(base, original)
                artifacts = {}
                for name, config in cells:
                    self.assertEqual(len(config["cases"]), 18)
                    self.assertEqual(len({c["id"] for c in config["cases"]}), 18)
                    for workers in WORKERS:
                        for policy in base["cases"]:
                            actual = next(c for c in config["cases"] if c["id"] == f'{policy["id"]}-p{workers}')
                            self.assertEqual(actual, dict(policy, id=actual["id"], executors=workers))
                            self.assertEqual(actual["max_speculative_inflight"], 0)
                    info = describe(config, "workers", name)
                    if "synthetic" in config["workload"]:
                        workload = config["workload"]["synthetic"]
                        self.assertEqual((workload["block_count"], workload["transactions_per_block"]), (1, 1536))
                        self.assertEqual(workload["mix"][0]["compute"], {
                            "min_units": 100000, "max_units": 100000, "prefix_fraction": 0.5})
                    else:
                        value = json.loads(Path(config["workload"]["artifact_path"]).read_text())
                        self.assertEqual(len(value["ordered_blocks"]), 1)
                        transactions = value["ordered_blocks"][0]["transactions"]
                        self.assertEqual(len(transactions), info["transactions"])
                        self.assertEqual(value["generator"]["config"]["units_per_ms"], 10)
                        artifacts[name] = {tx["id"]: tx for tx in transactions}
                for groups in (1, 128):
                    self.assertEqual(artifacts[f"HDU_g{groups}_s101"], artifacts[f"HUD_g{groups}_s101"])

    def test_summary_pairs_same_workers_and_same_policy_at_p8(self):
        self.check_summary("workers")

    def test_existing_summary_keeps_original_case_names(self):
        self.check_summary("placement")

    def check_summary(self, suite):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "configs").mkdir()
            cases, records = [], []
            for workers in ((1, 8) if suite == "workers" else (8,)):
                for policy, factor in (("runtime", 1), ("direct-ready", 0.8), ("estimate-abort", 1.2)):
                    case = {"id": f"{policy}-p{workers}" if suite == "workers" else policy, "executors": workers}
                    cases.append(case)
                    for round_index in (2, 0, 1):  # File order must not determine pairing.
                        metrics = {key: 2 for key in ("execution_attempts", "reexecution_attempts",
                            "discarded_execution_units", "reexecuted_execution_units", "useful_execution_units",
                            "validation_failures", "validation_events", "wait_events", "max_rss_bytes")}
                        metrics["kernel_policy"] = {key: 3 for key in ("estimate_aborts", "dispatch_deferrals",
                            "estimate_suspends", "estimate_suspend_ns", "worker_yields", "idle_parks")}
                        metrics["kernel_policy"]["peak_runnable_workers"] = workers + round_index
                        metrics["dependency"] = {"acquisition_ns": 1, "representation_ns": 2, "wait_ns": 7}
                        records.append({"case": case, "phase": "measurement", "round": round_index,
                            "status": "success", "censored": False, "canonical_match": True,
                            "timing": {"execution_ns": (round_index + 1) * 8000000 * factor / workers},
                            "metrics": metrics, "provenance": {"hardware": {
                                "gomaxprocs": 8, "cpu_allowed_list": "2-9"}}})
            path = root / "runs.jsonl"
            path.write_text("\n".join(json.dumps(r) for r in records))
            config = {"cases": cases, "measurement_rounds": 3, "output": {"run_records": str(path)}}
            (root / "configs" / "fixture.json").write_text(json.dumps(config))
            description = lambda *_: {"profile": "fixture", "seed": 42}
            summarize(root, suite, description)
            with (root / "summary.csv").open() as source:
                rows = {r["case"]: r for r in csv.DictReader(source)}
            self.assertEqual(len(rows), len(cases))
            for case in cases:
                row = rows[case["id"]]
                expected = 0.8 if case["id"].startswith("direct-ready") else 1.2 if case["id"].startswith("estimate-abort") else 1
                self.assertAlmostEqual(float(row["ratio_to_runtime"]), expected)
                if suite == "workers":
                    self.assertAlmostEqual(float(row["ratio_to_p8"]), 8 / case["executors"])
                    self.assertEqual(int(row["workers"]), case["executors"])
                    self.assertEqual(int(row["peak_runnable_workers"]), case["executors"] + 2)
                    self.assertEqual(int(row["dependency_wait_ns"]), 7)
                else:
                    self.assertNotIn("ratio_to_p8", row)
            with (root / "comparisons.csv").open() as source:
                comparisons = list(csv.DictReader(source))
            self.assertEqual(len(comparisons), 4 if suite == "workers" else 2)
            for row in comparisons:
                expected = 0.8 if row["comparison_family"] == "direct_vs_runtime" else 0.8 / 1.2
                self.assertAlmostEqual(float(row["ratio"]), expected)
            path.write_text("\n".join(json.dumps(r) for r in records[:-1]))
            with self.assertRaisesRegex(ValueError, "incomplete measurement rounds"):
                summarize(root, suite, description)


if __name__ == "__main__":
    unittest.main()
