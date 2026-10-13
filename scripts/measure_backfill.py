#!/usr/bin/env python3
"""Measure a frozen CLI test executable in isolated subprocesses.

Build outside this runner with go test -c ./internal/cli. CPU and peak RSS are
whole-subprocess resources, including fixture setup and output verification;
elapsed_seconds times only complete CLI Run. No real homes/providers are used.
"""
import argparse
import json
import os
import platform
import resource
import subprocess
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--samples", type=int, default=5)
    parser.add_argument("--scenario", action="append", choices=["recent", "old", "large", "related"])
    parser.add_argument("--small", action="store_true")
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    scenarios = args.scenario or ["recent", "old", "large", "related"]
    for scenario in scenarios:
        modes = ["import"] if scenario in ["large", "related"] else ["plan", "import"]
        for mode in modes:
            for index in range(args.samples):
                env = os.environ.copy()
                env.update(AA_BACKFILL_SCENARIO=scenario, AA_BACKFILL_MODE=mode)
                if args.small:
                    env["AA_BACKFILL_SMALL"] = "1"
                # wait4 returns this process's resource usage, not a cumulative
                # high-water mark across earlier children.
                with (args.output / f"{scenario}-{mode}-{index}.log").open("wb") as log:
                    proc = subprocess.Popen([str(args.binary.resolve()), "-test.run", "^TestBackfillCommandMeasurement$", "-test.v", "-test.timeout", "20m"], env=env, stdout=log, stderr=subprocess.STDOUT)
                    _, status, usage = os.wait4(proc.pid, 0)
                    proc.returncode = os.waitstatus_to_exitcode(status)
                content = (args.output / f"{scenario}-{mode}-{index}.log").read_text()
                if proc.returncode:
                    raise SystemExit(content)
                line = next(line for line in content.splitlines() if line.startswith("AA_BACKFILL_RESULT "))
                result = json.loads(line.split(" ", 1)[1])
                result.update(sample=index, process_cpu_seconds=usage.ru_utime + usage.ru_stime,
                              process_peak_rss_bytes=usage.ru_maxrss * (1 if platform.system() == "Darwin" else 1024),
                              host=platform.platform(), resource_scope="whole subprocess; fixture and verification included")
                with (args.output / "samples.jsonl").open("a") as out:
                    out.write(json.dumps(result, sort_keys=True) + "\n")
                print(f"{scenario} {mode} {index}: {result['elapsed_seconds']:.3f}s", flush=True)


if __name__ == "__main__":
    main()
