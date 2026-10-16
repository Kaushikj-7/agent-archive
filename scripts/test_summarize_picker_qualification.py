#!/usr/bin/env python3
"""Check that paired qualification requires every named process and case."""
import copy
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parent.parent
ARTIFACT = ROOT / "dev/maintainers/picker-qualification/results-20261013.json"


class QualificationSummaryTests(unittest.TestCase):
    def setUp(self):
        self.artifact = json.loads(ARTIFACT.read_text())
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.output = pathlib.Path(self.temp.name)

    def summarize(self, mutate=None):
        artifact = copy.deepcopy(self.artifact)
        if mutate:
            mutate(artifact)
        (self.output / "manifest.json").write_text(json.dumps(artifact["Manifest"]))
        for process in artifact["ProcessSamples"]:
            name = process["Name"]
            rows = [row for row in artifact["PhaseSamples"]
                    if name == f"{row['Sessions']}-{row['Kind']}-{row['Repeat']}-{row['Source']}"]
            (self.output / (name + ".txt")).write_text("".join(
                json.dumps({key: value for key, value in row.items()
                            if key not in ("Source", "Repeat", "Kind")}) + "\n"
                for row in rows))
            (self.output / (name + "-process.json")).write_text(json.dumps(process))
        return subprocess.run([sys.executable, str(ROOT / "scripts/summarize_picker_qualification.py"),
                               str(self.output)], capture_output=True, text=True)

    def test_complete_paired_matrix_preserves_phase_samples(self):
        result = self.summarize()
        self.assertEqual(result.returncode, 0, result.stderr)
        summary = json.loads((self.output / "summary.json").read_text())
        self.assertEqual(summary["PhaseSamples"], self.artifact["PhaseSamples"])
        self.assertEqual(summary["CaseSummary"], self.artifact["CaseSummary"])

    def test_same_count_paired_matrix_cannot_replace_required_command_or_state(self):
        for field, value in (("Command", "missing-show"), ("CacheState", "missing-changed")):
            with self.subTest(field=field):
                def mutate(artifact):
                    for row in artifact["PhaseSamples"]:
                        if row["Kind"] == "main" and row["Command"] == "show" and row["CacheState"] == "changed":
                            row[field] = value
                result = self.summarize(mutate)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("unexpected measured case", result.stderr)

    def test_native_matrix_requires_both_registration_costs(self):
        def mutate(artifact):
            for row in artifact["PhaseSamples"]:
                if row["Kind"] == "native" and row["Registrations"] == 120:
                    row["Registrations"] = 121
        result = self.summarize(mutate)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unexpected measured case", result.stderr)

    def test_process_manifest_cannot_replace_required_repeat(self):
        def mutate(artifact):
            artifact["Manifest"]["completed"][-1] = "100000-native-3-baseline"
        result = self.summarize(mutate)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing paired process", result.stderr)

    def test_process_manifest_cannot_duplicate_a_process(self):
        def mutate(artifact):
            artifact["Manifest"]["completed"][-1] = artifact["Manifest"]["completed"][-2]
        result = self.summarize(mutate)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing paired process", result.stderr)


if __name__ == "__main__":
    unittest.main()
