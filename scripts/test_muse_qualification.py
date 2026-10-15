#!/usr/bin/env python3
"""Offline Q0 controls: no native binary, listener or personal home is required."""

import json
import pathlib
import signal
import sys
import tempfile
import unittest
from unittest import mock

import muse_corpus as corpus
import muse_qualification as probe


FIXTURES = pathlib.Path(__file__).resolve().parent.parent / "internal/agents/muse/testdata"


class QualificationTests(unittest.TestCase):
    def test_environment_constructs_file_only_credentials_and_private_roots(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            with mock.patch.dict("os.environ", {"META_API_KEY": "PERSONAL_SECRET", "HTTPS_PROXY": "https://outside.invalid"}):
                env = probe.isolated_env(root)
            self.assertEqual(env["TBH_CREDENTIAL_BACKEND"], "file")
            self.assertEqual(env["META_API_KEY"], "synthetic-loopback-only-not-a-credential")
            self.assertNotIn("HTTPS_PROXY", env)
            for key in ("HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME",
                        "XDG_STATE_HOME", "TMPDIR", "AGENT_ARCHIVE_HOME", "AWS_CONFIG_FILE",
                        "AWS_SHARED_CREDENTIALS_FILE"):
                self.assertEqual(pathlib.Path(env[key]).parent, root)
            self.assertNotIn("PERSONAL_SECRET", json.dumps(env))

    def test_completed_leader_still_cleans_owned_process_group(self):
        process = mock.Mock(pid=12345)
        with mock.patch.object(probe.os, "killpg") as kill:
            probe.stop_process(process)
        self.assertEqual(kill.call_args_list, [mock.call(12345, signal.SIGTERM), mock.call(12345, signal.SIGKILL)])
        self.assertEqual(process.wait.call_count, 2)

    def test_timeout_is_failed_evidence_and_process_is_reaped(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            env = probe.isolated_env(root)
            result = probe.run_process([sys.executable, "-c", "import time; print('synthetic',flush=True); time.sleep(30)"],
                                       env=env, cwd=root, timeout=.2)
            self.assertTrue(result["timeout"])
            self.assertNotEqual(result["exit"], 0)
            self.assertEqual(result["argv"][0], sys.executable)

    def test_export_exit_zero_does_not_hide_rejected_line(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            case = mock.Mock(root=root, binary=pathlib.Path("/synthetic/muse"), env={}, workspace=root)
            probe.write_json(root / "bad-export.json", {"diagnostics": {"unparseable_lines": 1},
                                                       "events": [{"kind": "unparseable_line"}]})
            with mock.patch.object(probe, "run_process", return_value={"exit": 0, "stderr": "", "timeout": False}):
                result = probe.export_diagnostics(case, root / "input", "bad")
            self.assertEqual(result["exit"], 0)
            self.assertEqual(result["unparseable_lines"], 1)
            self.assertEqual(result["unparseable_diagnostics"], [])

    def test_msp_keeps_completion_that_arrives_before_start_acknowledgement(self):
        msp = probe.MSP.__new__(probe.MSP)
        msp.messages, msp.completed, msp.index = probe.queue.Queue(), set(), 0
        msp.send = mock.Mock()
        msp.messages.put({"method": "turn/completed", "params": {"sessionId": "synthetic"}})
        msp.messages.put({"id": 1, "result": {}})
        msp.turn("synthetic")
        self.assertIn("synthetic", msp.completed)

    def test_exact_native_vectors_have_separate_hash_and_integrity_claim(self):
        manifest = json.loads((FIXTURES / "native-vectors.json").read_text())
        for vector in manifest["vectors"]:
            self.assertEqual(probe.file_digest(FIXTURES / vector["file"]), vector["sha256"])
            self.assertFalse(vector["independent_integrity_verified"])
            self.assertEqual(vector["native_export_unparseable_lines"], 0)

    def test_sanitization_keeps_all_positions_but_drops_context_and_unknown_bodies(self):
        records = [
            {"schema_version": 1, "payload_type": "runtime.session", "stream": {"kind": "session", "id": "s"},
             "payload": {"kind": "run", "run_id": "r", "event": {"kind": "model_input_trace_recorded",
                         "model_messages": "PRIVATE_CONTEXT_SENTINEL", "base_instructions": "PRIVATE_INSTRUCTIONS"}}},
            {"schema_version": 1, "payload_type": "unknown.authority", "payload": {"credential": "SECRET"}},
        ]
        native = json.dumps({"retained_frame": "session_permission_transaction", "children": [
            {"child_index": 0, "record_json": json.dumps(records[0])}]}).encode() + b"\n" + json.dumps(records[1]).encode() + b"\n"
        safe, omissions = corpus.sanitize(native, pathlib.Path("/synthetic/source"))
        self.assertEqual([json.loads(line)["pos"] for line in safe.splitlines()],
                         [{"line": 1, "child": 0}, {"line": 2, "child": None}])
        for sentinel in (b"PRIVATE_CONTEXT_SENTINEL", b"PRIVATE_INSTRUCTIONS", b"SECRET"):
            self.assertNotIn(sentinel, safe)
        self.assertEqual(len(omissions), 2)

    def test_checked_in_corpus_hashes_and_exact_expectations(self):
        manifests = sorted(FIXTURES.glob("*.manifest.json"))
        self.assertGreaterEqual(len(manifests), 2, "complete root and tool sessions are required")
        for manifest_path in manifests:
            with self.subTest(manifest=manifest_path.name):
                manifest = json.loads(manifest_path.read_text())
                data = manifest_path.with_name(manifest_path.name.replace(".manifest.json", ".jsonl")).read_bytes()
                self.assertEqual(probe.digest(data), manifest["sanitized_sha256"])
                self.assertEqual(probe.file_digest(corpus.ALLOWLIST), manifest["allowlist_sha256"])
                self.assertFalse(manifest["native_integrity_valid"])
                self.assertEqual(corpus.summarize(data), manifest["expected"])
                self.assertGreater(manifest["expected"]["logical_records"], 30)
                self.assertIn(len(manifest["expected"]["own_run_starts"]), (1, 2))
                self.assertEqual(len(manifest["expected"]["visible_messages"]), len(manifest["expected"]["own_run_starts"]))
                self.assertTrue(manifest["omitted_bodies"])
                positions = [json.loads(line)["pos"] for line in data.splitlines()]
                self.assertEqual(set(position["line"] for position in positions),
                                 set(range(1, manifest["native_physical_lines"] + 1)))
                if manifest_path.name == "root-tool-resume.manifest.json":
                    self.assertEqual(len(manifest["expected"]["tool_calls"]), 1)
                    self.assertEqual(len(manifest["expected"]["tool_results"]), 1)
                    self.assertEqual(manifest["expected"]["tool_calls"][0]["call_id"],
                                     manifest["expected"]["tool_results"][0]["tool_call_id"])
                    self.assertEqual(len(manifest["expected"]["model_completions"]), 3)


if __name__ == "__main__":
    unittest.main()
