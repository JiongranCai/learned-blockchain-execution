import copy
import csv
import json
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
import unittest

from run_worker_experiment import WORKERS, configurations, describe
from run_workload_prefix_experiment import REPO, summarize
from run_workload_prefix_experiment import configurations as window_configurations


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

    def test_window_matrices_fix_p8_and_preserve_each_policy(self):
        base = json.loads((REPO / "configs/experiments/workload/standard-smoke.json").read_text())
        original = copy.deepcopy(base)
        for stage, expected in (("pilot", 3), ("repeated", 6)):
            args = SimpleNamespace(stage=stage, suite="speculation", notes="test")
            cells = list(window_configurations(base, args, Path("unused")))
            self.assertEqual(len(cells), expected)
            self.assertEqual(base, original)
            for name, config in cells:
                self.assertEqual(len(config["cases"]), 12)
                workload = config["workload"]["synthetic"]
                self.assertEqual((workload["block_count"], workload["transactions_per_block"]), (1, 1536))
                for limit in (1, 8, 32, 0):
                    for policy in base["cases"]:
                        expected = dict(policy, id=f'{policy["id"]}-l{limit or "w"}',
                                        executors=8, max_speculative_inflight=limit)
                        self.assertIn(expected, config["cases"], name)

    def test_summary_pairs_same_window_and_same_policy_at_lw(self):
        self.check_summary("speculation")

    def test_existing_summary_keeps_original_case_names(self):
        self.check_summary("placement")

    def check_summary(self, suite):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "configs").mkdir()
            cases, records = [], []
            settings = ((1, 0), (8, 0)) if suite == "workers" else ((8, 1), (8, 0)) if suite == "speculation" else ((8, 0),)
            for workers, limit in settings:
                for policy, factor in (("runtime", 1), ("direct-ready", 0.8), ("estimate-abort", 1.2)):
                    suffix = f"-p{workers}" if suite == "workers" else f'-l{limit or "w"}' if suite == "speculation" else ""
                    case = {"id": policy + suffix, "executors": workers, "max_speculative_inflight": limit}
                    cases.append(case)
                    for round_index in (2, 0, 1):  # File order must not determine pairing.
                        metrics = {key: 2 for key in ("execution_attempts", "reexecution_attempts",
                            "discarded_execution_units", "reexecuted_execution_units", "useful_execution_units",
                            "validation_failures", "validation_events", "wait_events", "max_rss_bytes")}
                        metrics["kernel_policy"] = {key: 3 for key in ("estimate_aborts", "dispatch_deferrals",
                            "estimate_suspends", "estimate_suspend_ns", "worker_yields", "idle_parks")}
                        metrics["kernel_policy"]["peak_runnable_workers"] = workers + round_index
                        metrics["dependency"] = {"acquisition_ns": 1, "representation_ns": 2, "wait_ns": 7}
                        metrics.update(speculation_telemetry_available=bool(limit), effective_speculation_limit=limit or 1536,
                                       peak_speculative_inflight=limit, admission_stall_events=4, admission_stall_ns=5)
                        records.append({"case": case, "phase": "measurement", "round": round_index,
                            "status": "success", "censored": False, "canonical_match": True,
                            "timing": {"execution_ns": (round_index + 1) * 8000000 * factor * (2 if limit else 1) / workers},
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
                if suite == "speculation":
                    limit = case["max_speculative_inflight"]
                    self.assertAlmostEqual(float(row["ratio_to_lw"]), 2 if limit else 1)
                    self.assertEqual(int(row["workers"]), 8)
                    self.assertEqual(int(row["effective_speculation_limit"]), limit or 1536)
                    self.assertEqual(row["peak_speculative_inflight"], "1" if limit else "")
                    self.assertEqual(row["admission_stall_events"], "4" if limit else "")
            with (root / "comparisons.csv").open() as source:
                comparisons = list(csv.DictReader(source))
            self.assertEqual(len(comparisons), 2 * len(settings))
            for row in comparisons:
                expected = 0.8 if row["comparison_family"] == "direct_vs_runtime" else 0.8 / 1.2
                self.assertAlmostEqual(float(row["ratio"]), expected)
            path.write_text("\n".join(json.dumps(r) for r in records[:-1]))
            with self.assertRaisesRegex(ValueError, "incomplete measurement rounds"):
                summarize(root, suite, description)


if __name__ == "__main__":
    unittest.main()
