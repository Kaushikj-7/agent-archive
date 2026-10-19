"""Offline replay preparation controls; these establish no native capability."""
import base64
import hashlib
import json
import os
import pathlib
import tempfile
import unittest

import muse_replay_launch as replay


def encoded(value):
    return json.dumps(value, separators=(",", ":")).encode()


def b64(value):
    return base64.b64encode(value).decode()


class ReplayLaunchTests(unittest.TestCase):
    def fixture(self, directory, marker=None):
        marker = {"run_id": "run-A"} if marker is None else marker
        control = {"launch_id": "launch-A", "replay": marker}
        command = "exact production command supplied by Hooks.Plan"
        owned = {"hooks": [{"type": "command", "command": command}]}
        foreign = {"hooks": [{"type": "command", "command": "relative/foreign 'λ'"}]}
        own = {"schema_version": 1, "hooks": {"SessionStart": [owned]}}
        removed = {"schema_version": 1, "hooks": {"SessionStart": [foreign]},
                   "managed_hooks_env_vars": ["FOREIGN_KEY"], "mcp_servers": {"foreign": {"command": "keep"}}}
        installed = dict(removed, hooks={"SessionStart": [foreign, owned]})
        path = "/private/synthetic/settings.json"
        change = lambda value, existed: {"Path": path, "Before": b64(encoded(installed)) if existed else "", "After": b64(encoded(value)), "Existed": existed, "Mode": 0o600, "Delete": False}
        removal = change(removed, True)
        removal["Before"] = b64(encoded(installed))
        plan = {"version": 1, "qualified": False, "preparation_only": True,
                "observed_settings": {"path": path, "present": True, "bytes": b64(encoded(installed)), "mode": 0o600, "regular": True},
                "observed_managed": {"path": "/private/synthetic/managed.json", "present": True, "bytes": "", "mode": 0o600, "regular": True},
                "owner_present": True, "launch_id": control["launch_id"], "replay": marker,
                "owned_install": [change(own, False)], "install_with_foreign": [change(installed, True)],
                "owned_removal": [removal], "command": command,
                "runner_path": str(directory / "hook-runner-launch-A"),
                "native_command": "'" + str(directory / "hook-runner-launch-A") + "'",
                "runner_bytes": b64(b"#!/bin/sh\n# Synthetic planner output, never native qualification\n")}
        managed = {"hooks": {"SessionStart": [foreign], "ForeignEvent": [foreign]},
                   "foreign": {"order": [3, 1, 2]}, "managed_hooks_env_vars": ["FOREIGN_KEY"]}
        plan["observed_managed"]["bytes"] = b64(encoded(managed))
        return encoded(plan), encoded(managed), encoded(control)

    def prepare(self, directory, marker=None):
        raw, managed, control = self.fixture(directory, marker)
        return replay.prepare_for_qualification(raw, managed, control,
            plan_sha256=hashlib.sha256(raw).hexdigest(), private_directory=directory)

    def test_preserves_foreign_handlers_and_environment(self):
        directory = pathlib.Path("/private/synthetic/launch-A")
        plan = self.prepare(directory)
        overlay = json.loads(plan["files"][1][1])
        self.assertEqual(overlay["managed_hooks_env_vars"], ["FOREIGN_KEY"])
        self.assertEqual(overlay["foreign"], {"order": [3, 1, 2]})
        self.assertEqual(overlay["hooks"]["SessionStart"][0], overlay["hooks"]["ForeignEvent"][0])
        self.assertEqual(len(overlay["hooks"]["SessionStart"]), 2)
        suppressed = json.loads(plan["flags"][3].removeprefix("hooks="))
        self.assertEqual(suppressed["SessionStart"], [overlay["hooks"]["SessionStart"][0]])
        self.assertFalse(plan["qualified"])
        self.assertNotIn("AGENT_ARCHIVE_REPLAY", str(plan["flags"]))

    def test_absent_owner_preserves_actual_original_bytes(self):
        directory = pathlib.Path("/private/synthetic/launch-A")
        raw, managed, control = self.fixture(directory)
        plan = json.loads(raw)
        observed = b'{ "schema_version":1, "hooks":{"ForeignEvent":[]}, "mcp_servers":{"keep":true} }\n'
        plan["observed_settings"]["bytes"] = b64(observed)
        plan["install_with_foreign"][0]["Before"] = b64(observed)
        plan["owned_removal"] = []
        plan["owner_present"] = False
        raw = encoded(plan)
        result = replay.prepare_for_qualification(raw, managed, control,
            plan_sha256=hashlib.sha256(raw).hexdigest(), private_directory=directory)
        self.assertEqual(result["original_settings_bytes"], observed)
        self.assertNotEqual(result["hypothetical_settings_bytes"], observed)
        self.assertEqual(json.loads(result["flags"][3].removeprefix("hooks=")), {"ForeignEvent": []})

    def test_generated_command_limit_counts_utf8_bytes(self):
        directory = pathlib.Path("/private/synthetic/launch-A")
        raw, managed, control = self.fixture(directory)
        plan = json.loads(raw)
        for command in ("x" * 4096, "λ" * 2048):
            plan["native_command"] = command
            accepted = encoded(plan)
            replay.prepare_for_qualification(accepted, managed, control,
                plan_sha256=hashlib.sha256(accepted).hexdigest(), private_directory=directory)
        for command in ("x" * 4097, "λ" * 2049):
            plan["native_command"] = command
            oversized = encoded(plan)
            with self.assertRaisesRegex(ValueError, "bounded actual generated command"):
                replay.prepare_for_qualification(oversized, managed, control,
                    plan_sha256=hashlib.sha256(oversized).hexdigest(), private_directory=directory)

    def test_control_is_closed_and_normalized(self):
        for raw in [b'{"launch_id":"x","launch_id":"y","replay":{}}',
                    b'{"launch_id":"x","replay":{"run_id":"a","run_id":"b"}}',
                    b'{"launch_id":"x","replay":{"Run_ID":"a"}}',
                    b'{"launch_id":"x","replay":null}',
                    b'{"launch_id":"x","replay":{"run_id":""}}',
                    b'{"launch_id":"x","replay":{"run_id":"raw text"}}',
                    b'{"launch_id":"x","replay":{},"extra":true}',
                    encoded({"launch_id": "x", "replay": {"run_id": "a" * 129}}),
                    encoded({"launch_id": "../escape", "replay": {}})]:
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                replay.replay_control(raw)
        self.assertEqual(replay.replay_control(b'{"launch_id":"x","replay":{}}')["replay"], {})
        self.assertEqual(replay.replay_control(encoded({"launch_id": "x", "replay": {"run_id": "a" * 128}}))["launch_id"], "x")

    def test_pinned_command_and_launch_paths(self):
        directory = pathlib.Path("/private/synthetic/launch-A")
        raw, managed, control = self.fixture(directory)
        with self.assertRaises(ValueError):
            replay.prepare_for_qualification(raw + b" ", managed, control,
                plan_sha256=hashlib.sha256(raw).hexdigest(), private_directory=directory)
        for mutate in [lambda p: p.update(launch_id="launch-B"),
                       lambda p: p.update(runner_path="/private/foreign/runner"),
                       lambda p: p.update(replay={}),
                       lambda p: p.update(version=2),
                       lambda p: p.update(qualified=True),
                       lambda p: p.update(unknown=True),
                       lambda p: p["owned_removal"][0].update(Before=b64(b"{}"))]:
            plan = json.loads(raw)
            mutate(plan)
            changed = encoded(plan)
            with self.assertRaises(ValueError):
                replay.prepare_for_qualification(changed, managed, control,
                    plan_sha256=hashlib.sha256(changed).hexdigest(), private_directory=directory)

    def test_exclusive_files_and_permissions(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = pathlib.Path(temporary).resolve() / "launch"
            plan = self.prepare(directory, {})
            files = replay.materialize_private_files(plan, directory)
            self.assertEqual(directory.stat().st_mode & 0o777, 0o700)
            self.assertEqual([path.stat().st_mode & 0o777 for path in files], [0o700, 0o600])
            snapshot = [path.read_bytes() for path in files]
            with self.assertRaises(FileExistsError):
                replay.materialize_private_files(plan, directory)
            self.assertEqual([path.read_bytes() for path in files], snapshot)
            with self.assertRaises(ValueError):
                replay.materialize_private_files(plan, directory.parent / "different")


class ShippingConsumerTests(unittest.TestCase):
    def test_request_limit_precedes_process(self):
        request = {"executable": "/private/nonexistent-planner", "control": {"launch_id": "a", "replay": {}}, "large": "x" * replay.CAP}
        with self.assertRaisesRegex(ValueError, "request cap"):
            replay.prepare_from_archive(request["executable"], request)

    @unittest.skipUnless(os.name == "posix", "POSIX bounded pipe consumer")
    def test_refuses_unbounded_and_timed_out_producer(self):
        with tempfile.TemporaryDirectory() as temporary:
            executable = pathlib.Path(temporary).resolve() / "archive producer"
            request = {"executable": str(executable), "control": {"launch_id": "a", "replay": {}}}
            # Overflow needs interpreter startup time; hang intentionally uses a short deadline.
            for body, reason, seconds in [('import sys\nsys.stdout.buffer.write(b"x"*65537)\n', "output cap", 2),
                                          ('import time\ntime.sleep(10)\n', "timed out", 0.2)]:
                executable.write_text('#!/usr/bin/python3 -I\n' + body)
                executable.chmod(0o700)
                with self.subTest(reason=reason), self.assertRaisesRegex(ValueError, reason):
                    replay.prepare_from_archive(str(executable), request, timeout=seconds)

    @unittest.skipUnless(os.environ.get("AGENT_ARCHIVE_REPLAY_TEST_BINARY"), "explicit built archive binary required")
    def test_actual_cli_consumed_without_test_exporter(self):
        executable = str(pathlib.Path(os.environ["AGENT_ARCHIVE_REPLAY_TEST_BINARY"]).resolve())
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary).resolve()
            settings = root / "settings λ's.json"
            managed = root / "foreign managed.json"
            home = str(root / "archive λ's $(not-executed)")
            original_managed = encoded({"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": str(root / "foreign managed handler"), "timeout": 3}]}]}, "foreign": {"keep": True}})
            original_settings = (json.dumps({"schema_version": 1, "hooks": {"ForeignEvent": []},
                "managed_hooks_path": str(managed), "managed_hooks_env_vars": ["FOREIGN_KEY"],
                "mcp_servers": {"sentinel": {"command": "preserve"}}}, indent=2) + "\n").encode()
            settings.write_bytes(original_settings)
            managed.write_bytes(original_managed)
            request = {"version": 1, "executable": executable, "data_home": home, "default_data_home": "",
                "ownership_locations": {home: home}, "private_directory": str(root / "launch-A"),
                "control": {"launch_id": "launch-A", "replay": {"run_id": "run-A"}},
                "settings": {"path": str(settings), "present": True, "bytes": b64(original_settings), "mode": 0o600, "regular": True},
                "managed": {"path": str(managed), "present": True, "bytes": b64(original_managed), "mode": 0o600, "regular": True}}
            absent = replay.prepare_from_archive(executable, request)
            self.assertFalse(absent["owner_present"])
            self.assertFalse(absent["qualified"])
            self.assertEqual(absent["original_settings_bytes"], original_settings)
            self.assertEqual(settings.read_bytes(), original_settings)
            self.assertEqual(managed.read_bytes(), original_managed)
            files = replay.materialize_private_files(absent, request["private_directory"])
            self.assertEqual([p.stat().st_mode & 0o777 for p in files], [0o700, 0o600])
            # Simulate a real installed private observation, rather than claiming
            # the planner itself installed this hypothetical After document.
            settings.write_bytes(absent["hypothetical_settings_bytes"])
            request["settings"]["bytes"] = b64(settings.read_bytes())
            request["private_directory"] = str(root / "launch-B")
            request["control"] = {"launch_id": "launch-B", "replay": {}}
            owned = replay.prepare_from_archive(executable, request)
            self.assertTrue(owned["owner_present"])
            self.assertIn(b"invalid replay marker", owned["files"][0][1])
            self.assertEqual(owned["original_settings_bytes"], settings.read_bytes())
            self.assertEqual(json.loads(owned["flags"][3].removeprefix("hooks=")), {"ForeignEvent": []})
            self.assertNotIn("AGENT_ARCHIVE_REPLAY", str(owned["flags"]))
            request["data_home"] = str(root / "other installation")
            request["ownership_locations"][request["data_home"]] = request["data_home"]
            changed = replay.prepare_from_archive(executable, request)
            self.assertFalse(changed["owner_present"])
            self.assertIn("SessionStart", json.loads(changed["flags"][3].removeprefix("hooks=")))
            self.assertEqual(managed.read_bytes(), original_managed)
            # Real CLI -> shipping Python: quote expansion is bounded before
            # success; accepted boundary paths are planned without filesystem IO.
            quote = lambda value: "'" + value.replace("'", "'\"'\"'") + "'"
            private = "/private" + "/a'" * 700
            self.assertLess(len(private.encode()), 4096)
            self.assertGreater(len(quote(private + "/hook-runner-launch-B").encode()), 4096)
            request["private_directory"] = private
            with self.assertRaisesRegex(ValueError, "archive replay-plan refused"):
                replay.prepare_from_archive(executable, request)
            for count in range(700, 0, -1):
                private = "/private" + "/a'" * count
                size = len(quote(private + "/hook-runner-launch-B").encode())
                if size <= 4096:
                    self.assertGreaterEqual(size, 4089)
                    request["private_directory"] = private
                    boundary = replay.prepare_from_archive(executable, request)
                    self.assertFalse(boundary["qualified"])
                    overlay = json.loads(boundary["files"][1][1])
                    generated = overlay["hooks"]["SessionStart"][-1]["hooks"][0]["command"]
                    self.assertEqual(generated, quote(private + "/hook-runner-launch-B"))
                    self.assertLessEqual(len(generated.encode()), 4096)
                    break
            else:
                self.fail("no accepted quoted-path boundary")


if __name__ == "__main__":
    unittest.main()
