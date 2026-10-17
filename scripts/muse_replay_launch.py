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


def planned_change(plans, name):
    values = plans.get(name)
    if not isinstance(values, list) or len(values) != 1 or not isinstance(values[0], dict):
        raise ValueError("one actual planner change required")
    value = values[0]
    path = value.get("Path")
    if value.get("Delete", False) or not isinstance(path, str) or not pathlib.Path(path).is_absolute():
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
    if plans.get("launch_id") != control["launch_id"] or plans.get("replay") != control["replay"]:
        raise ValueError("generated runner control mismatch")
    directory = pathlib.Path(private_directory)
    if not directory.is_absolute() or ".." in directory.parts:
        raise ValueError("absolute private directory required")
    runner_path = directory / ("hook-runner-" + control["launch_id"])
    overlay_path = directory / "managed-hooks.json"
    if plans.get("runner_path") != str(runner_path):
        raise ValueError("runner path is not bound to this launch")
    own = planned_change(plans, "owned_install")
    installed = planned_change(plans, "install_with_foreign")
    removed = planned_change(plans, "owned_removal")
    if own.get("Existed") is not False or installed.get("Existed") is not True or removed.get("Existed") is not True:
        raise ValueError("actual install and exact-owner removal required")
    if len({item["Path"] for item in (own, installed, removed)}) != 1:
        raise ValueError("planner paths disagree")
    installed_after = decoded_bytes(installed["After"])
    if decoded_bytes(removed["Before"]) != installed_after:
        raise ValueError("removal is not bound to installed bytes")
    documents = [strict_object(decoded_bytes(item["After"])) for item in (own, installed, removed)]
    if any(type(doc.get("schema_version")) is not int or doc["schema_version"] != 1 for doc in documents):
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
    return {"launch_id": control["launch_id"], "directory": str(directory), "qualified": False, "flags": flags,
            "files": [(runner_path.name, runner, 0o700), (overlay_path.name, encoded, 0o600)],
            "original_settings_bytes": installed_after, "original_managed_bytes": foreign_bytes}


def materialize_private_files(plan, directory):
    """Create an absent private directory/files; no replacement or early cleanup.

    A caller must retain these files until its separately qualified owned-process
    supervisor joins all hook descendants. Root exit alone is insufficient.
    """
    directory = pathlib.Path(directory)
    if not directory.is_absolute() or ".." in directory.parts:
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
