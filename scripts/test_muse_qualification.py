#!/usr/bin/env python3
"""Offline Q0 controls: no native binary, listener or personal home is required."""

import contextlib
import io
import json
import os
import pathlib
import subprocess
import time
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

    def test_command_stream_flood_fails_before_capturing_unbounded_output(self):
        for descriptor in (1, 2):
            with self.subTest(descriptor=descriptor), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                source = ("import os,time; os.write(" + str(descriptor) +
                          ",b'x'*65536); time.sleep(30)")
                with mock.patch.object(probe, "OUTPUT_LIMIT", 32768):
                    with self.assertRaisesRegex(RuntimeError, "stream byte limit"):
                        probe.run_process([sys.executable, "-c", source], env=probe.isolated_env(root),
                                          cwd=root, timeout=3)

    def test_msp_stream_and_pending_message_limits_fail_and_reap(self):
        sources = {
            "line": "import os,time; os.write(1,b'x'*65536); time.sleep(30)",
            "stderr": "import os,time; os.write(2,b'x'*65536); time.sleep(30)",
            "queue": "import os,time; os.write(1,b'{}\\n'*10); time.sleep(30)",
        }
        for label, source in sources.items():
            with self.subTest(limit=label), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                case = mock.Mock(root=root, env=probe.isolated_env(root), workspace=root)
                case.argv.return_value = [sys.executable, "-c", source]
                with mock.patch.object(probe, "OUTPUT_LIMIT", 32768), mock.patch.object(probe, "MSP_LINE_LIMIT", 4096), mock.patch.object(probe, "MSP_QUEUE_LIMIT", 2):
                    msp = probe.MSP(case)
                    try:
                        deadline = time.monotonic() + 3
                        while msp.reader_error is None and time.monotonic() < deadline:
                            time.sleep(.01)
                        self.assertIsNotNone(msp.reader_error)
                        with self.assertRaisesRegex(RuntimeError, "limit"):
                            msp.receive(lambda _message: True)
                    finally:
                        with self.assertRaisesRegex(RuntimeError, "limit"):
                            msp.close()
                    self.assertIsNotNone(msp.process.poll())
                    self.assertFalse(msp.reader.is_alive())
                    self.assertFalse(msp.stderr_reader.is_alive())
                    self.assertLessEqual((root / "serve-stderr.txt").stat().st_size, 32768)

    def test_oversized_evidence_is_refused_before_decoding_or_corpus_output(self):
        path = mock.MagicMock()
        source = path.open.return_value.__enter__.return_value
        source.read.return_value = b"x" * 33
        with self.assertRaisesRegex(RuntimeError, "file byte limit"):
            probe.read_limited(path, 32)
        source.read.assert_called_once_with(33)
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            native, output = root / "native.jsonl", root / "safe.jsonl"
            with native.open("wb") as stream:
                stream.seek(1024)
                stream.write(b"x")
            argv = ["muse_corpus.py", "--native-session", str(native), "--source-root", str(root), "--output", str(output)]
            with mock.patch.object(probe, "FILE_LIMIT", 1024), mock.patch.object(sys, "argv", argv):
                with self.assertRaisesRegex(RuntimeError, "file byte limit"):
                    corpus.main()
                with self.assertRaisesRegex(RuntimeError, "file byte limit"):
                    probe.inspect_session(native)
                with self.assertRaisesRegex(RuntimeError, "file byte limit"):
                    probe.retained_turn_assertions(native, 1)
            self.assertFalse(output.exists())
            self.assertFalse(output.with_suffix(".manifest.json").exists())

    def test_oversized_hook_evidence_is_recorded_as_failed_case(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            case = mock.Mock(root=root)
            case.logs.return_value = []
            (root / "hooks.jsonl").write_bytes(b"x" * 33)
            with mock.patch.object(probe, "file_digest", return_value=probe.PINNED_SHA256), mock.patch.object(probe, "Case", return_value=case), mock.patch.object(probe, "probe_exec", return_value={"assertions": {"synthetic": True}}), mock.patch.object(probe, "HOOK_LIMIT", 32):
                report = probe.native_run(root / "binary", root / "output", ["exec-utc"])
            self.assertEqual(report["cases"]["exec-utc"]["failed"], "RuntimeError")
            self.assertIn("file byte limit", report["cases"]["exec-utc"]["detail"])
            self.assertEqual(json.loads((root / "output/report.json").read_text()), report)
            case.close.assert_called_once()

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

    def test_retained_turn_checks_reject_missing_reply_usage_and_wrong_identity(self):
        for name in ("msp-root", "msp-fork", "tui-root", "root-resume", "root-tool-resume"):
            with self.subTest(corpus=name), tempfile.TemporaryDirectory() as directory:
                rows = [json.loads(line)["record"] for line in (FIXTURES / (name + ".jsonl")).read_text().splitlines()]
                run_rows = [row for row in rows if row.get("payload_type") == "runtime.session" and row.get("payload", {}).get("kind") == "run"]
                session_id = run_rows[0]["stream"]["id"]
                root = pathlib.Path(directory) / session_id
                root.mkdir()
                path = root / "session.jsonl"
                def write():
                    path.write_text("".join(json.dumps(row) + "\n" for row in rows))
                artifact = pathlib.Path("/synthetic/muse-q0/exec-tool/workspace/artifact.txt") if name == "root-tool-resume" else None
                expected_runs = 2 if "resume" in name else 1
                write()
                self.assertTrue(all(probe.retained_turn_assertions(path, expected_runs, artifact).values()))
                reply = next(row for row in run_rows if row["payload"]["event"].get("kind") == "assistant_message_committed")
                reply["payload"]["event"]["text"] = "wrong reply"
                write()
                self.assertFalse(probe.retained_turn_assertions(path, expected_runs, artifact)["exact_own_replies"])
                usage = next(row for row in run_rows if row["payload"]["event"].get("kind") == "model_completed")
                usage["payload"]["event"]["usage"]["input_tokens"] = 0
                write()
                self.assertFalse(probe.retained_turn_assertions(path, expected_runs, artifact)["exact_own_usage"])
                run_rows[0]["stream"] = {"id": "another-session"}
                write()
                self.assertFalse(probe.retained_turn_assertions(path, expected_runs, artifact)["own_stream_identity"])

    def test_resume_command_without_new_retained_turn_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            rows = [json.loads(line)["record"] for line in (FIXTURES / "msp-root.jsonl").read_text().splitlines()]
            session_id = next(row["stream"]["id"] for row in rows if "stream" in row)
            root = pathlib.Path(directory)
            session = root / "2026/10/15" / session_id
            session.mkdir(parents=True)
            path = session / "session.jsonl"
            path.write_text("".join(json.dumps(row) + "\n" for row in rows))
            case = mock.Mock(root=root, workspace=root, server=mock.Mock(tool=False))
            case.execute.return_value = {"exit": 0, "timeout": False}
            case.logs.return_value = [path]
            exported = {"export_written": True, "unparseable_lines": 0}
            def export(_case, source, _name):
                return dict(exported, unparseable_lines=int(source.name == "permission-invalid.jsonl"))
            with mock.patch.object(probe, "export_diagnostics", side_effect=export), mock.patch.object(probe, "probe_mutations", return_value={}):
                result = probe.probe_exec(case)
            self.assertTrue(result["assertions"]["resume_success"])
            self.assertFalse(result["assertions"]["resume_has_true_open"])
            self.assertFalse(result["assertions"]["resumed_exact_own_starts"])

    def test_corpus_requires_own_synthetic_conversation_and_preserves_existing_manifest(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            native, output = root / "native.jsonl", root / "safe.jsonl"
            native.write_text(json.dumps({"payload_type": "unknown.authority", "payload": {"private": "omitted"}}) + "\n")
            argv = ["muse_corpus.py", "--native-session", str(native), "--source-root", str(root), "--output", str(output)]
            with mock.patch.object(sys, "argv", argv), contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as failure:
                    corpus.main()
            self.assertEqual(failure.exception.code, 2)
            self.assertFalse(output.exists())
            manifest = output.with_suffix(".manifest.json")
            manifest.write_text("existing evidence")
            with mock.patch.object(sys, "argv", argv), contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit):
                    corpus.main()
            self.assertEqual(manifest.read_text(), "existing evidence")
            self.assertFalse(output.exists())

    def test_sigterm_unwinds_process_group_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            pidfile = root / "child.pid"
            source = ("import os,pathlib,time; p=pathlib.Path(" + repr(str(pidfile)) + "); "
                      "q=p.with_suffix('.ready'); q.write_text(str(os.getpid())); os.replace(q,p); time.sleep(30)")
            program = ("import pathlib,sys; sys.path.insert(0," + repr(str(pathlib.Path(probe.__file__).parent)) + "); import muse_qualification as p\n"
                       "with p.termination_cleanup():\n"
                       " p.run_process([sys.executable,'-c'," + repr(source) + "],env={},cwd=pathlib.Path(" + repr(directory) + "),timeout=30)\n")
            runner = subprocess.Popen([sys.executable, "-B", "-c", program], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            child = None
            try:
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline and not pidfile.exists() and runner.poll() is None:
                    time.sleep(.02)
                self.assertTrue(pidfile.exists(), "child did not start")
                child = int(pidfile.read_text())
                runner.terminate()
                runner.communicate(timeout=10)
                self.assertEqual(runner.returncode, 128 + signal.SIGTERM)
                with self.assertRaises(ProcessLookupError):
                    os.kill(child, 0)
            finally:
                if runner.poll() is None:
                    runner.kill()
                runner.communicate(timeout=5)
                if child is not None:
                    try:
                        os.killpg(child, signal.SIGKILL)
                    except ProcessLookupError:
                        pass

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
