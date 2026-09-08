import copy
import json
from pathlib import Path
from types import SimpleNamespace
import unittest

from run_smallbank_experiment import configurations, describe, profiles
from run_workload_prefix_experiment import REPO


class SmallBankExperimentTest(unittest.TestCase):
    def test_selected_profiles_and_rounds_keep_the_same_workloads(self):
        base = json.loads((REPO / "configs/experiments/workload/standard-smoke.json").read_text())
        args = SimpleNamespace(stage="repeated", notes="test", profiles=["selective-a50-p0"],
                               measurement_rounds=15)
        cells = list(configurations(base, args, Path("unused")))
        self.assertEqual(len(cells), 3)
        default = dict(configurations(base, SimpleNamespace(stage="repeated", notes="test"), Path("unused")))
        for name, config in cells:
            self.assertEqual(config["measurement_rounds"], 15)
            reference = copy.deepcopy(default[name])
            reference["measurement_rounds"] = 15
            self.assertEqual(config, reference)
        args.seeds = [151, 15151]
        self.assertEqual([config["workload"]["smallbank"]["seed"] for _, config in configurations(base, args, Path("unused"))], args.seeds)
        args.block_count, args.block_size = 48, 128
        args.accounts = 64
        for _, config in configurations(base, args, Path("unused")):
            bank = config["workload"]["smallbank"]
            self.assertEqual((bank["block_count"], bank["transactions_per_block"]), (48, 128))
            self.assertEqual(bank["accounts"], 64)

    def test_matrices_pair_policies_at_p8_lw_and_preserve_input(self):
        base = json.loads((REPO / "configs/experiments/workload/standard-smoke.json").read_text())
        original = copy.deepcopy(base)
        for stage, expected in (("pilot", 22), ("repeated", 66)):
            cells = list(configurations(base, SimpleNamespace(stage=stage, notes="test"), Path("unused")))
            self.assertEqual(len(cells), expected)
            self.assertEqual(len({name for name, _ in cells}), expected)
            self.assertEqual(base, original)
            for name, config in cells:
                self.assertEqual(config["cases"], base["cases"])
                self.assertEqual(set(config["workload"]), {"smallbank"})
                bank = config["workload"]["smallbank"]
                self.assertEqual((bank["block_count"], bank["transactions_per_block"]), (4, 1536))
                self.assertEqual((config["warmup_rounds"], config["measurement_rounds"]),
                                 (1, 3) if stage == "pilot" else (3, 30))
                info = describe(config, "smallbank", name)
                self.assertEqual(info["write_transaction_fraction"],
                                 0.85 if name.startswith("standard") else 0.5 if name.startswith("contention") else
                                 0.05 if name.startswith("selective-sparse") else 0 if name.startswith("readonly") else 0.25)
            # Editing one matrix must not change another seed or profile.
            untouched = copy.deepcopy(cells[1][1])
            cells[0][1]["workload"]["smallbank"]["mix"][0]["amount"] = 999
            cells[0][1]["workload"]["smallbank"]["initial_checking"]["min"] = 999
            self.assertEqual(cells[1][1], untouched)

    def test_cost_and_selective_profiles_isolate_the_intended_dimensions(self):
        cells = {name: (mix, initial) for name, mix, initial in profiles()}
        for hot in (1, 2):
            left = copy.deepcopy(cells[f"selective-sparse-hot{hot}-a50"])
            left[0][1]["amount"] = 200
            self.assertEqual(left, cells[f"selective-sparse-hot{hot}-a200"])
        for hot in (2, 8):
            followers = [cells[f"contention-hot{hot}-p{p}-s{s}"][0][1]
                         for p, s in ((0, 100000), (90000, 10000), (400000, 100000))]
            splits = []
            for tx in followers:
                cost = tx["compute"]
                prefix = int(cost["min_units"] * cost["prefix_fraction"])
                splits.append((prefix, cost["min_units"] - prefix))
            self.assertEqual(splits, [(0, 100000), (90000, 10000), (400000, 100000)])
        for amount in (50, 150, 200):
            left, initial = cells[f"selective-a{amount}-p0"]
            right, _ = cells[f"selective-a{amount}-p90000"]
            self.assertEqual(initial, {"min": 100, "max": 199})
            self.assertEqual([tx["type"] for tx in left], ["transact_savings", "check_funds"])
            self.assertEqual(left[0], right[0])
            self.assertEqual(left[1]["amount"], amount)
            self.assertEqual(left[1]["compute"]["max_units"], right[1]["compute"]["max_units"])
            self.assertEqual(left[0]["access"], left[1]["access"])


if __name__ == "__main__":
    unittest.main()
