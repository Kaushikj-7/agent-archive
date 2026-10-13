#!/usr/bin/env python3
"""Record one child's kernel peak RSS, wall time and exit status.

The measured process includes fixture construction and setup. This is separate
from the qualification harness's sampled command heap. wait4 returns this
specific child's rusage, never cumulative parent RUSAGE_CHILDREN statistics.
"""
import argparse
import json
import os
import pathlib
import subprocess
import sys
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command
    if command[:1] == ["--"]:
        command = command[1:]
    if not command:
        parser.error("a child command is required")
    if not hasattr(os, "wait4") or sys.platform not in ("linux", "darwin"):
        parser.error("kernel wait4 RSS units are supported only on Linux/macOS")
    started = time.monotonic()
    try:
        child = subprocess.Popen(command)
    except OSError as error:
        args.output.write_text(json.dumps({"process_maxrss_kib": None,
                                          "process_wall_seconds": time.monotonic() - started,
                                          "exit": 127, "error": str(error)}) + "\n")
        return 127
    _, status, usage = os.wait4(child.pid, 0)
    child.returncode = os.waitstatus_to_exitcode(status)
    # Linux ru_maxrss is KiB; macOS reports bytes. This is the high-water RSS
    # of the waited child, including its entire construction/setup lifetime.
    peak_kib = usage.ru_maxrss / 1024 if sys.platform == "darwin" else usage.ru_maxrss
    args.output.write_text(json.dumps({"process_maxrss_kib": peak_kib,
                                      "process_wall_seconds": time.monotonic() - started,
                                      "exit": child.returncode,
                                      "rss_source": "kernel wait4 child ru_maxrss",
                                      "rss_native_unit": "bytes" if sys.platform == "darwin" else "KiB"}) + "\n")
    return child.returncode if child.returncode >= 0 else 128 - child.returncode


if __name__ == "__main__":
    sys.exit(main())
