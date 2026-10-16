#!/usr/bin/env python3
"""Offline Q0 controls: synthetic loopback only; no native binary or personal home."""

import contextlib
import copy
import io
import json
import os
import pathlib
import subprocess
import time
import signal
import socket
import threading
import sys
import tempfile
import unittest
from unittest import mock

import muse_corpus as corpus
import muse_qualification as probe


FIXTURES = pathlib.Path(__file__).resolve().parent.parent / "internal/agents/muse/testdata"


# Artificial encoding-only known answers, independently calculated outside the
# runner. These deliberately are not admissible native permission transactions.
CHECKSUM_VECTORS = [
    {"outer_log_ordinal": 1,
     "transaction_id": "00000000-0000-7000-8000-000000000001",
     "children": [{"child_index": 0, "record_json": "{}"}],
     "content_sha256": "sha256:8ed3fea8e26b6ce51f758a2afd846821a41858b9a56a1bebb90b6383728d7621"},
    {"outer_log_ordinal": 0x0102030405060708,
     "transaction_id": "12345678-9abc-7def-8123-456789abcdef",
     "children": [{"child_index": 0, "record_json": '{"text":"λ\\n雪"}'},
                  {"child_index": 1, "record_json": '{"x":1}'}],
     "content_sha256": "sha256:e553c63a5d076e2ff8ebb8747c1f04d8246630869c653ec63e0ac36e708f0ea0"},
]


class QualificationTests(unittest.TestCase):

    def test_permission_checksum_known_answers_pin_endian_uuid_and_utf8_bytes(self):
        for frame in CHECKSUM_VECTORS:
            with self.subTest(ordinal=frame["outer_log_ordinal"]):
                self.assertEqual(probe.permission_content_sha256(frame), frame["content_sha256"])

    def test_permission_checksum_binds_child_bytes_indices_order_count_and_identity(self):
        original = CHECKSUM_VECTORS[1]
        mutations = [dict(original, outer_log_ordinal=2),
                     dict(original, transaction_id=CHECKSUM_VECTORS[0]["transaction_id"]),
                     dict(original, children=list(reversed(original["children"]))),
                     dict(original, children=original["children"][:1])]
        for child_key, value in (("child_index", 7), ("record_json", '{"text":"λ雪"}')):
            frame = copy.deepcopy(original)
            frame["children"][0][child_key] = value
            mutations.append(frame)
        for frame in mutations:
            self.assertNotEqual(probe.permission_content_sha256(frame), original["content_sha256"])
        # Reformatting the child object changes its bytes even if JSON values match.
        frame = copy.deepcopy(original)
        frame["children"][1]["record_json"] = '{ "x": 1 }'
        self.assertNotEqual(probe.permission_content_sha256(frame), original["content_sha256"])

    def test_permission_checksum_matches_are_distinct_from_native_grammar(self):
        frame = dict(CHECKSUM_VECTORS[0], retained_frame="session_permission_transaction",
                     frame_schema_version=999, content_sha256="sha256:" + "0" * 64)
        # Hash fields do not bind the outer schema or discriminator; the helper
        # must not present a checksum match as complete native-frame acceptance.
        expected = CHECKSUM_VECTORS[0]["content_sha256"]
        self.assertEqual(probe.permission_content_sha256(frame), expected)
        result = probe.permission_checksums(json.dumps(frame).encode())
        self.assertEqual(result[0]["computed_sha256"], expected)
        self.assertFalse(result[0]["matches"])
        frame["content_sha256"] = expected
        self.assertTrue(probe.permission_checksums(json.dumps(frame).encode())[0]["matches"])
        self.assertEqual(probe.permission_checksums(b'{"payload_type":"ordinary"}'), [])

    def test_permission_checksum_rejects_bad_hash_input_types_ranges_and_utf8(self):
        mutations = [None, [], {key: value for key, value in CHECKSUM_VECTORS[0].items()
                               if key != "outer_log_ordinal"}]
        for key, values in {
                "outer_log_ordinal": (True, -1, 1 << 64, 1.5, "1"),
                "transaction_id": (None, 3, "not-a-uuid", "00000000000070008000000000000001"),
                "children": (None, {}, [None]),
                "content_sha256": (None, 0, "sha256:" + "A" * 64, "0" * 64),
        }.items():
            mutations.extend(dict(CHECKSUM_VECTORS[0], **{key: value}) for value in values)
        for key, values in {"child_index": (True, -1, 1 << 32, 0.5, "0"),
                            "record_json": (None, {}, b"{}", "\ud800")}.items():
            for value in values:
                frame = copy.deepcopy(CHECKSUM_VECTORS[0])
                frame["children"][0][key] = value
                mutations.append(frame)
        for index, frame in enumerate(mutations):
            with self.subTest(invalid=index), self.assertRaises(ValueError):
                probe.permission_content_sha256(frame)

    def test_permission_checksum_limits_count_and_aggregate_utf8_before_hashing(self):
        frame = copy.deepcopy(CHECKSUM_VECTORS[0])
        frame["children"] = frame["children"] * 4097
        with self.assertRaisesRegex(ValueError, "4096"):
            probe.permission_content_sha256(frame)
        # Three Unicode characters require six bytes; character counts are not
        # a substitute for the checked byte budget, including across children.
        frame["children"] = [{"child_index": 0, "record_json": "λλλ"}]
        with mock.patch.object(probe, "FILE_LIMIT", 5):
            with self.assertRaisesRegex(ValueError, "child-byte limit"):
                probe.permission_content_sha256(frame)
        frame["children"] = [{"child_index": 0, "record_json": "abc"},
                             {"child_index": 1, "record_json": "def"}]
        with mock.patch.object(probe, "FILE_LIMIT", 5):
            with self.assertRaisesRegex(ValueError, "child-byte limit"):
                probe.permission_content_sha256(frame)
            with self.assertRaisesRegex(ValueError, "file byte limit"):
                probe.permission_checksums(b" " * 6)

    def test_unknown_binary_refused_before_output_or_process_creation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            with mock.patch.object(probe, "file_digest", return_value="0" * 64), mock.patch.object(probe, "run_process") as run, mock.patch.object(probe, "Case") as case:
                with self.assertRaisesRegex(ValueError, "not a pinned"):
                    probe.native_run(root / "binary", root / "output", ["exec-utc"])
            self.assertFalse((root / "output").exists())
            run.assert_not_called()
            case.assert_not_called()

    def test_both_pins_record_the_actual_producer_and_runner_hash(self):
        for checksum, producer in probe.PINNED_BUILDS.items():
            with self.subTest(producer=producer), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                with mock.patch.object(probe, "file_digest", side_effect=[checksum, "runner-hash"]), mock.patch.object(probe, "verify_version", return_value={"exit": 0}) as version, mock.patch.object(probe, "Case") as case:
                    report = probe.native_run(root / "binary", root / "output", ["no-cases"])
                self.assertEqual(report["binary_sha256"], checksum)
                self.assertEqual(report["producer"], producer)
                self.assertEqual(report["runner_sha256"], "runner-hash")
                version.assert_called_once_with(root / "binary", producer, root / "output/version")
                case.assert_not_called()

    def test_version_command_requires_exact_build_and_success_in_isolation(self):
        for producer in probe.PINNED_BUILDS.values():
            good = {"exit": 0, "timeout": False, "stderr": "", "stdout":
                    "Muse Code " + producer.split("-", 1)[0] + " (" + producer + ")\n"}
            variants = [good, dict(good, exit=1), dict(good, timeout=True),
                        dict(good, stderr="warning"), dict(good, stdout="Muse Code unknown\n")]
            for index, result in enumerate(variants):
                with self.subTest(producer=producer, variant=index), tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory) / "version"
                    with mock.patch.object(probe, "run_process", return_value=result) as run:
                        if index == 0:
                            self.assertEqual(probe.verify_version(pathlib.Path("/synthetic/muse"), producer, root), result)
                        else:
                            with self.assertRaisesRegex(ValueError, "exact expected version"):
                                probe.verify_version(pathlib.Path("/synthetic/muse"), producer, root)
                    self.assertEqual(run.call_args.args[0], ["/synthetic/muse", "--version"])
                    self.assertEqual(run.call_args.kwargs["env"]["HOME"], str(root / "home"))
                    self.assertEqual(run.call_args.kwargs["cwd"], root)
                    self.assertEqual(run.call_args.kwargs["timeout"], 15)

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
            with mock.patch.object(probe, "file_digest", return_value=next(iter(probe.PINNED_BUILDS))), mock.patch.object(probe, "verify_version", return_value={}), mock.patch.object(probe, "Case", return_value=case), mock.patch.object(probe, "probe_exec", return_value={"assertions": {"synthetic": True}}), mock.patch.object(probe, "HOOK_LIMIT", 32):
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
            exported = {"exit": 0, "timeout": False, "export_written": True, "unparseable_lines": 0}
            def export(_case, source, _name):
                return dict(exported, unparseable_lines=int(source.name == "permission-invalid.jsonl"))
            with mock.patch.object(probe, "export_diagnostics", side_effect=export), mock.patch.object(probe, "probe_mutations", return_value={}):
                result = probe.probe_exec(case)
            self.assertTrue(result["assertions"]["resume_success"])
            self.assertFalse(result["assertions"]["resume_has_true_open"])
            self.assertFalse(result["assertions"]["resumed_exact_own_starts"])

    def test_export_failure_after_writing_document_cannot_pass_native_case(self):
        rows = [json.loads(line)["record"] for line in (FIXTURES / "root-resume.jsonl").read_text().splitlines()]
        session_id = next(row["stream"]["id"] for row in rows if "stream" in row)
        second_open = next(index for index, row in enumerate(rows)
                           if row.get("payload_type") == "session.opened.observed" and
                           row["payload"]["record"].get("resume"))
        assertions = {"valid": "native_valid_export_clean", "permission-exact": "native_exact_permission_clean",
                      "permission-invalid": "native_bad_checksum_diagnostic", "copy": "copy_native_export_clean"}
        for failed_export, assertion in assertions.items():
            for exit_code, timed_out in ((9, False), (0, True)):
                with self.subTest(export=failed_export, exit=exit_code, timeout=timed_out), tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory)
                    folder = root / session_id
                    folder.mkdir()
                    path = folder / "session.jsonl"
                    path.write_text((FIXTURES / "permission-exact.jsonl").read_text() +
                                    "".join(json.dumps(row) + "\n" for row in rows[:second_open]))
                    case = mock.Mock(root=root, workspace=root, server=mock.Mock(tool=False))
                    case.logs.return_value = [path]
                    def execute(*args):
                        if args:
                            path.write_text((FIXTURES / "permission-exact.jsonl").read_text() +
                                            "".join(json.dumps(row) + "\n" for row in rows))
                        return {"exit": 0, "timeout": False}
                    case.execute.side_effect = execute
                    def export(_case, _source, name):
                        return {"exit": exit_code if name == failed_export else 0,
                                "timeout": timed_out if name == failed_export else False,
                                "export_written": True,
                                "unparseable_lines": int(name == "permission-invalid")}
                    with mock.patch.object(probe, "export_diagnostics", side_effect=export), mock.patch.object(probe, "probe_mutations", return_value={"control": {"matched": True}}):
                        result = probe.probe_exec(case)
                    self.assertFalse(result["assertions"][assertion])
                    self.assertTrue(all(ok for name, ok in result["assertions"].items() if name != assertion))

    def test_exec_checksum_assertions_reject_stale_hash_despite_clean_native_export(self):
        rows = [json.loads(line)["record"] for line in (FIXTURES / "msp-root.jsonl").read_text().splitlines()]
        session_id = next(row["stream"]["id"] for row in rows if "stream" in row)
        frame = json.loads((FIXTURES / "permission-exact.jsonl").read_text())
        frame["children"][0]["record_json"] += " "
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            folder = root / session_id
            folder.mkdir()
            path = folder / "session.jsonl"
            path.write_text(json.dumps(frame) + "\n" + "".join(json.dumps(row) + "\n" for row in rows))
            case = mock.Mock(root=root, workspace=root, server=mock.Mock(tool=False))
            case.logs.return_value = [path]
            case.execute.return_value = {"exit": 0, "timeout": False}
            exported = {"exit": 0, "timeout": False, "export_written": True, "unparseable_lines": 0}
            with mock.patch.object(probe, "export_diagnostics", return_value=exported), mock.patch.object(probe, "probe_mutations", return_value={}):
                result = probe.probe_exec(case)
            self.assertTrue(result["assertions"]["native_valid_export_clean"])
            self.assertFalse(result["assertions"]["fresh_permission_checksums_match"])
            self.assertFalse(result["assertions"]["resumed_permission_checksums_match"])

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


class ToolScenarioTests(unittest.TestCase):
    @contextlib.contextmanager
    def endpoint(self, name="exec-read-two"):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "artifact.txt").write_text("SYNTHETIC_TOOL_RESULT λ\n")
            scenario = probe.ToolScenario(name, root)
            server = probe.ScenarioServer(scenario)
            thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": .01})
            thread.start()
            try:
                yield server
            finally:
                server.close(thread)
                self.assertFalse(thread.is_alive())
                self.assertTrue(server.receipt()["handlers_joined"])
                self.assertTrue(all(not handler.is_alive() for handler in server.handlers))

    def initial(self):
        return {"model": probe.MODEL, "stream": True, "tools": [{"type": "namespace", "tools": [
            {"type": "function", "name": "read_file"}]}], "input": [{"type": "message", "content": "PRIVATE_CONTEXT"}]}

    def results(self, count=2):
        return dict(self.initial(), input=[{"type": "function_call_output", "call_id": "call_control_" + str(index),
                                           "output": "PRIVATE_RESULT"} for index in range(count)])

    def exchange(self, server, body=None, raw=None):
        if raw is None:
            encoded = json.dumps(body).encode()
            raw = b"POST /responses HTTP/1.1\r\nContent-Length: " + str(len(encoded)).encode() + b"\r\n\r\n" + encoded
        with socket.create_connection(server.server_address, timeout=3) as connection:
            connection.sendall(raw)
            received = bytearray()
            while True:
                try:
                    chunk = connection.recv(4096)
                except ConnectionResetError:
                    break
                if not chunk:
                    break
                received.extend(chunk)
            return bytes(received)

    def frames(self, response):
        return [json.loads(line[6:]) for line in response.split(b"\r\n\r\n", 1)[1].splitlines()
                if line.startswith(b"data: ")]

    def test_exact_model_catalog_routes_share_budget_and_preserve_provider_turns(self):
        with self.endpoint() as server:
            catalogs = [self.exchange(server, raw=b"GET " + path + b" HTTP/1.1\r\nContent-Length: 0\r\n\r\n")
                        for path in (b"/models", b"/muse-code/models")]
            self.assertIn(b"200 OK", catalogs[0])
            self.assertEqual(catalogs[0], catalogs[1])
            self.assertEqual(server.model_requests, 2)
            first = self.frames(self.exchange(server, self.initial()))
            self.assertEqual(sum(frame["type"] == "response.function_call_arguments.done" for frame in first), 2)
            final = self.frames(self.exchange(server, self.results()))
            self.assertEqual(final[-1]["type"], "response.completed")
            receipt = server.receipt()
            self.assertEqual(receipt["requests"], 4)
            self.assertEqual(receipt["connections"], 4)
            self.assertEqual(receipt["first_refusal"], "none")
            self.assertTrue(receipt["models_get_seen"])
            self.assertTrue(receipt["muse_code_models_get_seen"])
            self.assertTrue(receipt["responses_post_seen"])
            self.assertTrue(receipt["provider_complete"])
        self.assertTrue(server.receipt()["handlers_joined"])

    def controlled_echo(self, frames, name):
        calls = [frame for frame in frames if frame["type"] == "response.function_call_arguments.done"]
        echoed = [{"type": "function_call", "call_id": call["call_id"], "name": name,
                   "arguments": call["arguments"]} for call in calls]
        body = self.results(len(calls))
        body["input"] = echoed + body["input"]
        return body

    def echo_turn(self, server, name):
        self.assertIn(b"200 OK", self.exchange(server, raw=b"GET /muse-code/models HTTP/1.1\r\n\r\n"))
        frames = self.frames(self.exchange(server, self.initial()))
        return self.controlled_echo(frames, name)

    def test_qualified_controlled_echo_without_namespace_completes_provider_turn(self):
        with self.endpoint("exec-read-success") as server:
            body = self.echo_turn(server, "muse.read_file")
            self.assertEqual(len(body["input"]), 2)
            response = self.exchange(server, body)
            self.assertIn(b"200 OK", response, server.receipt()["first_refusal"])
            self.assertEqual(self.frames(response)[-1]["type"], "response.completed")
            receipt = server.receipt()
            self.assertTrue(receipt["provider_complete"])
            self.assertEqual(receipt["first_refusal"], "none")
            self.assertEqual(receipt["requests"], 3)
            self.assertTrue(receipt["muse_code_models_get_seen"])
            self.assertNotIn("muse.read_file", json.dumps(receipt))
        self.assertTrue(server.receipt()["handlers_joined"])

    def test_legacy_controlled_echo_behavior_remains_supported(self):
        for namespace in ("absent", "muse", None):
            with self.subTest(namespace=namespace), self.endpoint("exec-read-success") as server:
                body = self.echo_turn(server, "read_file")
                if namespace != "absent":
                    body["input"][0]["namespace"] = namespace
                response = self.exchange(server, body)
                self.assertIn(b"200 OK", response, server.receipt()["first_refusal"])
                self.assertTrue(server.receipt()["provider_complete"])
                self.assertEqual(server.receipt()["first_refusal"], "none")

    def test_two_qualified_echoes_preserve_distinct_controlled_associations(self):
        with self.endpoint() as server:
            body = self.echo_turn(server, "muse.read_file")
            self.assertEqual(len(body["input"]), 4)
            response = self.exchange(server, body)
            self.assertIn(b"200 OK", response, server.receipt()["first_refusal"])
            self.assertTrue(server.receipt()["provider_complete"])
            self.assertFalse(server.receipt()["provider_refused"])

    def test_qualified_echo_names_namespaces_and_associations_remain_closed(self):
        variants = ("namespace-null", "namespace-muse", "namespace-conflict", "namespace-empty",
                    "prefix", "suffix", "other-tool", "case", "arguments", "path", "id", "count", "duplicate",
                    "early", "output-id", "output-extra", "output-type")
        for mutation in variants:
            with self.subTest(mutation=mutation), self.endpoint() as server:
                if mutation == "early":
                    frames = [{"type": "response.function_call_arguments.done", "call_id": call_id,
                               "arguments": json.dumps(server.scenario.arguments(index))}
                              for index, call_id in enumerate(server.scenario.call_ids())]
                    body = self.controlled_echo(frames, "muse.read_file")
                else:
                    body = self.echo_turn(server, "muse.read_file")
                echo = body["input"][0]
                if mutation.startswith("namespace-"):
                    echo["namespace"] = {"namespace-null": None, "namespace-muse": "muse",
                        "namespace-conflict": "PRIVATE_NAMESPACE", "namespace-empty": ""}[mutation]
                elif mutation in ("prefix", "suffix", "other-tool", "case"):
                    echo["name"] = {"prefix": "PRIVATE.muse.read_file", "suffix": "muse.read_file.PRIVATE",
                        "other-tool": "muse.PRIVATE", "case": "Muse.read_file"}[mutation]
                elif mutation == "arguments":
                    echo["arguments"] = json.dumps(dict(server.scenario.arguments(0), offset=2))
                elif mutation == "path":
                    echo["arguments"] = json.dumps(dict(server.scenario.arguments(0), path="/PRIVATE_FOREIGN"))
                elif mutation == "id":
                    echo["call_id"] = "PRIVATE_FOREIGN"
                elif mutation == "count":
                    del body["input"][1]
                elif mutation == "duplicate":
                    body["input"][1]["call_id"] = echo["call_id"]
                elif mutation == "output-id":
                    body["input"][2]["call_id"] = "PRIVATE_OUTPUT_ID"
                elif mutation == "output-extra":
                    body["input"][2]["PRIVATE_KEY"] = "PRIVATE_VALUE"
                elif mutation == "output-type":
                    body["input"][2]["output"] = 7
                self.assertEqual(self.exchange(server, body), b"")
                receipt = server.receipt()
                self.assertEqual(receipt["first_refusal"], "association")
                self.assertFalse(receipt["provider_complete"])
                self.assertNotIn("PRIVATE", json.dumps(receipt))
            self.assertTrue(server.receipt()["handlers_joined"])

    def test_model_catalog_aliases_share_the_existing_two_request_limit(self):
        with self.endpoint() as server:
            for path in (b"/models", b"/muse-code/models"):
                self.assertIn(b"200 OK", self.exchange(server, raw=b"GET " + path + b" HTTP/1.1\r\n\r\n"))
            self.assertEqual(self.exchange(server, raw=b"GET /muse-code/models HTTP/1.1\r\n\r\n"), b"")
            self.assertEqual(server.model_requests, 2)
            self.assertEqual(server.receipt()["first_refusal"], "limit")

    def test_unqualified_catalog_targets_methods_and_bodies_refuse_without_content(self):
        variants = [(b"GET", target, b"", "route") for target in (
            b"/PRIVATE_TARGET", b"/models?PRIVATE_QUERY", b"/muse-code/models?PRIVATE_QUERY",
            b"/muse-code/models#PRIVATE_FRAGMENT", b"/muse-code/models/", b"/v1/models",
            b"http://PRIVATE_AUTHORITY/muse-code/models", b"/muse-code/responses")]
        variants += [(b"GET", path, b"PRIVATE_BODY", "route") for path in (b"/models", b"/muse-code/models")]
        variants += [(b"POST", b"/muse-code/models", b"{}", "route"),
                     (b"POST", b"/muse-code/responses", b"{}", "route"),
                     (b"DELETE", b"/muse-code/models", b"", "request-shape")]
        for method, path, body, reason in variants:
            with self.subTest(method=method, path=path), self.endpoint() as server:
                raw = method + b" " + path + b" HTTP/1.1\r\nX-PRIVATE_HEADER: PRIVATE_VALUE\r\nContent-Length: " + str(len(body)).encode() + b"\r\n\r\n" + body
                self.assertEqual(self.exchange(server, raw=raw), b"")
                receipt = server.receipt()
                self.assertEqual(receipt["first_refusal"], reason)
                self.assertFalse(receipt["models_get_seen"])
                self.assertFalse(receipt["muse_code_models_get_seen"])
                self.assertFalse(receipt["responses_post_seen"])
                self.assertNotIn("PRIVATE", json.dumps(receipt))
            self.assertTrue(server.receipt()["handlers_joined"])

    def test_partial_request_io_and_malformed_json_keep_only_fixed_codes(self):
        with self.endpoint() as server:
            with socket.create_connection(server.server_address, timeout=2) as connection:
                connection.sendall(b"POST /responses HTTP/1.1\r\nPRIVATE_PARTIAL")
                connection.shutdown(socket.SHUT_WR)
                self.assertEqual(connection.recv(1024), b"")
            self.assertEqual(server.receipt()["first_refusal"], "io")
            self.assertNotIn("PRIVATE", json.dumps(server.receipt()))
        with self.endpoint() as server:
            body = b'{"PRIVATE_UNKNOWN_KEY": PRIVATE_MALFORMED_BODY}'
            raw = b"POST /responses HTTP/1.1\r\nContent-Length: " + str(len(body)).encode() + b"\r\n\r\n" + body
            self.assertEqual(self.exchange(server, raw=raw), b"")
            self.assertEqual(server.receipt()["first_refusal"], "input-shape")
            self.assertNotIn("PRIVATE", json.dumps(server.receipt()))

    def test_first_refusal_is_closed_and_survives_later_failures(self):
        with self.endpoint() as server:
            self.assertEqual(self.exchange(server, raw=b"GET /PRIVATE_TARGET HTTP/1.1\r\n\r\n"), b"")
            server.fail("limit")
            server.fail("PRIVATE_EXCEPTION_TEXT")
            self.assertEqual(server.receipt()["first_refusal"], "route")
            self.assertNotIn("PRIVATE", json.dumps(server.receipt()))
        for code in ("PRIVATE_UNKNOWN_CODE", ["PRIVATE_UNKNOWN_CODE"]):
            with self.subTest(code=code), self.endpoint() as server:
                server.fail(code)
                self.assertEqual(server.receipt()["first_refusal"], "unclassified")
                self.assertNotIn("PRIVATE", json.dumps(server.receipt()))

    def test_internal_control_points_report_fixed_codes_and_unknown_errors_are_unclassified(self):
        for mutation, expected in (("request", "request-shape"), ("input", "input-shape"),
                                   ("tool", "tool-declaration"), ("transition", "transition"),
                                   ("association", "association"), ("exception", "unclassified")):
            with self.subTest(mutation=mutation), self.endpoint() as server:
                body = self.initial()
                if mutation == "request":
                    body["model"] = "PRIVATE_MODEL"
                elif mutation == "input":
                    body["input"] = "PRIVATE_INPUT"
                elif mutation == "tool":
                    body["tools"] = [{"name": "PRIVATE_TOOL"}]
                elif mutation == "transition":
                    body = self.results()
                elif mutation == "association":
                    self.exchange(server, body)
                    body = self.results()
                    body["input"][0]["call_id"] = "PRIVATE_ID"
                else:
                    server.scenario.respond = mock.Mock(side_effect=type("PRIVATE_EXCEPTION_NAME", (ValueError,), {})("PRIVATE_EXCEPTION_TEXT"))
                self.assertEqual(self.exchange(server, body), b"")
                self.assertEqual(server.receipt()["first_refusal"], expected)
                self.assertNotIn("PRIVATE", json.dumps(server.receipt()))

    def test_cleanup_failure_remains_separate_from_first_refusal(self):
        with self.assertRaisesRegex(RuntimeError, "PRIVATE_CLEANUP"):
            with self.endpoint() as server:
                self.exchange(server, raw=b"GET /PRIVATE_TARGET HTTP/1.1\r\n\r\n")
                original = server.server_close
                def close_then_fail():
                    original()
                    raise RuntimeError("PRIVATE_CLEANUP")
                server.server_close = close_then_fail
        receipt = server.receipt()
        self.assertEqual(receipt["first_refusal"], "route")
        self.assertTrue(receipt["cleanup_failed"])
        self.assertFalse(receipt["handlers_joined"])
        self.assertTrue(all(not handler.is_alive() for handler in server.handlers))
        self.assertFalse(server.sockets)
        self.assertNotIn("PRIVATE", json.dumps(receipt))

    def test_real_endpoint_emits_independent_items_and_closes_each_scenario(self):
        for name in probe.TOOL_SCENARIOS:
            with self.subTest(scenario=name), self.endpoint(name) as server:
                frames = self.frames(self.exchange(server, self.initial()))
                calls = [frame for frame in frames if frame["type"] == "response.function_call_arguments.done"]
                count = 2 if name == "exec-read-two" else 1
                self.assertEqual(len(calls), count)
                self.assertEqual([call["output_index"] for call in calls], list(range(count)))
                self.assertEqual(len({call["item_id"] for call in calls}), count)
                self.assertEqual(len({call["call_id"] for call in calls}), count)
                for call in calls:
                    args = json.loads(call["arguments"])
                    self.assertEqual(args["limit"], 10)
                    self.assertEqual(pathlib.Path(args["path"]).exists(), name != "exec-read-missing")
                final = self.frames(self.exchange(server, self.results(count)))
                self.assertEqual(final[-1]["type"], "response.completed")
                self.assertTrue(server.receipt()["provider_complete"])
                self.assertFalse(server.error)
                self.assertNotIn("PRIVATE", json.dumps(server.receipt()))

    def test_result_reordering_preserves_association_without_claiming_parallel(self):
        with self.endpoint() as server:
            self.exchange(server, self.initial())
            body = self.results()
            body["input"].reverse()
            self.assertIn(b"200 OK", self.exchange(server, body))
            self.assertTrue(server.receipt()["provider_complete"])
            self.assertFalse(server.receipt()["provider_result_order"])

    def test_wrong_result_ids_counts_and_transitions_abort_without_retry(self):
        for mutation in ("id", "duplicate", "count", "type", "extra", "early", "late"):
            with self.subTest(mutation=mutation), self.endpoint() as server:
                if mutation != "early":
                    self.exchange(server, self.initial())
                body = self.results()
                if mutation == "id":
                    body["input"][0]["call_id"] = "foreign"
                elif mutation == "duplicate":
                    body["input"][1]["call_id"] = body["input"][0]["call_id"]
                elif mutation == "count":
                    body["input"].pop()
                elif mutation == "type":
                    body["input"][0]["output"] = 7
                elif mutation == "extra":
                    body["input"][0]["arbitrary"] = "PRIVATE"
                elif mutation == "late":
                    self.exchange(server, body)
                self.assertEqual(self.exchange(server, body), b"")
                self.assertTrue(server.error)
                self.assertTrue(server.stop.is_set())

    def test_malformed_routes_headers_lengths_and_body_are_refused(self):
        requests = [b"POST /unexpected HTTP/1.1\r\nContent-Length: 2\r\n\r\n{}",
                    b"POST /responses HTTP/1.1\r\nContent-Length: -1\r\n\r\n",
                    b"POST /responses HTTP/1.1\r\nContent-Length: 2\r\nContent-Length: 2\r\n\r\n{}",
                    b"POST /responses HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n",
                    b"POST /responses HTTP/1.1\r\nContent-Length: 1\r\n\r\n{",
                    b"POST /responses HTTP/1.1\r\nContent-Length: " + str(probe.SCENARIO_BODY_LIMIT + 1).encode() + b"\r\n\r\n",
                    b"X" * probe.SCENARIO_HEADER_LIMIT]
        for raw in requests:
            with self.subTest(request=raw[:35]), self.endpoint() as server:
                self.assertEqual(self.exchange(server, raw=raw), b"")
                self.assertTrue(server.error)

    def test_request_connection_and_aggregate_budgets_fail_closed(self):
        for limit in ("SCENARIO_REQUESTS", "SCENARIO_CONNECTIONS", "SCENARIO_INPUT_LIMIT"):
            with self.subTest(limit=limit), mock.patch.object(probe, limit, 1), self.endpoint() as server:
                if limit != "SCENARIO_INPUT_LIMIT":
                    self.assertIn(b"200 OK", self.exchange(server, self.initial()))
                    self.assertEqual(self.exchange(server, self.results()), b"")
                else:
                    self.assertEqual(self.exchange(server, self.initial()), b"")
                    self.assertEqual(server.input_bytes, 0)
                self.assertTrue(server.error)
                self.assertLessEqual(server.connections, probe.SCENARIO_CONNECTIONS)
                self.assertLessEqual(server.requests, probe.SCENARIO_REQUESTS)

    def test_stalled_partial_read_and_expired_case_terminate_and_join(self):
        for overall in (False, True):
            with self.subTest(overall=overall), mock.patch.object(probe, "SCENARIO_HANDLER_SECONDS", .1), self.endpoint() as server:
                if overall:
                    server.deadline = time.monotonic() - 1
                start = time.monotonic()
                self.assertEqual(self.exchange(server, raw=b"POST /responses HTTP/1.1\r\nContent-Length: 100\r\n\r\n{"), b"")
                self.assertLess(time.monotonic() - start, 2)
                self.assertTrue(server.error)
                self.assertEqual(server.receipt()["first_refusal"], "deadline")

    def test_stalled_response_write_has_a_total_deadline(self):
        with mock.patch.object(probe, "SCENARIO_HANDLER_SECONDS", .1), self.endpoint() as server:
            get_request = server.get_request
            def bounded_send_buffer():
                request, address = get_request()
                request.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 1024)
                return request, address
            server.get_request = bounded_send_buffer
            server.scenario.respond = lambda _body: b"x" * (4 * 1024 * 1024)
            with socket.socket() as connection:
                connection.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 1024)
                connection.settimeout(2)
                connection.connect(server.server_address)
                body = json.dumps(self.initial()).encode()
                connection.sendall(b"POST /responses HTTP/1.1\r\nContent-Length: " +
                                   str(len(body)).encode() + b"\r\n\r\n" + body)
                deadline = time.monotonic() + 2
                while not server.error and time.monotonic() < deadline:
                    time.sleep(.01)
                self.assertTrue(server.error)
                self.assertEqual(server.receipt()["first_refusal"], "deadline")

    def test_unknown_owned_group_monitor_never_certifies_cleanup(self):
        process = mock.Mock(pid=12345)
        receipt = {"group_absent": False}
        with mock.patch.object(probe, "owned_group_members", side_effect=RuntimeError("unavailable")), mock.patch.object(probe, "wait_owned_group", side_effect=RuntimeError("cleanup incomplete")), mock.patch.object(probe.os, "killpg", side_effect=PermissionError(1, "synthetic query denied")) as kill:
            with self.assertRaisesRegex(RuntimeError, "cleanup incomplete"):
                probe.stop_scenario_process(process, receipt)
        process.wait.assert_called()
        self.assertEqual(kill.call_args_list, [mock.call(12345, 0), mock.call(12345, 0)])
        self.assertFalse(receipt["group_absent"])

    def test_unavailable_monitor_terminates_and_reaps_owned_python_leader(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            real_popen = subprocess.Popen
            children = []
            def spawn(*args, **kwargs):
                child = real_popen(*args, **kwargs)
                children.append(child)
                return child
            try:
                with mock.patch.object(probe.subprocess, "Popen", side_effect=spawn), mock.patch.object(
                        probe, "owned_group_members", side_effect=RuntimeError("synthetic unavailable")):
                    with self.assertRaisesRegex(RuntimeError, "cleanup incomplete"):
                        probe.run_process([sys.executable, "-B", "-c", "import time; time.sleep(30)"],
                            env=probe.isolated_env(root), cwd=root, timeout=1, group_limit=True, evidence=root)
                self.assertEqual(len(children), 1)
                child = children[0]
                self.assertIsNotNone(child.returncode)
                self.assertTrue(child.stdout.closed and child.stderr.closed)
                self.assertEqual(probe.owned_group_members(child.pid), set())
                receipt = json.loads((root / "process.json").read_text())
                self.assertTrue(receipt["cleanup"]["leader_reaped"])
                self.assertFalse(receipt["cleanup"]["group_absent"])
                self.assertTrue(receipt["cleanup"]["inspection_errors"])
                self.assertFalse(receipt["finished"])
            finally:
                for child in children:
                    if child.poll() is None:
                        probe.stop_process(child)
                    probe.wait_owned_group(child)

    def test_failed_term_still_attempts_kill_and_reaps_owned_python_leader(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            killpg = os.killpg
            def signal_group(group, signum):
                if signum == signal.SIGTERM:
                    raise PermissionError(1, "synthetic TERM denied")
                return killpg(group, signum)
            with mock.patch.object(probe.os, "killpg", side_effect=signal_group):
                result = probe.run_process([sys.executable, "-B", "-c", "import time; time.sleep(30)"],
                    env=probe.isolated_env(root), cwd=root, timeout=.1, group_limit=True, evidence=root)
            receipt = json.loads((root / "process.json").read_text())
            self.assertTrue(result["timeout"])
            self.assertEqual(receipt["cleanup"]["term"], "signal_failed")
            self.assertEqual(receipt["cleanup"]["term_errno"], 1)
            self.assertEqual(receipt["cleanup"]["kill"], "sent")
            self.assertTrue(receipt["cleanup"]["leader_reaped"])
            self.assertTrue(receipt["cleanup"]["group_absent"])
            self.assertEqual(probe.owned_group_members(receipt["pid"]), set())

    def test_cancel_closes_active_sockets_and_joins_handlers(self):
        connection = None
        try:
            with self.endpoint() as server:
                connection = socket.create_connection(server.server_address, timeout=2)
                connection.sendall(b"POST /responses HTTP/1.1\r\nContent-Length: 100\r\n\r\n{")
                deadline = time.monotonic() + 2
                while not server.handlers and time.monotonic() < deadline:
                    time.sleep(.01)
                self.assertTrue(server.handlers)
                # Exit while the peer remains open: close must terminate the read.
            self.assertTrue(server.receipt()["handlers_joined"])
        finally:
            if connection:
                connection.close()
        # This is harness cleanup, never a typed native cancellation observation.

    def test_fixture_foreign_missing_symlink_and_size_changes_refuse(self):
        for mutation in ("path", "missing", "symlink", "size"):
            name = "exec-read-missing" if mutation == "missing" else "exec-read-success"
            with self.subTest(mutation=mutation), self.endpoint(name) as server:
                path = server.scenario.paths[0]
                if mutation == "path":
                    server.scenario.paths[0] = path.parent.parent / "foreign.txt"
                elif mutation == "missing":
                    path.write_text("unexpected")
                elif mutation == "symlink":
                    path.unlink()
                    path.symlink_to("absent")
                else:
                    path.write_bytes(b"x" * (probe.SCENARIO_FILE_LIMIT + 1))
                self.assertEqual(self.exchange(server, self.initial()), b"")
                self.assertTrue(server.error)

    def native_rows(self, scenario):
        calls = [{"id": "fc_control_" + str(index), "call_id": call_id, "name": "read_file",
                  "args": json.dumps(scenario.arguments(index))} for index, call_id in enumerate(scenario.call_ids())]
        results = [{"tool_call_index": index, "tool_call_id": call_id, "text": "Read text file `" + str(scenario.paths[index]) + "`.\n1|" +
                   ["SYNTHETIC_TOOL_RESULT λ", "SYNTHETIC_SECOND_RESULT"][index]}
                   for index, call_id in enumerate(scenario.call_ids())]
        return [{"payload_type": "runtime.session", "stream": {"id": "synthetic"},
                 "payload": {"kind": "run", "run_id": "private-run", "event": event}} for event in (
                     {"kind": "assistant_tool_calls_committed", "message_id": "private-batch", "tool_calls": calls},
                     {"kind": "tool_result_batch_committed", "batch_id": "private-batch", "results": results})]

    def test_native_receipt_checks_join_indices_paths_order_without_retaining_content(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "artifact.txt").write_text("synthetic")
            scenario = probe.ToolScenario("exec-read-two", root)
            folder = root / "synthetic"
            folder.mkdir()
            path = folder / "session.jsonl"
            case = mock.Mock(scenario=scenario, root=root)
            for mutation in ("valid", "reorder", "id", "index", "count", "batch", "owner", "path", "transition"):
                rows = self.native_rows(scenario)
                results = rows[1]["payload"]["event"]["results"]
                if mutation == "reorder":
                    results.reverse()
                elif mutation == "id":
                    results[0]["tool_call_id"] = "foreign"
                elif mutation == "index":
                    results[0]["tool_call_index"] = 1
                elif mutation == "count":
                    results.pop()
                elif mutation == "batch":
                    rows[1]["payload"]["event"]["batch_id"] = "foreign"
                elif mutation == "owner":
                    rows[1]["payload"]["run_id"] = "foreign"
                elif mutation == "path":
                    rows[0]["payload"]["event"]["tool_calls"][0]["args"] = '{"path":"/foreign"}'
                elif mutation == "transition":
                    rows.reverse()
                path.write_text("".join(json.dumps(row) + "\n" for row in rows))
                with self.subTest(mutation=mutation):
                    if mutation not in ("valid", "reorder"):
                        with self.assertRaises(RuntimeError):
                            probe.scenario_tool_receipt(case, path)
                    else:
                        receipt = probe.scenario_tool_receipt(case, path)
                        self.assertEqual(receipt["success_output"], "matched")
                        self.assertEqual(receipt["typed_status"], "unavailable")
                        self.assertEqual(receipt["parallel_execution"], "unavailable")
                        self.assertEqual(receipt["result_order"], mutation == "valid")
                        for secret in ("PRIVATE", "private-run", "private-batch", "read_file", "call_control", str(root)):
                            self.assertNotIn(secret, json.dumps(receipt))

    def test_missing_file_prose_never_becomes_typed_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            scenario = probe.ToolScenario("exec-read-missing", root)
            folder = root / "synthetic"
            folder.mkdir()
            path = folder / "session.jsonl"
            rows = self.native_rows(scenario)
            rows[1]["payload"]["event"]["results"][0]["text"] = "PRIVATE: file missing, error failed cancelled"
            path.write_text("".join(json.dumps(row) + "\n" for row in rows))
            receipt = probe.scenario_tool_receipt(mock.Mock(root=root, scenario=scenario), path)
            self.assertEqual(receipt["success_output"], "unavailable")
            self.assertEqual(receipt["typed_status"], "unavailable")
            self.assertNotIn("PRIVATE", json.dumps(receipt))

    def test_owned_python_group_threshold_aborts_and_cleans(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            pidfile = root / "child.pid"
            source = ("import os,pathlib,time; child=os.fork(); "
                      "pathlib.Path(" + repr(str(pidfile)) + ").write_text(str(child)) if child else None; time.sleep(30)")
            with mock.patch.object(probe, "SCENARIO_GROUP_LIMIT", 1):
                with self.assertRaisesRegex(RuntimeError, "descendant limit"):
                    probe.run_process([sys.executable, "-B", "-c", source], env=probe.isolated_env(root),
                                      cwd=root, timeout=3, group_limit=True)
            self.assertTrue(pidfile.exists())
            child = int(pidfile.read_text())
            # Group absence is checked internally, including on the error path.
            if sys.platform == "darwin":
                with self.assertRaises(ProcessLookupError):
                    os.kill(child, 0)

    def test_exited_python_leader_still_terminates_owned_child(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            pidfile = root / "child.pid"
            # A fixed synthetic startup delay exceeds the old 300ms allowance.
            source = ("import os,pathlib,time\ntime.sleep(.4)\nchild=os.fork()\nif child:\n pathlib.Path(" +
                repr(str(pidfile)) + ").write_text(str(child))\n os._exit(0)\ntime.sleep(30)\n")
            result = probe.run_process([sys.executable, "-B", "-c", source], env=probe.isolated_env(root),
                                       cwd=root, timeout=3, group_limit=True)
            self.assertTrue(result["timeout"])
            self.assertEqual(result["exit"], 0)
            self.assertTrue(pidfile.exists())
            child = int(pidfile.read_text())
            with self.assertRaises(ProcessLookupError):
                os.kill(child, 0)

    def test_native_nonregular_foreign_and_oversize_acquisition_refuses(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            scenario = probe.ToolScenario("exec-read-missing", root)
            case = mock.Mock(root=root, scenario=scenario)
            path = root / "session.jsonl"
            os.mkfifo(path)
            with self.assertRaisesRegex(RuntimeError, "regular-file"):
                probe.scenario_tool_receipt(case, path)
            path.unlink()
            path.write_bytes(b"x" * 33)
            with mock.patch.object(probe, "FILE_LIMIT", 32):
                with self.assertRaisesRegex(RuntimeError, "byte limit"):
                    probe.scenario_tool_receipt(case, path)
            case.root = root / "foreign"
            case.root.mkdir()
            with self.assertRaisesRegex(RuntimeError, "native path"):
                probe.scenario_tool_receipt(case, path)

    def test_no_native_match_and_failed_acquisition_are_not_passing_receipts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            case = mock.Mock(root=root)
            case.execute.return_value = {"exit": 0, "timeout": False}
            case.logs.return_value = []
            receipt = probe.probe_tool_scenario(case)
            self.assertFalse(receipt["assertions"]["one_root_log"])
            self.assertNotIn("tools", receipt)
            with self.assertRaisesRegex(RuntimeError, "provider refused"):
                probe.run_process([sys.executable, "-B", "-c", "import time; time.sleep(30)"],
                    env={}, cwd=root, timeout=3, abort=lambda: True, evidence=root)
            process = json.loads((root / "process.json").read_text())
            self.assertFalse(process["finished"])
            self.assertNotEqual(process["exit"], 0)
            self.assertEqual(probe.file_digest(root / "stdout.bin"), process["stdout_sha256"])
            with self.assertRaises(ProcessLookupError):
                os.kill(process["pid"], 0)

    def test_failed_interrupted_and_no_match_scenarios_keep_unsuccessful_receipts(self):
        for exception in (RuntimeError("PRIVATE"), type("PRIVATE_EXCEPTION_NAME", (RuntimeError,), {})("PRIVATE"), KeyboardInterrupt()):
            with self.subTest(error=type(exception).__name__), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                case = mock.Mock(root=root)
                case.server.receipt.return_value = {"requests": 1, "connections": 1, "provider_complete": False,
                    "provider_refused": True, "provider_result_order": False, "handlers_joined": True}
                with mock.patch.object(probe, "file_digest", return_value=next(iter(probe.PINNED_BUILDS))), mock.patch.object(probe, "verify_version", return_value={}), mock.patch.object(probe, "Case", return_value=case), mock.patch.object(probe, "probe_tool_scenario", side_effect=exception):
                    if isinstance(exception, KeyboardInterrupt):
                        with self.assertRaises(KeyboardInterrupt):
                            probe.native_run(root / "binary", root / "output", ["exec-read-missing"])
                    else:
                        probe.native_run(root / "binary", root / "output", ["exec-read-missing"])
                report = json.loads((root / "output/report.json").read_text())
                self.assertIn("failed", report["cases"]["exec-read-missing"])
                self.assertNotIn("PRIVATE", json.dumps(report))
                case.close.assert_called_once()


if __name__ == "__main__":
    unittest.main()
