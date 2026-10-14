#!/usr/bin/env python3
"""Copy the identical repaired picker harness to an isolated protocol10 baseline.

Never changes production source. The destination must be a clean checkout at the
pinned baseline; only these new test files are allowed to exist afterwards.
"""
import argparse
import hashlib
import pathlib
import subprocess

BASELINE = "a187e1b883924fd36c9d6700606a5c2756fc2b6f"
FILES = ("picker_qualification_fixture_test.go", "picker_qualification_test.go")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("baseline", type=pathlib.Path)
    args = parser.parse_args()
    source = pathlib.Path(__file__).resolve().parent.parent
    destination = args.baseline.resolve()
    if destination == source:
        parser.error("baseline must be a separate checkout")
    head = subprocess.check_output(["git", "-C", str(destination), "rev-parse", "HEAD"], text=True).strip()
    if head != BASELINE:
        parser.error(f"baseline head must be {BASELINE}; got {head}")
    allowed = {"internal/cli/" + name for name in FILES} | {"internal/cli/picker_qualification_format_test.go"}
    status = subprocess.check_output(["git", "-C", str(destination), "status", "--porcelain"], text=True)
    for row in status.splitlines():
        if not row.startswith("?? ") or row[3:] not in allowed:
            parser.error("baseline contains changes outside the qualification harness")
    for name in FILES:
        data = (source / "internal/cli" / name).read_bytes()
        (destination / "internal/cli" / name).write_bytes(data)
        print(name, hashlib.sha256(data).hexdigest())
    data = (source / "dev/maintainers/picker-qualification/baseline-format.go.txt").read_bytes()
    (destination / "internal/cli/picker_qualification_format_test.go").write_bytes(data)
    print("protocol10-format-adapter", hashlib.sha256(data).hexdigest())


if __name__ == "__main__":
    main()
