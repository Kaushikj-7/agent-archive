"""Prepare private Muse replay files for the source-pinned qualification runner.

This module does not execute Muse or advertise a qualified replay capability.
Generated hook commands and owner removal must come from actual Hooks.Plan.
The caller owns the files until every root and hook descendant has joined.
"""
import base64
import copy
import hashlib
import json
import os
import pathlib
import re
import selectors
import subprocess
import time

CAP = 65536
IDENTITY = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}\Z")


def strict_object(raw):
    if not isinstance(raw, bytes) or len(raw) > CAP:
        raise ValueError("bounded JSON bytes required")

    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate JSON key")
            result[key] = value
        return result

    def nonfinite(_):
        raise ValueError("nonfinite JSON number")

    result = json.loads(raw, object_pairs_hook=unique, parse_constant=nonfinite)
    if not isinstance(result, dict):
        raise ValueError("JSON object required")
    return result


def replay_control(raw):
    """Decode normalized runner-owned control; never accept raw marker text."""
    control = strict_object(raw)
    if set(control) != {"launch_id", "replay"}:
        raise ValueError("closed launch control required")
    launch = control["launch_id"]
    replay = control["replay"]
    if not isinstance(launch, str) or not IDENTITY.fullmatch(launch):
        raise ValueError("bounded launch identity required")
    if not isinstance(replay, dict) or set(replay) - {"run_id"}:
        raise ValueError("normalized replay object required")
    if "run_id" in replay:
        run = replay["run_id"]
        if not isinstance(run, str) or not IDENTITY.fullmatch(run):
            raise ValueError("bounded replay identity required")
    return control


def decoded_bytes(value):
    if not isinstance(value, str) or len(value) > CAP * 2:
        raise ValueError("bounded encoded plan required")
    raw = base64.b64decode(value, validate=True)
    if len(raw) > CAP:
        raise ValueError("plan byte cap")
    return raw


def absolute_path(value):
    if not isinstance(value, str):
        return False
    try:
        raw = value.encode("utf-8")
    except UnicodeError:
        return False
    return (0 < len(raw) <= 4096 and not any(char in value for char in "\0\r\n")
            and not value.startswith("//") and pathlib.Path(value).is_absolute()
            and os.path.normpath(value) == value)


def planned_change(plans, name):
    values = plans.get(name)
    if not isinstance(values, list) or len(values) != 1 or not isinstance(values[0], dict):
        raise ValueError("one actual planner change required")
    value = values[0]
    if (set(value) != {"Path", "Before", "After", "Existed", "Mode", "Delete"}
            or type(value["Existed"]) is not bool or type(value["Delete"]) is not bool
            or type(value["Mode"]) is not int or not 0 <= value["Mode"] <= 0o777):
        raise ValueError("closed planner change required")
    decoded_bytes(value["Before"])
    decoded_bytes(value["After"])
    path = value.get("Path")
    if value.get("Delete", False) or not absolute_path(path):
        raise ValueError("absolute nondeleting planner change required")
    return value


def hooks(document):
    value = document.get("hooks", {})
    if not isinstance(value, dict) or len(value) > 32:
        raise ValueError("bounded hooks object required")
    if any(not isinstance(entries, list) or len(entries) > 64 for entries in value.values()):
        raise ValueError("bounded hook groups required")
    return value


def prepare_for_qualification(plan_bytes, foreign_bytes, control_bytes, *, plan_sha256, private_directory):
    """Return files/overrides from pinned generated commands; never launch them.

    The exact plan digest is supplied by the frozen outer qualification packet.
    Ordinary Hooks.Plan installation and exact-owner HookRemove establish which
    handlers may be suppressed. Foreign managed environment keys are untouched.
    """
    if not isinstance(plan_bytes, bytes) or hashlib.sha256(plan_bytes).hexdigest() != plan_sha256:
        raise ValueError("exact generated plan digest required")
    control = replay_control(control_bytes)
    plans = strict_object(plan_bytes)
    response_fields = {"version", "qualified", "preparation_only", "launch_id", "replay",
        "observed_settings", "observed_managed", "owner_present", "install_with_foreign",
        "owned_install", "owned_removal", "command", "runner_bytes", "runner_path", "native_command"}
    if (set(plans) != response_fields or type(plans.get("version")) is not int or plans["version"] != 1
            or plans["qualified"] is not False or plans["preparation_only"] is not True):
        raise ValueError("unsupported replay plan protocol")
    if plans.get("launch_id") != control["launch_id"] or plans.get("replay") != control["replay"]:
        raise ValueError("generated runner control mismatch")
    directory = pathlib.Path(private_directory)
    if not absolute_path(str(directory)):
        raise ValueError("absolute private directory required")
    runner_path = directory / ("hook-runner-" + control["launch_id"])
    overlay_path = directory / "managed-hooks.json"
    if plans.get("runner_path") != str(runner_path):
        raise ValueError("runner path is not bound to this launch")
    own = planned_change(plans, "owned_install")
    installed = planned_change(plans, "install_with_foreign")
    observed = plans["observed_settings"]
    observed_managed = plans["observed_managed"]
    for observation in (observed, observed_managed):
        if (not isinstance(observation, dict) or set(observation) != {"path", "present", "bytes", "mode", "regular"}
                or type(observation["present"]) is not bool or type(observation["regular"]) is not bool
                or type(observation["mode"]) is not int or not 0 <= observation["mode"] <= 0o777):
            raise ValueError("closed file observation required")
    original_settings = decoded_bytes(observed["bytes"])
    original_managed = decoded_bytes(observed_managed["bytes"])
    if original_managed != (foreign_bytes if observed_managed["present"] else b""):
        raise ValueError("foreign managed observation mismatch")
    if not observed_managed["present"] and foreign_bytes != b"{}":
        raise ValueError("absent managed source requires empty overlay base")
    if own.get("Existed") is not False or installed.get("Existed") is not observed["present"]:
        raise ValueError("hypothetical install observation mismatch")
    if installed["Path"] != observed["path"] or own["Path"] != observed["path"]:
        raise ValueError("planner paths disagree")
    if decoded_bytes(installed["Before"]) != original_settings:
        raise ValueError("hypothetical install is not bound to observed bytes")
    removals = plans["owned_removal"]
    if not isinstance(removals, list) or len(removals) > 1 or type(plans["owner_present"]) is not bool:
        raise ValueError("bounded actual removal required")
    if plans["owner_present"] != bool(removals):
        raise ValueError("owner presence and actual removal disagree")
    if removals:
        removed = planned_change(plans, "owned_removal")
        if (removed["Path"] != observed["path"] or removed.get("Existed") is not True
                or decoded_bytes(removed["Before"]) != original_settings):
            raise ValueError("removal is not bound to actual observation")
        suppressed = strict_object(decoded_bytes(removed["After"]))
    else:
        suppressed = strict_object(original_settings) if observed["present"] else {}
    installed_after = decoded_bytes(installed["After"])
    documents = [strict_object(decoded_bytes(own["After"])), strict_object(installed_after), suppressed]
    for document in documents[:2]:
        if type(document.get("schema_version")) is not int or document["schema_version"] != 1:
            raise ValueError("unsupported settings schema")
    command = plans.get("command")
    native_command = plans.get("native_command")
    for value in (command, native_command):
        if not isinstance(value, str) or not value or len(value) > 4096 or "\0" in value:
            raise ValueError("bounded actual generated command required")
    runner = decoded_bytes(plans.get("runner_bytes"))
    if not runner.startswith(b"#!/bin/sh\n") or b"\0" in runner:
        raise ValueError("generated direct runner required")
    owned_hooks = copy.deepcopy(hooks(documents[0]))
    if not owned_hooks:
        raise ValueError("no generated owned events")
    for groups in owned_hooks.values():
        for group in groups:
            handlers = group.get("hooks") if isinstance(group, dict) else None
            if not isinstance(handlers, list) or not handlers or len(handlers) > 64:
                raise ValueError("bounded owned handler group required")
            for handler in handlers:
                if not isinstance(handler, dict) or handler.get("type") != "command" or handler.get("command") != command:
                    raise ValueError("owned plan command mismatch")
                handler["command"] = native_command
    foreign = strict_object(foreign_bytes)
    hooks(foreign)
    overlay = copy.deepcopy(foreign)
    overlay_hooks = overlay.setdefault("hooks", {})
    for event, groups in owned_hooks.items():
        overlay_hooks.setdefault(event, []).extend(groups)
    encoded = json.dumps(overlay, ensure_ascii=False, separators=(",", ":")).encode()
    if len(encoded) > CAP:
        raise ValueError("overlay byte cap")
    flags = ["-c", "managed_hooks_path=" + json.dumps(str(overlay_path), ensure_ascii=False),
             "-c", "hooks=" + json.dumps(hooks(documents[2]), ensure_ascii=False, separators=(",", ":"))]
    return {"launch_id": control["launch_id"], "owner_present": plans["owner_present"], "directory": str(directory), "qualified": False, "flags": flags,
            "files": [(runner_path.name, runner, 0o700), (overlay_path.name, encoded, 0o600)],
            "original_settings_bytes": original_settings, "original_settings_present": observed["present"],
            "hypothetical_settings_bytes": installed_after, "original_managed_bytes": original_managed,
            "original_managed_present": observed_managed["present"]}


def materialize_private_files(plan, directory):
    """Create an absent private directory/files; no replacement or early cleanup.

    A caller must retain these files until its separately qualified owned-process
    supervisor joins all hook descendants. Root exit alone is insufficient.
    """
    directory = pathlib.Path(directory)
    if not absolute_path(str(directory)):
        raise ValueError("absolute private directory required")
    if str(directory) != plan.get("directory") or directory.parent.resolve() != directory.parent:
        raise ValueError("private plan directory mismatch or symlink parent")
    parent = directory.parent.stat()
    if parent.st_uid != os.getuid() or parent.st_mode & 0o077:
        raise ValueError("private parent must be owned and inaccessible to other users")
    files = plan["files"]
    names = [item[0] for item in files]
    if len(set(names)) != len(names) or any(pathlib.Path(name).name != name for name in names):
        raise ValueError("distinct direct-child filenames required")
    directory.mkdir(mode=0o700)
    directory.chmod(0o700)
    created = []
    try:
        for name, raw, mode in files:
            if mode not in (0o600, 0o700) or not isinstance(raw, bytes) or len(raw) > CAP:
                raise ValueError("bounded private file required")
            path = directory / name
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
            created.append(path)
            with os.fdopen(fd, "wb") as stream:
                os.fchmod(stream.fileno(), mode)
                stream.write(raw)
    except BaseException:
        for path in reversed(created):
            path.unlink()
        directory.rmdir()
        raise
    return tuple(created)


def prepare_from_archive(executable, request, *, timeout=10):
    """Consume the shipping pure CLI planner; no native agent or files are run.

    The caller supplies observed canonical ownership equality and rechecks those
    observations before any separately qualified native launch. This bounded
    child is the selected archive planner, not a native process supervisor.
    """
    if not absolute_path(executable):
        raise ValueError("absolute archive executable required")
    if not isinstance(request, dict) or request.get("executable") != executable:
        raise ValueError("explicit executable binding required")
    if not isinstance(timeout, (int, float)) or not 0 < timeout <= 30:
        raise ValueError("bounded planner timeout required")
    raw = json.dumps(request, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode()
    if len(raw) > CAP:
        raise ValueError("aggregate replay request cap")
    # Validate normalized control before starting even the pure planner.
    control = json.dumps(request.get("control"), separators=(",", ":")).encode()
    replay_control(control)
    argv = [executable, "eval", "replay-plan", "--harness", "muse"]
    env = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "PYTHONDONTWRITEBYTECODE": "1"}
    process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
    selector = selectors.DefaultSelector()
    output = bytearray()
    diagnostic = bytearray()
    sent = 0
    deadline = time.monotonic() + timeout
    try:
        for stream in (process.stdin, process.stdout, process.stderr):
            os.set_blocking(stream.fileno(), False)
        selector.register(process.stdin, selectors.EVENT_WRITE, "input")
        selector.register(process.stdout, selectors.EVENT_READ, "output")
        selector.register(process.stderr, selectors.EVENT_READ, "diagnostic")
        while selector.get_map():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise ValueError("archive planner timed out")
            for key, _ in selector.select(min(remaining, 0.1)):
                stream = key.fileobj
                if key.data == "input":
                    try:
                        sent += os.write(stream.fileno(), raw[sent:sent + 4096])
                    except BrokenPipeError:
                        sent = len(raw)
                    if sent == len(raw):
                        selector.unregister(stream)
                        stream.close()
                else:
                    chunk = os.read(stream.fileno(), 4096)
                    if not chunk:
                        selector.unregister(stream)
                        stream.close()
                        continue
                    if len(output) + len(diagnostic) + len(chunk) > CAP:
                        raise ValueError("aggregate archive planner output cap")
                    (output if key.data == "output" else diagnostic).extend(chunk)
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise ValueError("archive planner timed out")
        if process.wait(timeout=remaining) != 0 or diagnostic:
            raise ValueError("archive replay-plan refused")
    finally:
        selector.close()
        for stream in (process.stdin, process.stdout, process.stderr):
            stream.close()
        if process.poll() is None:
            process.kill()
        process.wait(timeout=2)
    plans = strict_object(bytes(output))
    if (plans.get("observed_settings") != request.get("settings")
            or plans.get("observed_managed") != request.get("managed")):
        raise ValueError("archive observation binding mismatch")
    managed = request["managed"]
    foreign = decoded_bytes(managed["bytes"]) if managed["present"] else b"{}"
    return prepare_for_qualification(bytes(output), foreign, control,
        plan_sha256=hashlib.sha256(output).hexdigest(), private_directory=request["private_directory"])
