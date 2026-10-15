#!/usr/bin/env python3
"""Reproduce Muse producer observations, never archive or grant capture consent.

Native logs remain in the explicitly supplied disposable output directory. This
program never downloads a binary, inherits credentials, or promotes raw logs to
fixtures. See dev/qualification/muse/README.md before running native probes.
"""

import argparse
import collections
import contextlib
import datetime
import fcntl
import hashlib
import http.server
import json
import os
import pathlib
import pty
import queue
import re
import select
import shlex
import signal
import subprocess
import struct
import sys
import threading
import time
import termios
import uuid


PINNED_SHA256 = "94ea8fd1c73664c22e5c6157d8706945b667b11bd1c7c51691811e79f17dfcf1"
MODEL = "archive-synthetic-model"
PROMPT = "Synthetic qualification prompt: hello λ.\nSecond synthetic line."
REPLY = "Synthetic qualification answer λ.\nSecond synthetic line."
OUTPUT_LIMIT = 8 * 1024 * 1024  # Per captured stream; exceeding it fails the case.
MSP_LINE_LIMIT = 1024 * 1024
MSP_QUEUE_LIMIT = 1024
FILE_LIMIT = 32 * 1024 * 1024  # Harness input/export cap, not a native format limit.
HOOK_LIMIT = 1024 * 1024
USAGE = {"input_tokens": 101, "output_tokens": 23, "total_tokens": 124,
         "input_tokens_details": {"cached_tokens": 17},
         "output_tokens_details": {"reasoning_tokens": 5}}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def file_digest(path):
    value = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def read_limited(path, limit=None):
    """Refuse oversized local evidence before allocating or decoding its body."""
    limit = FILE_LIMIT if limit is None else limit
    with path.open("rb") as source:
        data = source.read(limit + 1)
    if len(data) > limit:
        raise RuntimeError("qualification evidence exceeded file byte limit")
    return data


def command_id():
    # The pinned MSP explicitly requires UUIDv7 command IDs, not UUIDv4.
    value = (int(time.time() * 1000) << 80) | (7 << 76) | (uuid.uuid4().int & ((1 << 76) - 1))
    value = (value & ~(3 << 62)) | (2 << 62)
    return str(uuid.UUID(int=value))


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, ensure_ascii=False) + "\n")


def isolated_env(root, timezone="UTC"):
    """Construct, rather than edit, the environment; no host auth or proxies."""
    values = {"PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "LANG": "en_US.UTF-8",
              "TERM": "xterm-256color", "TZ": timezone, "MUSE_NO_AUTO_UPDATE": "1",
              "TBH_CREDENTIAL_BACKEND": "file",
              "META_API_KEY": "synthetic-loopback-only-not-a-credential"}
    for name, directory in (("HOME", "home"), ("XDG_CONFIG_HOME", "config"),
                            ("XDG_DATA_HOME", "data"), ("XDG_CACHE_HOME", "cache"),
                            ("XDG_STATE_HOME", "state"), ("TMPDIR", "tmp"),
                            ("AGENT_ARCHIVE_HOME", "archive")):
        path = root / directory
        path.mkdir()
        values[name] = str(path)
    # Explicit dummy files also prevent future SDK fallback to shared AWS state.
    for name in ("AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE"):
        path = root / name.lower()
        path.write_text("")
        values[name] = str(path)
    return values


def stop_process(process):
    """Reap the owned session even if its leader has already exited."""
    with contextlib.suppress(ProcessLookupError):
        os.killpg(process.pid, signal.SIGTERM)
    try:
        process.wait(timeout=2)
    except subprocess.TimeoutExpired:
        pass
    with contextlib.suppress(ProcessLookupError):
        os.killpg(process.pid, signal.SIGKILL)
    process.wait(timeout=5)


@contextlib.contextmanager
def termination_cleanup():
    """Unwind owned-process cleanup when the CLI receives SIGTERM."""
    def terminate(signum, _frame):
        raise SystemExit(128 + signum)
    previous = signal.signal(signal.SIGTERM, terminate)
    try:
        yield
    finally:
        signal.signal(signal.SIGTERM, previous)


def run_process(argv, *, env, cwd, timeout=40):
    process = subprocess.Popen(argv, env=env, cwd=cwd, stdin=subprocess.DEVNULL,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               start_new_session=True)
    captured = {process.stdout: bytearray(), process.stderr: bytearray()}
    pending = list(captured)
    deadline = time.monotonic() + timeout
    timed_out = False
    try:
        while pending:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                timed_out = True
                break
            ready, _, _ = select.select(pending, [], [], min(.2, remaining))
            for stream in ready:
                chunk = os.read(stream.fileno(), 65536)
                if not chunk:
                    pending.remove(stream)
                    continue
                if len(captured[stream]) + len(chunk) > OUTPUT_LIMIT:
                    raise RuntimeError("native command exceeded captured stream byte limit")
                captured[stream].extend(chunk)
        if not timed_out:
            try:
                process.wait(timeout=max(.001, deadline - time.monotonic()))
            except subprocess.TimeoutExpired:
                timed_out = True
    finally:
        stop_process(process)
        process.stdout.close()
        process.stderr.close()
    return {"argv": argv, "exit": process.returncode,
            "stdout": captured[process.stdout].decode(errors="replace"),
            "stderr": captured[process.stderr].decode(errors="replace"), "timeout": timed_out}


class Endpoint(http.server.BaseHTTPRequestHandler):
    """Loopback Responses fixture; incoming context is discarded immediately."""

    def log_message(self, *args):
        pass

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"object": "list", "data": [{
            "id": MODEL, "object": "model", "metadata": {"muse-code": {
                "release_date": "2026-01-01", "is_hidden": False,
                "limit": {"context": 1000000, "output": 1024}}}}]}).encode())

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        if not 0 < length <= 8 * 1024 * 1024:
            self.send_error(413)
            return
        # Inspect only to reject malformed requests. Do not retain provider context.
        body = json.loads(self.rfile.read(length))
        with self.server.request_lock:
            self.server.requests += 1
            index = self.server.requests
        frame = {"id": "resp_synthetic_" + str(index), "object": "response",
                 "model": MODEL, "status": "in_progress", "output": []}
        events = [
            {"type": "response.created", "sequence_number": 1, "response": frame},
            {"type": "response.output_text.delta", "sequence_number": 2,
             "output_index": 0, "item_id": "msg_synthetic_" + str(index),
             "content_index": 0, "delta": REPLY},
            {"type": "response.completed", "sequence_number": 3,
             "response": dict(frame, status="completed", usage=USAGE)},
        ]
        if self.server.tool and index == 1:
            tools = [item for tool in body.get("tools", []) for item in tool.get("tools", [tool])]
            if not any(tool.get("name") == "read_file" for tool in tools):
                self.send_error(422)
                return
            events[1] = {"type": "response.function_call_arguments.done", "sequence_number": 2,
                         "output_index": 0, "item_id": "fc_synthetic", "namespace": "muse",
                         "name": "read_file", "call_id": "call_synthetic",
                         "arguments": json.dumps({"path": str(self.server.artifact), "offset": 1, "limit": 10})}
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        try:
            for event in events:
                self.wfile.write(("data: " + json.dumps(event) + "\n\n").encode())
                self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass


class Case:
    def __init__(self, root, binary, timezone="UTC", tool=False):
        root.mkdir()
        self.root, self.binary = root, binary
        self.env = isolated_env(root, timezone)
        self.workspace = root / "workspace"
        self.workspace.mkdir()
        (self.workspace / "artifact.txt").write_text("SYNTHETIC_TOOL_RESULT λ\n")
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Endpoint)
        self.server.daemon_threads = True
        self.server.requests = 0
        self.server.request_lock = threading.Lock()
        self.server.tool = tool
        self.server.artifact = self.workspace / "artifact.txt"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        config = root / "config/muse"
        config.mkdir()
        # The recorder intentionally drops prompt/tool/result and arbitrary env.
        hook = root / "hook.py"
        hook.write_text("import json,os,sys\n"
                        "p=json.loads(sys.stdin.read(65536))\n"
                        "p={k:p[k] for k in ('hook_event_name','session_id','transcript_path','cwd','source','reason') if k in p}\n"
                        "e={k:os.environ[k] for k in ('MUSE_SESSION_ID','MUSE_THREAD_ID') if k in os.environ}\n"
                        "with open(sys.argv[1],'a') as f:f.write(json.dumps({'payload':p,'env':e})+'\\n')\n")
        command = shlex.join([sys.executable, str(hook), str(root / "hooks.jsonl")])
        hooks = {event: [{"hooks": [{"type": "command", "command": command, "timeout": 3}]}]
                 for event in ("SessionStart", "UserPromptSubmit", "Stop", "SessionEnd",
                               "PostToolUseFailure", "StopFailure")}
        write_json(config / "settings.json", {"schema_version": 1,
                   "endpoint_transport": {"base_url": "http://127.0.0.1:" +
                                          str(self.server.server_port), "auth": "bearer"}, "hooks": hooks})
        write_json(config / "auth.json", {"schema_version": 1, "providers": {
            "meta": {"api_key": "synthetic-loopback-only-not-a-credential"}}})

    def argv(self, mode):
        return [str(self.binary), mode, "--provider", "meta", "--model", MODEL,
                "--disable-write", "--disable-shell", "--trust-workspace"]

    def execute(self, extra=()):
        return run_process(self.argv("exec") + ["--disable-reminders", "--no-foreign-personal-context",
                           "--max-model-steps", "3", "--workspace",
                           str(self.workspace)] + list(extra) + [PROMPT],
                           env=self.env, cwd=self.workspace)

    def logs(self):
        return sorted((self.root / "data/muse/sessions").glob("*/*/*/*/session.jsonl"))

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=3)


def logical_rows(data):
    """Inspection only: deliberately NOT an independent integrity decoder."""
    rows = []
    for ordinal, line in enumerate(data.splitlines(), 1):
        value = json.loads(line)
        if value.get("retained_frame") == "session_permission_transaction":
            for child in value["children"]:
                rows.append((ordinal, child["child_index"], json.loads(child["record_json"])))
        else:
            rows.append((ordinal, None, value))
    return rows


def inspect_session(path):
    data = read_limited(path)
    rows = logical_rows(data)
    opened = [{"line": line, "recorded_at": row.get("recorded_at"),
               "record": row["payload"]["record"]}
              for line, _, row in rows if row.get("payload_type") == "session.opened.observed"]
    kinds = collections.Counter(row.get("payload_type", row.get("retained_marker", "untyped"))
                                for _, _, row in rows)
    events = collections.Counter(row.get("payload", {}).get("event", {}).get("kind", "")
                                 for _, _, row in rows)
    session_id = path.parent.name
    id_millis = uuid.UUID(session_id).int >> 80
    native_date = datetime.datetime.fromtimestamp(id_millis / 1000, datetime.timezone.utc).date()
    return {"sha256": digest(data), "bytes": len(data), "lines": len(data.splitlines()),
            "logical_records": len(rows), "session_id": session_id,
            "folder_date": "-".join(path.parts[-5:-2]), "id_utc_date": str(native_date),
            "opened": opened, "payload_types": dict(sorted(kinds.items())),
            "event_kinds": dict(sorted(events.items()))}


def retained_turn_assertions(path, expected_runs, artifact=None):
    """Check own retained turns, not command acknowledgements or copied context."""
    rows = [row for _, _, row in logical_rows(read_limited(path))]
    runs = [row for row in rows if row.get("payload_type") == "runtime.session" and
            row.get("payload", {}).get("kind") == "run"]
    starts = [row["payload"] for row in runs if row["payload"].get("event", {}).get("kind") == "started"]
    ids = [row.get("run_id") for row in starts]
    events = collections.defaultdict(lambda: collections.defaultdict(list))
    for row in runs:
        payload = row["payload"]
        event = payload.get("event", {})
        events[payload.get("run_id")][event.get("kind")].append(event)
    expected_usage = {"input_tokens": 101, "output_tokens": 23, "cached_tokens": 17,
                      "cache_write_tokens": 0, "cache_read_tokens": 17, "reasoning_tokens": 5}
    checks = {
        "exact_own_starts": len(starts) == expected_runs and len(set(ids)) == expected_runs and
            all(ids) and all(row["event"].get("prompt") == PROMPT for row in starts),
        "own_stream_identity": bool(runs) and all(row.get("stream", {}).get("id") == path.parent.name for row in runs),
        "exact_own_replies": bool(ids) and all(
            [event.get("text") for event in events[run_id]["assistant_message_committed"]] == [REPLY]
            for run_id in ids),
        "exact_own_usage": bool(ids) and all(
            len(events[run_id]["model_completed"]) == (2 if artifact is not None and index == 0 else 1) and
            all(event.get("model") == MODEL and event.get("usage") == expected_usage
                for event in events[run_id]["model_completed"])
            for index, run_id in enumerate(ids)),
        "no_unowned_run_events": set(events) == set(ids),
    }
    calls = [(run_id, call) for run_id, kinds in events.items()
             for event in kinds["assistant_tool_calls_committed"] for call in event.get("tool_calls", [])]
    results = [(run_id, result) for run_id, kinds in events.items()
               for event in kinds["tool_result_batch_committed"] for result in event.get("results", [])]
    checks["exact_own_tools"] = not calls and not results
    if artifact is not None and ids:
        checks["exact_own_tools"] = (len(calls) == len(results) == 1 and
            calls[0][0] == results[0][0] == ids[0] and
            calls[0][1].get("call_id") == results[0][1].get("tool_call_id") == "call_synthetic" and
            calls[0][1].get("name") == "read_file" and
            json.loads(calls[0][1].get("args", "null")) == {"path": str(artifact), "offset": 1, "limit": 10} and
            results[0][1].get("text") == "Read text file `" + str(artifact) + "`.\n1|SYNTHETIC_TOOL_RESULT λ")
    return checks


def export_diagnostics(case, source, name):
    target = case.root / (name + "-export.json")
    result = run_process([str(case.binary), "export", "--session", str(source),
                          "--out", str(target)], env=case.env, cwd=case.workspace)
    # Exit zero is not acceptance. Preserve diagnostics separately and count them.
    diagnostics = re.findall(r"[^\n]*unparseable[^\n]*", result["stderr"], re.IGNORECASE)
    result["unparseable_diagnostics"] = diagnostics
    result["export_written"] = target.is_file()
    result["diagnostics"] = json.loads(read_limited(target).decode()).get("diagnostics", {}) if target.is_file() else {}
    result["unparseable_lines"] = result["diagnostics"].get("unparseable_lines")
    return result


def probe_mutations(case, permission, ordinary):
    compact = lambda value: json.dumps(value, separators=(",", ":"))
    transaction, opened = json.loads(permission), json.loads(ordinary)
    variants = {
        "permission-exact": (permission.decode().rstrip("\n"), 0),
        "permission-checksum-zero": (compact(dict(transaction, content_sha256="sha256:" + "0" * 64)), 1),
        "permission-reordered": (compact(dict(reversed(list(transaction.items())))), 1),
        "permission-leading-space": (" " + permission.decode().rstrip("\n"), 1),
        "permission-future-version": (compact(dict(transaction, frame_schema_version=999)), 1),
        "ordinary-exact": (ordinary.decode().rstrip("\n"), 0),
        "ordinary-future-version": (compact(dict(opened, schema_version=999)), 0),
        "ordinary-missing-version": (compact({key: value for key, value in opened.items() if key != "schema_version"}), 1),
        "ordinary-changed-timestamp": (compact(dict(opened, recorded_at=opened["recorded_at"] + 1)), 0),
    }
    results = {}
    for name, (line, expected) in variants.items():
        path = case.root / ("mutation-" + name + ".jsonl")
        path.write_text(line + "\n")
        exported = export_diagnostics(case, path, "mutation-" + name)
        results[name] = {"input_sha256": file_digest(path), "exit": exported["exit"],
                         "timeout": exported["timeout"], "diagnostics": exported["diagnostics"],
                         "expected_unparseable_lines": expected,
                         "matched": exported["exit"] == 0 and not exported["timeout"] and
                             exported["unparseable_lines"] == expected}
    return results


def probe_exec(case):
    fresh = case.execute()
    write_json(case.root / "fresh-command.json", fresh)
    if fresh["exit"] != 0 or fresh["timeout"] or len(case.logs()) != 1:
        raise RuntimeError("fresh exec did not produce exactly one completed log")
    path = case.logs()[0]
    first = read_limited(path)
    before = inspect_session(path)
    artifact = case.workspace / "artifact.txt" if case.server.tool else None
    fresh_turn = retained_turn_assertions(path, 1, artifact)
    resumed = case.execute(["--session-id", path.parent.name])
    after = inspect_session(path)
    export = export_diagnostics(case, path, "valid")
    frame = first.splitlines(keepends=True)[0]
    baseline = case.root / "permission-exact.jsonl"
    baseline.write_bytes(frame)
    exact = export_diagnostics(case, baseline, "permission-exact")
    value = json.loads(frame)
    value["content_sha256"] = "sha256:" + "0" * 64
    bad = case.root / "permission-invalid.jsonl"
    bad.write_text(json.dumps(value, separators=(",", ":")) + "\n")
    invalid = export_diagnostics(case, bad, "permission-invalid")
    ordinary = next(line for line in first.splitlines() if json.loads(line).get("payload_type") == "session.opened.observed")
    mutations = probe_mutations(case, frame, ordinary)
    copied = case.root / "copied-session.jsonl"
    copied.write_bytes(read_limited(path))
    copy_export = export_diagnostics(case, copied, "copy")
    assertions = {
        **{"fresh_" + key: value for key, value in fresh_turn.items()},
        **{"resumed_" + key: value for key, value in retained_turn_assertions(path, 2, artifact).items()},
        "resume_has_true_open": len(after["opened"]) == 2 and after["opened"][-1]["record"].get("resume") is True,
        "fresh_has_one_false_resume_open": len(before["opened"]) == 1 and
            before["opened"][0]["record"].get("resume") is False,
        "resume_success": resumed["exit"] == 0 and not resumed["timeout"],
        "resume_preserves_prefix": read_limited(path).startswith(first),
        "native_valid_export_clean": export["exit"] == 0 and not export["timeout"] and
            export["export_written"] and export["unparseable_lines"] == 0,
        "native_exact_permission_clean": exact["exit"] == 0 and not exact["timeout"] and
            exact["export_written"] and exact["unparseable_lines"] == 0,
        "native_bad_checksum_diagnostic": invalid["exit"] == 0 and not invalid["timeout"] and
            invalid["export_written"] and invalid["unparseable_lines"] == 1,
        "copy_bytes_identical": read_limited(copied) == read_limited(path),
        "copy_native_export_clean": copy_export["exit"] == 0 and not copy_export["timeout"] and
            copy_export["export_written"] and copy_export["unparseable_lines"] == 0,
        "mutation_diagnostics_match": all(value["matched"] for value in mutations.values()),
    }
    return {"assertions": assertions, "fresh": fresh, "resumed": resumed,
            "before": before, "after": after, "valid_export": export,
            "permission_exact_export": exact, "permission_invalid_export": invalid,
            "copied_export": copy_export, "mutations": mutations, "provider_requests": case.server.requests}


class MSP:
    def __init__(self, case):
        self.case, self.messages, self.index = case, queue.Queue(maxsize=MSP_QUEUE_LIMIT), 0
        self.reader_error = None
        self.completed = set()
        self.stderr = (case.root / "serve-stderr.txt").open("w")
        self.process = subprocess.Popen(case.argv("serve"), env=case.env, cwd=case.workspace,
                                        stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                        stderr=subprocess.PIPE, start_new_session=True)
        def fail(detail):
            self.reader_error = detail
            with contextlib.suppress(ProcessLookupError):
                os.killpg(self.process.pid, signal.SIGKILL)

        def read():
            total = 0
            while True:
                line = self.process.stdout.readline(MSP_LINE_LIMIT + 1)
                if not line:
                    return
                total += len(line)
                if len(line) > MSP_LINE_LIMIT or total > OUTPUT_LIMIT:
                    fail("MSP stdout exceeded line or stream byte limit")
                    return
                try:
                    message = json.loads(line)
                except ValueError:
                    message = {"invalid_json": True}
                try:
                    self.messages.put_nowait(message)
                except queue.Full:
                    fail("MSP pending message limit exceeded")
                    return

        def read_stderr():
            total = 0
            while True:
                chunk = self.process.stderr.read(65536)
                if not chunk:
                    return
                total += len(chunk)
                if total > OUTPUT_LIMIT:
                    fail("MSP stderr exceeded stream byte limit")
                    return
                self.stderr.write(chunk.decode(errors="replace"))

        self.reader = threading.Thread(target=read, daemon=True)
        self.stderr_reader = threading.Thread(target=read_stderr, daemon=True)
        self.reader.start()
        self.stderr_reader.start()

    def send(self, value):
        self.process.stdin.write((json.dumps(value) + "\n").encode())
        self.process.stdin.flush()

    def receive(self, predicate, timeout=25):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if getattr(self, "reader_error", None):
                raise RuntimeError(self.reader_error)
            try:
                message = self.messages.get(timeout=min(.2, max(.001, end - time.monotonic())))
            except queue.Empty:
                continue
            if message.get("method") == "turn/completed":
                self.completed.add(message.get("params", {}).get("sessionId"))
            if predicate(message):
                return message
        raise TimeoutError("MSP response deadline")

    def call(self, method, params):
        self.index += 1
        self.send({"jsonrpc": "2.0", "id": self.index, "method": method, "params": params})
        response = self.receive(lambda message: message.get("id") == self.index)
        if "error" in response:
            write_json(self.case.root / (method.replace("/", "-") + "-error.json"), response)
        return response

    def turn(self, session_id):
        self.completed.discard(session_id)
        response = self.call("turn/start", {"commandId": command_id(), "sessionId": session_id,
                                           "input": [{"type": "text", "text": PROMPT}]})
        if "result" not in response:
            raise RuntimeError("MSP turn/start rejected")
        if session_id not in self.completed:
            self.receive(lambda message: message.get("method") == "turn/completed" and
                         message.get("params", {}).get("sessionId") == session_id)

    def close(self):
        with contextlib.suppress(BrokenPipeError):
            self.process.stdin.close()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            pass
        finally:
            stop_process(self.process)
            self.reader.join(timeout=3)
            self.stderr_reader.join(timeout=3)
            self.process.stdout.close()
            self.process.stderr.close()
            self.stderr.close()
        if self.reader_error:
            raise RuntimeError(self.reader_error)


def probe_msp(case):
    msp = MSP(case)
    try:
        initialized = msp.call("initialize", {"clientInfo": {"name": "archive_qualification", "version": "1"}})
        if "result" not in initialized:
            raise RuntimeError("MSP initialize rejected")
        msp.send({"jsonrpc": "2.0", "method": "initialized"})
        started = msp.call("session/start", {"commandId": command_id(),
                                            "workspaceRoot": str(case.workspace), "modelId": MODEL})
        if "result" not in started:
            raise RuntimeError("session/start rejected: " + json.dumps(started.get("error")))
        session_id = started["result"]["session"]["sessionId"]
        msp.turn(session_id)
        forked = msp.call("session/fork", {"commandId": command_id(), "sessionId": session_id})
        fork_id = forked.get("result", {}).get("session", {}).get("sessionId")
        if fork_id:
            msp.turn(fork_id)
    finally:
        msp.close()
    logs = {path.parent.name: path for path in case.logs()}
    assertions = {"root_log_present": session_id in logs, "fork_log_present": bool(fork_id) and fork_id in logs}
    for label, native_id in (("root", session_id), ("fork", fork_id)):
        if native_id in logs:
            assertions.update({label + "_" + key: value for key, value in retained_turn_assertions(logs[native_id], 1).items()})
    return {"assertions": assertions, "root_id": session_id, "fork_id": fork_id,
            "fork_error": forked.get("error"), "provider_requests": case.server.requests}


def probe_pty(case):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 120, 0, 0))
    process = subprocess.Popen([str(case.binary)] + case.argv("unused")[2:] +
                               ["--workspace", str(case.workspace)],
                               env=case.env, cwd=case.workspace, stdin=slave,
                               stdout=slave, stderr=slave, start_new_session=True)
    os.close(slave)
    received = bytearray()
    deadline = time.monotonic() + 45
    submitted, settled = False, False
    try:
        while time.monotonic() < deadline and process.poll() is None:
            if select.select([master], [], [], .2)[0]:
                try:
                    chunk = os.read(master, 65536)
                except OSError:
                    break
                received.extend(chunk)
                if b"\x1b[6n" in chunk:
                    os.write(master, b"\x1b[1;1R")
                if len(received) > 1024 * 1024:
                    break
                # Startup only: do not infer a settled turn from screen output.
                if b"Muse" in received and len(received) > 1000 and not submitted:
                    os.write(master, b"\x1b[200~" + PROMPT.encode() + b"\x1b[201~\r")
                    submitted = True
            if submitted:
                for path in case.logs():
                    try:
                        rows = logical_rows(read_limited(path))
                    except (ValueError, KeyError):
                        continue
                    runs = [row.get("payload", {}) for _, _, row in rows if row.get("payload_type") == "runtime.session" and row.get("payload", {}).get("kind") == "run"]
                    starts = {payload.get("run_id") for payload in runs if payload.get("event", {}).get("kind") == "started" and payload["event"].get("prompt") == PROMPT}
                    if any(payload.get("run_id") in starts and payload.get("event", {}).get("kind") == "assistant_message_committed" and payload["event"].get("text") == REPLY for payload in runs):
                        settled = True
                        break
                if settled:
                    break
        if process.poll() is None:
            with contextlib.suppress(OSError):
                os.write(master, b"\x03")
        (case.root / "pty-output.bin").write_bytes(received)
        assertions = {"startup_visible": b"Muse" in received, "synthetic_reply_retained": settled,
                      "one_root_log": len(case.logs()) == 1}
        if len(case.logs()) == 1:
            assertions.update(retained_turn_assertions(case.logs()[0], 1))
        return {"assertions": assertions,
                "output_bytes": len(received), "output_sha256": digest(received),
                "exit_before_interrupt": process.poll(),
                "started": b"Muse" in received, "provider_requests": case.server.requests,
                "turn_submitted": submitted, "reply_retained": settled,
                "scope": "one synthetic bracketed-paste turn and interrupt; not compaction/orderly-exit qualification"}
    finally:
        stop_process(process)
        os.close(master)


def native_run(binary, output, selected=None):
    if file_digest(binary) != PINNED_SHA256:
        raise ValueError("binary SHA-256 differs from pinned macOS x86_64 1.4.8 build")
    output.mkdir(mode=0o700)
    report = {"schema": 1, "binary_sha256": PINNED_SHA256,
              "runner_sha256": file_digest(pathlib.Path(__file__)),
              "integrity_encoding": "unresolved; native exporter is not an independent verifier",
              "cases": {}}
    try:
        for name, probe, timezone in (("exec-utc", probe_exec, "UTC"),
                                      ("exec-minus12", probe_exec, "Etc/GMT+12"),
                                      ("exec-plus14", probe_exec, "Pacific/Kiritimati"),
                                      ("exec-tool", probe_exec, "UTC"),
                                      ("msp", probe_msp, "UTC"), ("pty", probe_pty, "UTC")):
            if selected and name not in selected:
                continue
            case = Case(output / name, binary, timezone, tool=name == "exec-tool")
            try:
                report["cases"][name] = probe(case)
            except (KeyboardInterrupt, SystemExit) as error:
                report["cases"][name] = {"failed": type(error).__name__, "detail": str(error)}
                raise
            except Exception as error:
                report["cases"][name] = {"failed": type(error).__name__, "detail": str(error)}
                command = case.root / "fresh-command.json"
                if command.exists():
                    report["cases"][name]["fresh"] = json.loads(read_limited(command).decode())
            finally:
                case.close()
                report["cases"][name]["timezone"] = timezone
                report["cases"][name]["observed_utc"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
                try:
                    report["cases"][name]["sessions"] = [inspect_session(path) for path in case.logs()]
                    hooks = case.root / "hooks.jsonl"
                    report["cases"][name]["hooks"] = [json.loads(line) for line in read_limited(hooks, HOOK_LIMIT).decode().splitlines()] if hooks.exists() else []
                except Exception as error:
                    report["cases"][name].update(failed=type(error).__name__, detail=str(error))
                write_json(output / "report.json", report)
    finally:
        write_json(output / "report.json", report)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=pathlib.Path)
    parser.add_argument("--output", required=True, type=pathlib.Path,
                        help="new disposable directory; contains native logs, never commit it")
    parser.add_argument("--cases", nargs="+", choices=["exec-utc", "exec-minus12", "exec-plus14",
                                                      "exec-tool", "msp", "pty"])
    args = parser.parse_args()
    with termination_cleanup():
        report = native_run(args.binary.resolve(strict=True), args.output.absolute(), args.cases)
    print(args.output / "report.json")
    return int(any("failed" in value or any(not ok for ok in value.get("assertions", {}).values())
                   for value in report["cases"].values()))


if __name__ == "__main__":
    raise SystemExit(main())
