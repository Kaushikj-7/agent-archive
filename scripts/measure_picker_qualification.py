#!/usr/bin/env python3
"""Build once, then run paired synthetic picker processes serially.

Run only after the shared validation lane is assigned. Runtime outputs contain
counts and synthetic fixture hashes, never real homes, credentials or bodies.
"""
import argparse
import hashlib
import json
import os
import pathlib
import platform
import subprocess
import sys

BASELINE = "a187e1b883924fd36c9d6700606a5c2756fc2b6f"
COMMON = ("picker_qualification_fixture_test.go", "picker_qualification_test.go")
ADAPTER = "picker_qualification_format_test.go"


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def git(checkout, *args):
    return subprocess.check_output(["git", "-C", str(checkout), *args], text=True).strip()


def run(command, cwd, env, logfile):
    with logfile.open("w") as stream:
        subprocess.run(command, cwd=cwd, env=env, stdout=stream,
                       stderr=subprocess.STDOUT, check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=pathlib.Path, required=True)
    parser.add_argument("--candidate", type=pathlib.Path, required=True)
    parser.add_argument("--go", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--repeats", type=int, default=2)
    parser.add_argument("--sizes", type=int, nargs="+", default=[10000, 100000], choices=[10000, 100000])
    args = parser.parse_args()
    if args.repeats < 1:
        parser.error("repeats must be positive")
    sources = {"baseline": args.baseline.resolve(), "candidate": args.candidate.resolve()}
    if git(sources["baseline"], "rev-parse", "HEAD") != BASELINE:
        parser.error("baseline production source differs from approved pin")
    if git(sources["candidate"], "status", "--short"):
        parser.error("candidate must be a committed, clean source checkpoint")
    allowed_baseline = {"?? internal/cli/" + name for name in (*COMMON, ADAPTER)}
    if set(git(sources["baseline"], "status", "--short").splitlines()) != allowed_baseline:
        parser.error("baseline may contain only the three injected qualification files")
    for name in COMMON:
        if sha(sources["baseline"] / "internal/cli" / name) != sha(sources["candidate"] / "internal/cli" / name):
            parser.error("baseline/candidate common repaired harness differs: " + name)
    env = os.environ.copy()
    for name in ("GOCACHE", "GOMODCACHE", "GOPATH", "GOTMPDIR", "TMPDIR"):
        if not env.get(name):
            parser.error("set explicit isolated " + name)
    env.update(GOMAXPROCS="2", AGENT_ARCHIVE_PERF="1")
    version = subprocess.check_output([str(args.go), "version"], env=env, text=True).strip()
    if "go1.27.2 " not in version:
        parser.error("Go1.27.2 exactly required")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    manifest = {"toolchain": version, "platform": platform.platform(),
                "cpu_count": os.cpu_count(), "GOMAXPROCS": 2, "compile_p": 2,
                "terminal_first": [120, 70], "terminal_older": [120, 120], "repeats": args.repeats, "sizes": args.sizes,
                "common_harness": {name: sha(sources["candidate"] / "internal/cli" / name) for name in COMMON},
                "sources": {}, "completed": [], "failed": None,
                "memory_limit": "process-maxrss includes fixture/setup; command heap is sampled separately",
                "process_measurement": "kernel wait4 per-child ru_maxrss; Linux KiB/macOS bytes normalized",
                "process_measurement_adapter_sha256": sha(sources["candidate"] / "scripts/measure_picker_process.py")}
    record = output / "manifest.json"
    if record.exists():
        parser.error("use a fresh output directory; an existing manifest is immutable evidence")
    for label, checkout in sources.items():
        manifest["sources"][label] = {"production_head": git(checkout, "rev-parse", "HEAD"),
                                      "status": git(checkout, "status", "--short"),
                                      "format_adapter_sha256": sha(checkout / "internal/cli" / ADAPTER)}
    record.write_text(json.dumps(manifest, indent=2) + "\n")

    def verify_inputs():
        if sha(sources["candidate"] / "scripts/measure_picker_process.py") != manifest["process_measurement_adapter_sha256"]:
            raise RuntimeError("process measurement adapter changed during qualification")
        for label, checkout in sources.items():
            pinned = manifest["sources"][label]
            if git(checkout, "rev-parse", "HEAD") != pinned["production_head"] or git(checkout, "status", "--short") != pinned["status"]:
                raise RuntimeError("source checkpoint changed during qualification: " + label)
            if sha(checkout / "internal/cli" / ADAPTER) != pinned["format_adapter_sha256"]:
                raise RuntimeError("format adapter changed during qualification: " + label)
            for name in COMMON:
                if sha(checkout / "internal/cli" / name) != manifest["common_harness"][name]:
                    raise RuntimeError("common harness changed during qualification: " + label)

    try:
        for label, checkout in sources.items():
            verify_inputs()
            binary = output / (label + ".test")
            run([str(args.go), "test", "-p", "2", "-c", "-o", str(binary), "./internal/cli"],
                checkout, env, output / (label + "-build.txt"))
            manifest["sources"][label]["binary_sha256"] = sha(binary)
            record.write_text(json.dumps(manifest, indent=2) + "\n")
        for size in args.sizes:
            for kind in ("main", "native"):
                benchmark = "^BenchmarkPickerQualification$" if kind == "main" else "^BenchmarkPickerQualificationNative$"
                for repeat in range(args.repeats):
                    # Alternate which source runs first; no processes overlap.
                    order = list(sources) if repeat % 2 == 0 else list(reversed(sources))
                    for label in order:
                        verify_inputs()
                        name = f"{size}-{kind}-{repeat + 1}-{label}"
                        current_env = dict(env, AGENT_ARCHIVE_PICKER_SIZE=str(size))
                        command = [sys.executable, str(sources["candidate"] / "scripts/measure_picker_process.py"),
                                   "--output", str(output / (name + "-process.json")), "--",
                                   str(output / (label + ".test")), "-test.run=^$",
                                   "-test.bench=" + benchmark, "-test.benchtime=1x",
                                   "-test.count=1", "-test.timeout=30m"]
                        run(command, sources[label], current_env, output / (name + ".txt"))
                        manifest["completed"].append(name)
                        record.write_text(json.dumps(manifest, indent=2) + "\n")
    except (subprocess.CalledProcessError, RuntimeError, OSError) as error:
        manifest["failed"] = {"error": str(error)}
        if isinstance(error, subprocess.CalledProcessError):
            manifest["failed"].update(command=error.cmd, exit=error.returncode)
        record.write_text(json.dumps(manifest, indent=2) + "\n")
        raise


if __name__ == "__main__":
    main()
