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
import ctypes
import fcntl
import hashlib
import http.server
import itertools
import json
import os
import pathlib
import pty
import queue
import re
import select
import shlex
import signal
import socket
import socketserver
import stat
import subprocess
import struct
import sys
import threading
import time
import termios
import uuid


# Historical corpus provenance stays pinned to 1.4.8; new runs use PINNED_BUILDS.
PINNED_SHA256 = "94ea8fd1c73664c22e5c6157d8706945b667b11bd1c7c51691811e79f17dfcf1"
PINNED_BUILDS = {
    "94ea8fd1c73664c22e5c6157d8706945b667b11bd1c7c51691811e79f17dfcf1": "1.4.8-R6256.1",
    "4c6ed04ccd5a453bd7b38799aeb9eb290b8635b32c95b5a35e6950a2f20446cb": "1.4.9-R6297.1",
}
MODEL = "archive-synthetic-model"
PROMPT = "Synthetic qualification prompt: hello λ.\nSecond synthetic line."
REPLY = "Synthetic qualification answer λ.\nSecond synthetic line."
OUTPUT_LIMIT = 8 * 1024 * 1024  # Per captured stream; exceeding it fails the case.
MSP_LINE_LIMIT = 1024 * 1024
MSP_QUEUE_LIMIT = 1024
FILE_LIMIT = 32 * 1024 * 1024  # Harness input/export cap, not a native format limit.
HOOK_LIMIT = 1024 * 1024
TOOL_SCENARIOS = ("exec-read-success", "exec-read-missing", "exec-read-two")
SCENARIO_CONNECTIONS = 4
SCENARIO_REQUESTS = 4
SCENARIO_BODY_LIMIT = 256 * 1024
SCENARIO_INPUT_LIMIT = 1024 * 1024
SCENARIO_HEADER_LIMIT = 8192
SCENARIO_HANDLER_SECONDS = 2
SCENARIO_SECONDS = 45
SCENARIO_GROUP_LIMIT = 16  # Detection threshold, not a kernel pre-fork limit.
SCENARIO_FILE_LIMIT = 1024
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



def permission_content_sha256(frame):
    """Recompute the pinned v1 byte recipe, not full frame grammar or authority.

    FILE_LIMIT is this harness's aggregate child-byte budget, not a native format
    limit. Schema, physical ordinal, child semantics and canonical outer JSON
    need separate validation even when this digest matches.
    """
    def integer(value, bits, label):
        if type(value) is not int or not 0 <= value < 1 << bits:
            raise ValueError("permission checksum requires unsigned " + str(bits) + "-bit " + label)
        return value.to_bytes(bits // 8, "big")

    if not isinstance(frame, dict):
        raise ValueError("permission checksum requires a frame object")
    expected = frame.get("content_sha256")
    if not isinstance(expected, str) or re.fullmatch(r"sha256:[0-9a-f]{64}", expected) is None:
        raise ValueError("permission checksum requires lowercase sha256 content_sha256")
    transaction = frame.get("transaction_id")
    if not isinstance(transaction, str) or re.fullmatch(
            r"[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}", transaction) is None:
        raise ValueError("permission checksum requires a hyphenated transaction UUID")
    children = frame.get("children")
    if not isinstance(children, list) or len(children) > 4096:
        raise ValueError("permission checksum requires a children array of at most 4096 entries")
    value = hashlib.sha256()
    value.update(b"muse.session_permission_transaction.v1\0")
    value.update(integer(frame.get("outer_log_ordinal"), 64, "outer_log_ordinal"))
    value.update(uuid.UUID(transaction).bytes)
    value.update(integer(len(children), 32, "child count"))
    total = 0
    for child in children:
        if not isinstance(child, dict):
            raise ValueError("permission checksum requires child objects")
        index = integer(child.get("child_index"), 32, "child_index")
        record = child.get("record_json")
        if not isinstance(record, str):
            raise ValueError("permission checksum requires record_json strings")
        if len(record) > FILE_LIMIT - total:
            raise ValueError("permission checksum exceeded harness child-byte limit")
        # Two bounded UTF-8 passes avoid allocating a complete encoded child or
        # transaction preimage. Lone surrogates are rejected by strict encoding.
        size = 0
        for offset in range(0, len(record), 65536):
            size += len(record[offset:offset + 65536].encode("utf-8"))
            if total + size > FILE_LIMIT:
                raise ValueError("permission checksum exceeded harness child-byte limit")
        total += size
        value.update(index)
        value.update(integer(size, 64, "record byte length"))
        for offset in range(0, len(record), 65536):
            value.update(record[offset:offset + 65536].encode("utf-8"))
    return "sha256:" + value.hexdigest()


def permission_checksums(data):
    """Report independent hashes of observed permission frames, without bodies."""
    if len(data) > FILE_LIMIT:
        raise ValueError("permission checksum exceeded harness file byte limit")
    checks = []
    for line, raw in enumerate(data.splitlines(), 1):
        frame = json.loads(raw)
        if frame.get("retained_frame") == "session_permission_transaction":
            computed = permission_content_sha256(frame)
            checks.append({"line": line, "outer_log_ordinal": frame["outer_log_ordinal"],
                           "children": len(frame["children"]),
                           "content_sha256": frame["content_sha256"],
                           "computed_sha256": computed,
                           "matches": computed == frame["content_sha256"]})
    return checks


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



class OwnedGroupOverflow(RuntimeError):
    """A bounded query observed more owned members than the scenario permits."""

    def __init__(self):
        super().__init__("owned process-group descendant limit")


def owned_group_members(group):
    """Read PID membership of this recorded group only; never argv or environment."""
    if sys.platform == "darwin":
        library = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
        # SDK libproc.h: uint32 type/typeinfo, void* buffer, int byte capacity.
        # Use proc_listpids (byte return), not the count-returning convenience API.
        listing = library.proc_listpids
        listing.argtypes = (ctypes.c_uint32, ctypes.c_uint32, ctypes.c_void_p, ctypes.c_int)
        listing.restype = ctypes.c_int
        values = (ctypes.c_int * (SCENARIO_GROUP_LIMIT + 1))()
        ctypes.set_errno(0)
        size = listing(2, group, values, ctypes.sizeof(values))  # PROC_PGRP_ONLY = 2.
        if size < 0 or ctypes.get_errno() or size % ctypes.sizeof(ctypes.c_int):
            raise RuntimeError("owned process-group inspection unavailable")
        if size >= ctypes.sizeof(values):
            raise OwnedGroupOverflow()
        members = set(values[:size // ctypes.sizeof(ctypes.c_int)])
        if any(value <= 0 for value in members):
            raise RuntimeError("owned process-group inspection unavailable")
        return members
    if sys.platform.startswith("linux"):
        members, entries, reads = set(), 0, 0
        deadline = time.monotonic() + .25
        # Bound even the global catalog traversal, not just matching members.
        with os.scandir("/proc") as catalog:
            for entry in catalog:
                entries += 1
                if entries > 4096 or reads >= 2048 or time.monotonic() >= deadline:
                    raise RuntimeError("owned process-group inspection unavailable")
                if not entry.name.isdecimal():
                    continue
                try:
                    reads += 1
                    raw = read_limited(pathlib.Path(entry.path) / "stat", 4096).decode()
                    fields = raw[raw.rfind(")") + 2:].split()
                    if int(fields[2]) == group:
                        members.add(int(entry.name))
                        if len(members) > SCENARIO_GROUP_LIMIT:
                            raise OwnedGroupOverflow()
                except (FileNotFoundError, ProcessLookupError):
                    continue  # An exited PID cannot be an active group member.
                except (OSError, ValueError, IndexError) as error:
                    raise RuntimeError("owned process-group inspection unavailable") from error
        return members
    raise RuntimeError("owned process-group inspection unavailable on this platform")


def wait_owned_group(process, deadline=None):
    """Verify bounded group cleanup after leader reaping; no foreign PID signals."""
    deadline = min(deadline, time.monotonic() + 3) if deadline is not None else time.monotonic() + 3
    while True:
        try:
            if not owned_group_members(process.pid):
                return
        except RuntimeError:
            pass  # Unknown/overflow is never absence; keep checking after KILL.
        if time.monotonic() >= deadline:
            raise RuntimeError("owned process-group cleanup incomplete")
        time.sleep(.02)


def stop_scenario_process(process, receipt):
    """Finish every cleanup phase, even when inspection or an earlier signal fails."""
    deadline = time.monotonic() + 8
    inspection_errors = receipt.setdefault("inspection_errors", [])

    def present(label):
        try:
            return bool(owned_group_members(process.pid))
        except OwnedGroupOverflow:
            return True  # Truncation proves nonempty, never absence.
        except RuntimeError:
            inspection_errors.append(label)
            # A positive signal-zero query proves this recorded group exists.
            # It cannot certify complete catalog inspection or final cleanup.
            try:
                os.killpg(process.pid, 0)
                return True
            except ProcessLookupError:
                return False
            except OSError as error:
                receipt[label + "_query_errno"] = error.errno
                return None

    def send(signum, label):
        exists = present(label)
        if exists is not True:
            receipt[label] = "not_needed" if exists is False else "presence_unknown"
            return
        try:
            os.killpg(process.pid, signum)
            receipt[label] = "sent"
        except OSError as error:
            receipt[label] = "signal_failed"
            receipt[label + "_errno"] = error.errno
            # Do not let a failed TERM skip the independently checked KILL.

    try:
        send(signal.SIGTERM, "term")
        try:
            process.wait(timeout=min(2, max(.001, deadline - time.monotonic())))
        except subprocess.TimeoutExpired:
            pass
    finally:
        try:
            send(signal.SIGKILL, "kill")
        finally:
            try:
                process.wait(timeout=max(.001, deadline - time.monotonic()))
                receipt["leader_reaped"] = True
            except subprocess.TimeoutExpired:
                receipt["leader_wait_failed"] = True
                raise
            finally:
                wait_owned_group(process, deadline)
                receipt["group_absent"] = True
    if inspection_errors:
        raise RuntimeError("owned process-group inspection unavailable during cleanup")


def run_process(argv, *, env, cwd, timeout=40, group_limit=False, abort=None, evidence=None):
    process = subprocess.Popen(argv, env=env, cwd=cwd, stdin=subprocess.DEVNULL,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               start_new_session=True)
    captured = {process.stdout: bytearray(), process.stderr: bytearray()}
    pending = list(captured)
    deadline = time.monotonic() + timeout
    timed_out, finished = False, False
    started = time.monotonic()
    cleanup = {"term": "unavailable", "kill": "unavailable", "leader_reaped": False, "group_absent": False}
    try:
        while pending or process.poll() is None:
            if abort and abort():
                raise RuntimeError("scenario provider refused")
            if group_limit:
                owned_group_members(process.pid)
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
        finished = True
    finally:
        try:
            if group_limit:
                stop_scenario_process(process, cleanup)
            else:
                stop_process(process)
        finally:
            process.stdout.close()
            process.stderr.close()
            if evidence is not None:
                for label, stream in (("stdout", process.stdout), ("stderr", process.stderr)):
                    (evidence / (label + ".bin")).write_bytes(captured[stream])
                write_json(evidence / "process.json", {"argv": argv, "pid": process.pid,
                    "exit": process.returncode, "timeout": timed_out, "finished": finished, "cleanup": cleanup,
                    "duration_seconds": time.monotonic() - started,
                    "stdout_sha256": digest(captured[process.stdout]), "stderr_sha256": digest(captured[process.stderr])})
    return {"argv": argv, "exit": process.returncode,
            "stdout": captured[process.stdout].decode(errors="replace"),
            "stderr": captured[process.stderr].decode(errors="replace"), "timeout": timed_out}


SCENARIO_REFUSAL_CODES = frozenset({
    "route", "request-shape", "input-shape", "tool-declaration", "transition",
    "association", "limit", "deadline", "io", "unclassified",
})


class ScenarioRefusal(RuntimeError):
    """Carry only a closed control-point code, never request or exception text."""

    def __init__(self, code):
        self.code = code if isinstance(code, str) and code in SCENARIO_REFUSAL_CODES else "unclassified"
        super().__init__("scenario refused")


class ToolScenario:
    """Closed synthetic provider transitions, independent of native status grammar."""

    def __init__(self, name, workspace):
        if name not in TOOL_SCENARIOS:
            raise ValueError("unknown tool scenario")
        self.name, self.workspace = name, workspace
        self.calls = 2 if name == "exec-read-two" else 1
        self.paths = [workspace / ("missing.txt" if name == "exec-read-missing" else "artifact.txt")]
        if self.calls == 2:
            self.paths.append(workspace / "artifact-two.txt")
            self.paths[1].write_text("SYNTHETIC_SECOND_RESULT\n")
        self.phase, self.result_order = "initial", False
        self.validate_files()

    def validate_files(self):
        for path in self.paths:
            if path.parent != self.workspace or path.is_symlink():
                raise ScenarioRefusal("input-shape")
            if self.name == "exec-read-missing":
                if path.exists():
                    raise ScenarioRefusal("input-shape")
            else:
                info = path.stat()
                if not stat.S_ISREG(info.st_mode) or info.st_size > SCENARIO_FILE_LIMIT:
                    raise ScenarioRefusal("limit")

    def call_ids(self):
        return ["call_control_" + str(index) for index in range(self.calls)]

    def arguments(self, index):
        return {"path": str(self.paths[index]), "offset": 1, "limit": 10}

    def respond(self, body):
        if not isinstance(body, dict) or body.get("model") != MODEL or body.get("stream") is not True:
            raise ScenarioRefusal("request-shape")
        inputs = body.get("input")
        if not isinstance(inputs, list):
            raise ScenarioRefusal("input-shape")
        outputs = [item for item in inputs if isinstance(item, dict) and item.get("type") == "function_call_output"]
        # Context is deliberately ignored. Only controlled call associations matter.
        echoed = [item for item in inputs if isinstance(item, dict) and item.get("type") == "function_call"]
        if echoed:
            ids = [item.get("call_id") for item in echoed]
            if self.phase != "results" or len(ids) != self.calls or set(ids) != set(self.call_ids()):
                raise ScenarioRefusal("association")
            for item in echoed:
                index = self.call_ids().index(item["call_id"])
                known_name = (item.get("name") == "read_file" or
                              (item.get("name") == "muse.read_file" and "namespace" not in item))
                if (not known_name or not isinstance(item.get("arguments"), str) or
                        json.loads(item["arguments"]) != self.arguments(index)):
                    raise ScenarioRefusal("association")
        if self.phase == "initial":
            if outputs:
                raise ScenarioRefusal("transition")
            tools = body.get("tools")
            if not isinstance(tools, list):
                raise ScenarioRefusal("tool-declaration")
            declared = [item for tool in tools if isinstance(tool, dict)
                        for item in tool.get("tools", [tool]) if isinstance(item, dict)]
            if not any(item.get("name") == "read_file" for item in declared):
                raise ScenarioRefusal("tool-declaration")
            self.validate_files()
            events = [{"type": "response.function_call_arguments.done", "output_index": index,
                       "item_id": "fc_control_" + str(index), "namespace": "muse", "name": "read_file",
                       "call_id": call_id, "arguments": json.dumps(self.arguments(index))}
                      for index, call_id in enumerate(self.call_ids())]
            self.phase = "results"
        elif self.phase == "results":
            ids = [item.get("call_id") for item in outputs]
            if (len(ids) != self.calls or any(not isinstance(value, str) for value in ids) or
                    len(set(ids)) != self.calls or set(ids) != set(self.call_ids()) or
                    any(set(item) != {"type", "call_id", "output"} or
                        not isinstance(item["output"], (str, list)) for item in outputs)):
                raise ScenarioRefusal("association")
            self.result_order = ids == self.call_ids()
            events = [{"type": "response.output_text.delta", "output_index": 0,
                       "item_id": "msg_control", "content_index": 0, "delta": REPLY}]
            self.phase = "complete"
        else:
            raise ScenarioRefusal("transition")
        index = 1 if self.phase == "results" else 2
        frame = {"id": "resp_control_" + str(index), "object": "response", "model": MODEL,
                 "status": "in_progress", "output": []}
        frames = [{"type": "response.created", "response": frame}] + events + [
            {"type": "response.completed", "response": dict(frame, status="completed", usage=USAGE)}]
        return b"".join(("data: " + json.dumps(dict(event, sequence_number=number)) + "\n\n").encode()
                        for number, event in enumerate(frames, 1))


class ScenarioEndpoint(socketserver.BaseRequestHandler):
    """One bounded HTTP request per owned connection, with an absolute deadline."""

    def handle(self):
        deadline = min(self.server.deadline, time.monotonic() + SCENARIO_HANDLER_SECONDS)
        def receive(size):
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise ScenarioRefusal("deadline")
            self.request.settimeout(remaining)
            chunk = self.request.recv(size)
            if not chunk:
                raise ScenarioRefusal("io")
            return chunk
        try:
            data = bytearray()
            while not data.endswith(b"\r\n\r\n"):
                if len(data) >= SCENARIO_HEADER_LIMIT:
                    raise ScenarioRefusal("limit")
                # Avoid overreading a body before its advertised size is checked.
                data.extend(receive(1))
            try:
                lines = bytes(data).decode("ascii").split("\r\n")
                method, path, protocol = lines[0].split(" ")
            except (UnicodeDecodeError, ValueError):
                raise ScenarioRefusal("request-shape") from None
            if protocol != "HTTP/1.1" or method not in ("GET", "POST"):
                raise ScenarioRefusal("request-shape")
            headers = {}
            for line in lines[1:-2]:
                try:
                    key, value = line.split(":", 1)
                except ValueError:
                    raise ScenarioRefusal("request-shape") from None
                key = key.lower()
                if key in headers:
                    raise ScenarioRefusal("request-shape")
                headers[key] = value.strip()
            if "transfer-encoding" in headers:
                raise ScenarioRefusal("request-shape")
            length_text = headers.get("content-length", "0")
            if not length_text.isdecimal() or len(length_text) > 9:
                raise ScenarioRefusal("request-shape")
            length = int(length_text)
            if length > SCENARIO_BODY_LIMIT:
                raise ScenarioRefusal("limit")
            with self.server.lock:
                if time.monotonic() >= self.server.deadline:
                    raise ScenarioRefusal("deadline")
                if self.server.error:
                    raise ScenarioRefusal("transition")
                if self.server.requests >= SCENARIO_REQUESTS:
                    raise ScenarioRefusal("limit")
                self.server.requests += 1
                if self.server.input_bytes + len(data) + length > SCENARIO_INPUT_LIMIT:
                    raise ScenarioRefusal("limit")
                self.server.input_bytes += len(data) + length
            body = bytearray()
            while len(body) < length:
                body.extend(receive(min(16384, length - len(body))))
            with self.server.lock:
                if method == "GET" and path in ("/models", "/muse-code/models") and length == 0:
                    if self.server.model_requests >= 2:
                        raise ScenarioRefusal("limit")
                    self.server.model_requests += 1
                    if path == "/models":
                        self.server.models_get_seen = True
                    else:
                        self.server.muse_code_models_get_seen = True
                    payload = json.dumps({"object": "list", "data": [{"id": MODEL, "object": "model",
                        "metadata": {"muse-code": {"release_date": "2026-01-01", "is_hidden": False,
                        "limit": {"context": 1000000, "output": 1024}}}}]}).encode()
                    content_type = b"application/json"
                elif method == "POST" and path == "/responses" and length > 0:
                    self.server.responses_post_seen = True
                    payload = self.server.scenario.respond(json.loads(body))
                    content_type = b"text/event-stream"
                else:
                    raise ScenarioRefusal("route")
            del body, data  # Do not retain provider context in a receipt or exception.
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise ScenarioRefusal("deadline")
            self.request.settimeout(remaining)
            self.request.sendall(b"HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Type: " + content_type +
                                 b"\r\nContent-Length: " + str(len(payload)).encode() + b"\r\n\r\n" + payload)
        except ScenarioRefusal as error:
            self.server.fail(error.code)
        except socket.timeout:
            self.server.fail("deadline")
        except OSError:
            self.server.fail("io")
        except (json.JSONDecodeError, UnicodeDecodeError):
            self.server.fail("input-shape")
        except Exception:
            self.server.fail("unclassified")


class ScenarioServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    """Finite owned sockets and non-daemon threads, all closed and joined."""

    allow_reuse_address = False
    daemon_threads = False
    block_on_close = True

    def __init__(self, scenario):
        self.scenario, self.lock = scenario, threading.Lock()
        self.connections = self.requests = self.input_bytes = self.model_requests = 0
        self.error, self.closed, self.joined = False, False, False
        self.first_refusal = "none"
        self.cleanup_failed = False
        self.models_get_seen = self.muse_code_models_get_seen = self.responses_post_seen = False
        self.stop = threading.Event()
        self.timeout = .05
        self.sockets, self.handlers = set(), []
        self.deadline = time.monotonic() + SCENARIO_SECONDS
        super().__init__(("127.0.0.1", 0), ScenarioEndpoint)

    def serve_forever(self, poll_interval=.05):
        while not self.stop.is_set():
            if time.monotonic() >= self.deadline:
                self.fail("deadline")
                break
            self.handle_request()

    def shutdown(self):
        self.stop.set()

    def _fail_locked(self, code):
        if self.first_refusal == "none":
            self.first_refusal = code if isinstance(code, str) and code in SCENARIO_REFUSAL_CODES else "unclassified"
        self.error = True
        self.stop.set()

    def fail(self, code="unclassified"):
        with self.lock:
            self._fail_locked(code)

    def process_request(self, request, client_address):
        with self.lock:
            if self.closed or self.error or self.connections >= SCENARIO_CONNECTIONS or time.monotonic() >= self.deadline:
                self._fail_locked("deadline" if time.monotonic() >= self.deadline else
                                  "limit" if self.connections >= SCENARIO_CONNECTIONS else "transition")
                self.shutdown_request(request)
                return
            self.connections += 1
            self.sockets.add(request)
            thread = threading.Thread(target=self.process_request_thread, args=(request, client_address))
            self.handlers.append(thread)
            thread.start()

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            with self.lock:
                self.sockets.discard(request)

    def close(self, thread):
        try:
            self.shutdown()
            with self.lock:
                self.closed = True
                sockets = list(self.sockets)
            for connection in sockets:
                with contextlib.suppress(OSError):
                    connection.shutdown(socket.SHUT_RDWR)
                connection.close()
            thread.join(timeout=3)
            try:
                self.server_close()
            finally:
                for handler in self.handlers:
                    handler.join(timeout=3)
            if thread.is_alive() or any(handler.is_alive() for handler in self.handlers) or self.sockets:
                raise RuntimeError("scenario endpoint cleanup incomplete")
            self.joined = True
        except BaseException:
            self.cleanup_failed = True
            raise

    def receipt(self):
        return {"requests": self.requests, "connections": self.connections,
                "first_refusal": self.first_refusal, "cleanup_failed": self.cleanup_failed,
                "models_get_seen": self.models_get_seen, "muse_code_models_get_seen": self.muse_code_models_get_seen,
                "responses_post_seen": self.responses_post_seen,
                "provider_complete": self.scenario.phase == "complete", "provider_refused": self.error,
                "provider_result_order": self.scenario.result_order,
                "handlers_joined": self.joined and not self.sockets and all(not t.is_alive() for t in self.handlers)}


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
    def __init__(self, root, binary, timezone="UTC", tool=False, scenario=None):
        root.mkdir()
        self.root, self.binary = root, binary
        self.env = isolated_env(root, timezone)
        self.workspace = root / "workspace"
        self.workspace.mkdir()
        (self.workspace / "artifact.txt").write_text("SYNTHETIC_TOOL_RESULT λ\n")
        self.scenario = ToolScenario(scenario, self.workspace) if scenario else None
        self.server = (ScenarioServer(self.scenario) if scenario else
                       http.server.ThreadingHTTPServer(("127.0.0.1", 0), Endpoint))
        try:
            self.server.daemon_threads = scenario is None
            self.server.requests = 0
            self.server.request_lock = threading.Lock()
            self.server.tool = tool
            self.server.artifact = self.workspace / "artifact.txt"
            self.thread = threading.Thread(target=self.server.serve_forever, daemon=scenario is None)
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
                                              str(self.server.server_address[1]), "auth": "bearer"}, "hooks": hooks})
            write_json(config / "auth.json", {"schema_version": 1, "providers": {
                "meta": {"api_key": "synthetic-loopback-only-not-a-credential"}}})
            self.thread.start()
        except BaseException:
            self.server.server_close()
            raise

    def argv(self, mode):
        return [str(self.binary), mode, "--provider", "meta", "--model", MODEL,
                "--disable-write", "--disable-shell", "--trust-workspace"]

    def execute(self, extra=()):
        return run_process(self.argv("exec") + ["--disable-reminders", "--no-foreign-personal-context",
                           "--max-model-steps", "3", "--workspace",
                           str(self.workspace)] + list(extra) + [PROMPT],
                           env=self.env, cwd=self.workspace,
                           **({"timeout": max(.001, self.server.deadline - time.monotonic()),
                               "group_limit": True, "abort": lambda: self.server.error,
                               "evidence": self.root} if self.scenario else {}))

    def logs(self):
        paths = (self.root / "data/muse/sessions").glob("*/*/*/*/session.jsonl")
        return sorted(itertools.islice(paths, 2) if self.scenario else paths)

    def close(self):
        if self.scenario:
            self.server.close(self.thread)
            return
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
            "permission_checksums": permission_checksums(data),
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


def scenario_tool_receipt(case, path):
    """Inspect controlled joins only; never infer typed status from result prose."""
    scenario = case.scenario
    if not path.resolve(strict=True).is_relative_to(case.root.resolve(strict=True)):
        raise RuntimeError("scenario native path refused")
    descriptor = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    with os.fdopen(descriptor, "rb") as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > FILE_LIMIT:
            raise RuntimeError("scenario native regular-file byte limit")
        data = source.read(FILE_LIMIT + 1)
        if len(data) > FILE_LIMIT:
            raise RuntimeError("scenario native file byte limit")
    rows = logical_rows(data)
    calls, results, owner, batch, call_position = [], [], None, None, None
    for position, (_, _, row) in enumerate(rows):
        if row.get("payload_type") != "runtime.session":
            continue
        payload = row.get("payload", {})
        event = payload.get("event", {})
        kind = event.get("kind")
        if kind not in ("assistant_tool_calls_committed", "tool_result_batch_committed"):
            continue
        if row.get("stream", {}).get("id") != path.parent.name or payload.get("kind") != "run":
            raise RuntimeError("scenario tool owner refused")
        if kind == "assistant_tool_calls_committed":
            if calls or results or not isinstance(payload.get("run_id"), str) or not payload["run_id"]:
                raise RuntimeError("scenario call transition refused")
            owner, batch, call_position = payload["run_id"], event.get("message_id"), position
            calls = event.get("tool_calls")
            if not isinstance(batch, str) or not batch or not isinstance(calls, list) or len(calls) != scenario.calls:
                raise RuntimeError("scenario call count refused")
            for index, call in enumerate(calls):
                if (not isinstance(call, dict) or set(call) != {"id", "call_id", "name", "args"} or
                        call["id"] != "fc_control_" + str(index) or call["call_id"] != scenario.call_ids()[index] or
                        call["name"] != "read_file" or not isinstance(call["args"], str) or
                        json.loads(call["args"]) != scenario.arguments(index)):
                    raise RuntimeError("scenario call identity or path refused")
        else:
            if (not calls or results or position <= call_position or payload.get("run_id") != owner or
                    event.get("batch_id") != batch):
                raise RuntimeError("scenario result transition refused")
            results = event.get("results")
            if not isinstance(results, list) or len(results) != scenario.calls:
                raise RuntimeError("scenario result count refused")
            seen = set()
            for result in results:
                if (not isinstance(result, dict) or set(result) != {"tool_call_index", "tool_call_id", "text"} or
                        type(result["tool_call_index"]) is not int or
                        not 0 <= result["tool_call_index"] < scenario.calls or
                        result["tool_call_index"] in seen or
                        result["tool_call_id"] != scenario.call_ids()[result["tool_call_index"]] or
                        not isinstance(result["text"], str)):
                    raise RuntimeError("scenario result index or identity refused")
                seen.add(result["tool_call_index"])
    if len(calls) != scenario.calls or len(results) != scenario.calls:
        raise RuntimeError("scenario missing controlled tool observations")
    success = "unavailable"
    if scenario.name != "exec-read-missing":
        expected = ["SYNTHETIC_TOOL_RESULT λ", "SYNTHETIC_SECOND_RESULT"]
        success = "matched" if all(result["text"] == "Read text file `" +
            str(scenario.paths[result["tool_call_index"]]) + "`.\n1|" + expected[result["tool_call_index"]]
            for result in results) else "mismatched"
    return {"success_output": success, "observed_shape": "call-result-batch", "calls": len(calls), "results": len(results),
            "call_ids_unique": True, "item_ids_unique": True, "result_indices_unique": True,
            "call_result_association": True, "batch_owner_join": True, "call_before_result": True,
            "result_order": [result["tool_call_index"] for result in results] == list(range(scenario.calls)),
            "typed_status": "unavailable", "cancellation": "unavailable", "parallel_execution": "unavailable"}


def probe_tool_scenario(case):
    """Run one opt-in finite read scenario; raw command evidence stays private."""
    command = case.execute()
    write_json(case.root / "fresh-command.json", command)
    paths = case.logs()
    receipt = {"assertions": {"command_success": command["exit"] == 0 and not command["timeout"],
                               "one_root_log": len(paths) == 1}}
    if len(paths) == 1:
        receipt["tools"] = scenario_tool_receipt(case, paths[0])
        receipt["assertions"]["controlled_success_output"] = receipt["tools"]["success_output"] != "mismatched"
        write_json(case.root / "private-attribution.json", {
            "native_sha256": file_digest(paths[0]), "command_sha256": file_digest(case.root / "fresh-command.json")})
    return receipt


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
        "fresh_permission_checksums_match": bool(before["permission_checksums"]) and
            all(check["matches"] for check in before["permission_checksums"]),
        "resumed_permission_checksums_match": bool(after["permission_checksums"]) and
            all(check["matches"] for check in after["permission_checksums"]),
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


def verify_version(binary, producer, root):
    """Require the pinned binary to report its exact build in a private environment."""
    root.mkdir()
    result = run_process([str(binary), "--version"], env=isolated_env(root), cwd=root,
                         timeout=15)
    expected = "Muse Code " + producer.split("-", 1)[0] + " (" + producer + ")\n"
    if (result["exit"] != 0 or result["timeout"] or result["stderr"] or
            result["stdout"] != expected):
        raise ValueError("pinned binary did not report its exact expected version")
    return result


def native_run(binary, output, selected=None):
    binary_sha256 = file_digest(binary)
    producer = PINNED_BUILDS.get(binary_sha256)
    if producer is None:
        raise ValueError("binary SHA-256 is not a pinned macOS x86_64 Muse build")
    output.mkdir(mode=0o700)
    report = {"schema": 1, "binary_sha256": binary_sha256, "producer": producer,
              "runner_sha256": file_digest(pathlib.Path(__file__)),
              "integrity_encoding": "pinned permission v1 SHA-256 recipe independently recomputed; "
                  "full frame grammar and original creation remain unqualified",
              "cases": {}}
    try:
        report["version_command"] = verify_version(binary, producer, output / "version")
        for name, probe, timezone in (("exec-utc", probe_exec, "UTC"),
                                      ("exec-minus12", probe_exec, "Etc/GMT+12"),
                                      ("exec-plus14", probe_exec, "Pacific/Kiritimati"),
                                      ("exec-tool", probe_exec, "UTC"),
                                      *((name, probe_tool_scenario, "UTC") for name in TOOL_SCENARIOS),
                                      ("msp", probe_msp, "UTC"), ("pty", probe_pty, "UTC")):
            if ((name in TOOL_SCENARIOS and (not selected or name not in selected)) or
                    (selected and name not in selected)):
                continue
            scenario = name in TOOL_SCENARIOS
            case = None
            try:
                case = Case(output / name, binary, timezone, tool=name == "exec-tool",
                            **({"scenario": name} if scenario else {}))
                report["cases"][name] = probe(case)
            except (KeyboardInterrupt, SystemExit) as error:
                report["cases"][name] = {"failed": ("Interrupted" if scenario else type(error).__name__), "detail": "scenario interrupted" if scenario else str(error)}
                raise
            except Exception as error:
                report["cases"][name] = {"failed": ("ScenarioRefused" if scenario else type(error).__name__), "detail": "scenario refused" if scenario else str(error)}
                command = case.root / "fresh-command.json" if case else None
                if command is not None and command.exists() and not scenario:
                    report["cases"][name]["fresh"] = json.loads(read_limited(command).decode())
            finally:
                try:
                    if case is not None:
                        case.close()
                except Exception as error:
                    report["cases"][name].update(failed="CleanupError" if scenario else type(error).__name__, detail="endpoint cleanup failed")
                if case is None:
                    write_json(output / "report.json", report)
                elif scenario:
                    receipt = case.server.receipt()
                    report["cases"][name]["endpoint"] = receipt
                    report["cases"][name].setdefault("assertions", {}).update(
                        provider_complete=receipt["provider_complete"], provider_accepted=not receipt["provider_refused"],
                        handlers_joined=receipt["handlers_joined"])
                    write_json(output / "report.json", report)
                else:
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
                                                      "exec-tool", "msp", "pty", *TOOL_SCENARIOS])
    args = parser.parse_args()
    with termination_cleanup():
        report = native_run(args.binary.resolve(strict=True), args.output.absolute(), args.cases)
    print(args.output / "report.json")
    return int(any("failed" in value or any(not ok for ok in value.get("assertions", {}).values())
                   for value in report["cases"].values()))


if __name__ == "__main__":
    raise SystemExit(main())
