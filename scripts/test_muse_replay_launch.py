"""Offline replay preparation controls; these establish no native capability."""
import base64
import hashlib
import json
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
        change = lambda value, existed: {"Path": path, "After": b64(encoded(value)), "Existed": existed}
        removal = change(removed, True)
        removal["Before"] = b64(encoded(installed))
        plan = {"launch_id": control["launch_id"], "replay": marker,
                "owned_install": [change(own, False)], "install_with_foreign": [change(installed, True)],
                "owned_removal": [removal], "command": command,
                "runner_path": str(directory / "hook-runner-launch-A"),
                "native_command": "'" + str(directory / "hook-runner-launch-A") + "'",
                "runner_bytes": b64(b"#!/bin/sh\n# Synthetic planner output, never native qualification\n")}
        managed = {"hooks": {"SessionStart": [foreign], "ForeignEvent": [foreign]},
                   "foreign": {"order": [3, 1, 2]}, "managed_hooks_env_vars": ["FOREIGN_KEY"]}
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


if __name__ == "__main__":
    unittest.main()
