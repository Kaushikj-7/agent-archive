#!/usr/bin/env python3
"""Summarize completed paired synthetic runs; preserve raw phase observations."""
import argparse
import hashlib
import json
import pathlib
import statistics

parser = argparse.ArgumentParser()
parser.add_argument("output", type=pathlib.Path)
args = parser.parse_args()
manifest = json.loads((args.output / "manifest.json").read_text())
if manifest["failed"]:
    raise SystemExit("matrix incomplete: failed process; do not claim paired qualification")
expected_processes = len(manifest["sizes"]) * 2 * manifest["repeats"] * 2
if len(manifest["completed"]) != expected_processes:
    raise SystemExit("matrix incomplete: missing paired process")
rows, processes = [], []
for name in manifest["completed"]:
    size, kind, repeat, source = name.split("-")
    raw = args.output / (name + ".txt")
    parsed = []
    for line in raw.read_text().splitlines():
        if "{\"NativeStats\":" not in line:
            continue
        result = json.loads(line[line.index("{"):])
        if result["Sessions"] != int(size) or result["Format"] != (10 if source == "baseline" else 11):
            raise SystemExit("wrong source/size result: " + name)
        result.update(Source=source, Repeat=int(repeat), Kind=kind)
        parsed.append(result)
    if len(parsed) != (9 if kind == "main" else 4):
        raise SystemExit("missing/extra measured cases: " + name + " count=" + str(len(parsed)))
    identities = {(r["Command"], r["CacheState"], r["Registrations"]) for r in parsed}
    if len(identities) != len(parsed):
        raise SystemExit("duplicate measured case: " + name)
    if source == "candidate":
        for result in parsed:
            for phase in ("First", "Older"):
                reads = result[phase]["Reads"]
                if any(reads[k] != 0 for k in ("Lists", "Metadata", "Sources", "NonProjectRoots")):
                    raise SystemExit("candidate used forbidden archive work: " + name)
            if result["Older"]["Reads"]["Head"] < 1:
                raise SystemExit("candidate Older lacks fresh head: " + name)
            if result["Registrations"] == 0 and result["Older"]["Reads"]["Coordinator"] < 1:
                raise SystemExit("candidate archive Older lacks coordinator: " + name)
    rows.extend(parsed)
    process = json.loads((args.output / (name + "-process.json")).read_text())
    if process["exit"] != 0:
        raise SystemExit("unsuccessful process: " + name)
    process.update(Name=name, Source=source, Sessions=int(size), Kind=kind, Repeat=int(repeat),
                   RawLogSHA256=hashlib.sha256(raw.read_bytes()).hexdigest())
    processes.append(process)
pairs = {}
for row in rows:
    key = (row["Sessions"], row["Kind"], row["Command"], row["CacheState"], row["Registrations"], row["Repeat"])
    pairs.setdefault(key, {})[row["Source"]] = row
for key, pair in pairs.items():
    if set(pair) != {"baseline", "candidate"} or pair["baseline"]["FixtureSHA256"] != pair["candidate"]["FixtureSHA256"]:
        raise SystemExit("unmatched logical fixture pair: " + str(key))
summary = []
groups = {}
for row in rows:
    key = (row["Source"], row["Sessions"], row["Kind"], row["Command"], row["CacheState"], row["Registrations"])
    groups.setdefault(key, []).append(row)
for key, samples in sorted(groups.items()):
    source, size, kind, command, cache, registrations = key
    if len(samples) != manifest["repeats"]:
        raise SystemExit("missing repeat: " + str(key))
    entry = dict(Source=source, Sessions=size, Kind=kind, Command=command,
                 CacheState=cache, Registrations=registrations, Samples=len(samples))
    for phase in ("First", "Older"):
        entry[phase] = {}
        for metric in ("ElapsedNS", "AllocBytes", "AllocObjects", "NativeStats", "NativeActivityRefs", "NativeOpens", "NativeBytes"):
            values = [r[phase][metric] for r in samples]
            entry[phase][metric] = dict(Min=min(values), Median=statistics.median(values), Max=max(values))
        entry[phase]["Reads"] = {}
        for metric in samples[0][phase]["Reads"]:
            values = [r[phase]["Reads"][metric] for r in samples]
            entry[phase]["Reads"][metric] = dict(Min=min(values), Median=statistics.median(values), Max=max(values))
    for metric in ("SampledPeakHeapInuse", "InitialHeapInuse"):
        values = [r[metric] for r in samples]
        entry[metric] = dict(Min=min(values), Median=statistics.median(values), Max=max(values))
    summary.append(entry)
artifact = dict(Manifest=manifest, PhaseSamples=rows, CaseSummary=summary, ProcessSamples=processes,
                Limits="Synthetic transport; sampled heap includes fixture residency; process RSS includes construction/setup; repeat medians are observations, not confidence intervals; production provider/clock/cutover HOLD")
(args.output / "summary.json").write_text(json.dumps(artifact, indent=2) + "\n")
print("verified", len(rows), "phase result records;", len(pairs), "matched fixture pairs;", len(processes), "successful serial processes")
