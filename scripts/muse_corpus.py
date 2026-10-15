#!/usr/bin/env python3
"""Project reviewed synthetic native sessions into a derived Q0 corpus.

No record or position is removed. Omitted bodies are recorded in the manifest.
This is a one-time review helper, NOT the production privacy filter or decoder.
The output is never certified by native checksums (even when its first frame is
kept separately as an exact native vector). Do not run on personal transcripts.
"""

import argparse
import copy
import json
import pathlib

from muse_qualification import PINNED_SHA256, PROMPT, REPLY, digest, logical_rows, write_json


ROOT = pathlib.Path(__file__).resolve().parent.parent
ALLOWLIST = ROOT / "dev/qualification/muse/retained-allowlist.json"


def project(value, paths):
    output = {}
    for path in paths:
        keys = path.split(".")
        source, target = value, output
        for index, key in enumerate(keys):
            if not isinstance(source, dict) or key not in source:
                break
            if index == len(keys) - 1:
                target[key] = copy.deepcopy(source[key])
            else:
                source = source[key]
                target = target.setdefault(key, {})
    return output


def sanitize(data, source_root):
    allow = json.loads(ALLOWLIST.read_text())
    output, omissions = [], []
    for line, child, row in logical_rows(data):
        envelope = project(row, allow["envelope"])
        payload = row.get("payload", {})
        fields = allow["common_payload"] + allow["payload_types"].get(row.get("payload_type"), [])
        safe = project(payload, fields)
        if row.get("payload_type") == "runtime.session":
            event = payload.get("event", {})
            safe["event"] = project(event, ["kind"] + allow["runtime_events"].get(event.get("kind"), []))
            for key, paths in allow.get("runtime_event_arrays", {}).get(event.get("kind"), {}).items():
                safe["event"][key] = [project(item, paths) for item in event.get(key, [])]
        envelope["payload"] = safe
        if safe != payload:
            omissions.append({"line": line, "child": child, "type": row.get("payload_type"),
                              "event": payload.get("event", {}).get("kind")})
        # Scratch path spellings are replaced after allowlist projection. IDs,
        # sequence and timestamps are preserved; checksum validity is not claimed.
        encoded = json.dumps({"pos": {"line": line, "child": child}, "record": envelope},
                             separators=(",", ":"), ensure_ascii=False)
        output.append(encoded.replace(str(source_root), "/synthetic/muse-q0"))
    return ("\n".join(output) + "\n").encode(), omissions


def summarize(data):
    rows = [json.loads(line)["record"] for line in data.splitlines()]
    runs, texts, usage, calls, results = [], [], [], [], []
    for row in rows:
        payload = row.get("payload", {})
        event = payload.get("event", {})
        if row.get("payload_type") != "runtime.session" or payload.get("kind") != "run":
            continue
        if event.get("kind") == "started":
            runs.append({"id": payload["run_id"], "prompt": event.get("prompt")})
        if event.get("kind") == "assistant_message_committed":
            texts.append({"run_id": payload["run_id"], "text": event.get("text")})
        if event.get("kind") == "assistant_tool_calls_committed":
            calls.extend({"run_id": payload["run_id"], **call} for call in event.get("tool_calls", []))
        if event.get("kind") == "tool_result_batch_committed":
            results.extend({"run_id": payload["run_id"], **result} for result in event.get("results", []))
        if event.get("kind") == "model_completed":
            usage.append({"run_id": payload["run_id"], "model": event.get("model"), "usage": event.get("usage")})
    return {"logical_records": len(rows), "own_run_starts": runs, "visible_messages": texts,
            "model_completions": usage, "tool_calls": calls, "tool_results": results,
            "opened": [row["payload"] for row in rows if row.get("payload_type") == "session.opened.observed"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--native-session", type=pathlib.Path, required=True)
    parser.add_argument("--source-root", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True,
                        help="new file; inspect every emitted value before adding to git")
    args = parser.parse_args()
    if args.output.exists():
        parser.error("output already exists")
    native = args.native_session.read_bytes()
    sanitized, omissions = sanitize(native, args.source_root)
    expected = summarize(sanitized)
    if any(run["prompt"] != PROMPT for run in expected["own_run_starts"]) or any(message["text"] != REPLY for message in expected["visible_messages"]):
        parser.error("input is not the controlled synthetic qualification conversation")
    args.output.write_bytes(sanitized)
    write_json(args.output.with_suffix(".manifest.json"), {
        "schema": 1, "synthetic": True, "producer": "1.4.8-R6256.1",
        "binary_sha256": PINNED_SHA256, "provider": "loopback synthetic Responses",
        "source_base": "37609182169b658c380e9113888b0cd11d387c05",
        "procedure": "scripts/muse_qualification.py; complete settled log after owned process cleanup",
        "native_sha256": digest(native), "native_physical_lines": len(native.splitlines()),
        "sanitized_sha256": digest(sanitized), "allowlist_sha256": digest(ALLOWLIST.read_bytes()),
        "native_integrity_valid": False, "omitted_bodies": omissions, "expected": expected})


if __name__ == "__main__":
    main()
