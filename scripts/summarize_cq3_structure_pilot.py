#!/usr/bin/env python3

import json
import pathlib
import statistics
import sys


root = pathlib.Path(sys.argv[1])
case_order = (
    "serial-oracle",
    "runtime-l1",
    "runtime-l8",
    "runtime-lw",
    "direct-l8",
    "direct-lw",
    "estimate-l8",
    "estimate-lw",
    "direct-estimate-l8",
    "direct-estimate-lw",
    "summary-l8",
    "summary-lw",
    "summary-estimate-l8",
    "summary-estimate-lw",
)
header = ("workload", *case_order, "summary_vs_direct_lw_pct", "estimate_vs_direct_lw_pct", "best")
print(",".join(header))
for path in sorted(root.glob("**/runs.jsonl")):
    measurements = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        record = json.loads(line)
        if record["phase"] != "measurement":
            continue
        measurements.setdefault(record["case"]["id"], []).append(record["timing"]["execution_ns"] / 1e6)
    medians = {case: statistics.median(values) for case, values in measurements.items()}
    ranked = sorted(medians.items(), key=lambda item: item[1])
    summary_gain = 100 * (medians["direct-lw"] - medians["summary-lw"]) / medians["direct-lw"]
    estimate_gain = 100 * (medians["direct-lw"] - medians["estimate-lw"]) / medians["direct-lw"]
    fields = [path.parent.name]
    fields.extend(f"{medians[case]:.3f}" for case in case_order)
    fields.extend((f"{summary_gain:.2f}", f"{estimate_gain:.2f}", f"{ranked[0][0]}:{ranked[0][1]:.3f}"))
    print(",".join(fields))
