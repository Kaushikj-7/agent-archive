#!/usr/bin/env python3
"""Compare five or more matched frozen backfill command samples."""
import argparse
import json
import statistics
from collections import defaultdict
from pathlib import Path


def read(path):
    groups = defaultdict(list)
    for line in path.read_text().splitlines():
        row = json.loads(line)
        groups[(row["scenario"], row["mode"], row.get("state", "cold"), row.get("trace", False))].append(row)
    return groups


def median(rows, field):
    return statistics.median(row[field] for row in rows)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("baseline", type=Path)
    parser.add_argument("candidate", type=Path)
    args = parser.parse_args()
    before, after = read(args.baseline), read(args.candidate)
    if not before or not after:
        raise SystemExit("At least five matched samples required; input is empty")
    if before.keys() != after.keys():
        raise SystemExit("Scenario/mode/state/trace sets differ")
    print("| Scenario / mode / state | Sessions / bytes | Original seconds (range) | Candidate seconds (range) | Imported/s original → candidate | CPU seconds original → candidate | RSS MiB original → candidate | Speedup |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- |")
    for key, original in before.items():
        candidate = after[key]
        if min(len(original), len(candidate)) < 5:
            raise SystemExit(f"At least five samples required: {key}")
        indices = [{row["sample"] for row in rows} for rows in [original, candidate]]
        if any(len(index) != len(rows) for index, rows in zip(indices, [original, candidate])):
            raise SystemExit(f"Duplicate sample indices: {key}")
        if indices[0] != indices[1]:
            raise SystemExit(f"Unmatched sample indices: {key}")
        for field in ["host", "logical_cpus", "gomaxprocs", "fixture_version", "toolchain", "planning_workers", "default_adapter_workers", "byte_budget", "sessions", "selected", "imported", "skipped", "input_bytes", "artifact_sha256", "plan_sha256"]:
            values = {row[field] for row in original + candidate}
            if len(values) != 1:
                raise SystemExit(f"Non-equivalent {field}: {key}: {values}")
        seconds = [median(rows, "elapsed_seconds") for rows in [original, candidate]]
        timing = [f"{seconds[i]:.3f} ({min(r['elapsed_seconds'] for r in rows):.3f}–{max(r['elapsed_seconds'] for r in rows):.3f})" for i, rows in enumerate([original, candidate])]
        cpu = [median(rows, "command_cpu_seconds") for rows in [original, candidate]]
        rss = [median(rows, "command_peak_rss_at_return_bytes") / (1 << 20) for rows in [original, candidate]]
        throughput = [original[0]["imported"] / value for value in seconds]
        row = original[0]
        print(f"| {' / '.join(str(value) for value in key)} | {row['sessions']} / {row['input_bytes']} | {timing[0]} | {timing[1]} | {throughput[0]:.2f} → {throughput[1]:.2f} | {cpu[0]:.3f} → {cpu[1]:.3f} | {rss[0]:.1f} → {rss[1]:.1f} | {seconds[0]/seconds[1]:.3f}× |")


if __name__ == "__main__":
    main()
