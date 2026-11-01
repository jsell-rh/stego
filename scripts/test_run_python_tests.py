#!/usr/bin/env python3
"""Check that the Python runner writes each unittest result as assure record reads it."""

import contextlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest


spec = importlib.util.spec_from_file_location("runner", Path(__file__).with_name("run-python-tests.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)

SUITE = '''
import unittest


class Fixture(unittest.TestCase):
    def test_cases(self):
        with self.subTest("ST-FIX-INV01-T holds"):
            self.assertEqual(4, 2 + 2)
        with self.subTest("ST-FIX-INV02-T breaks"):
            self.assertEqual(5, 2 + 2)
        with self.subTest("ST-FIX-INV03-T waits"):
            self.skipTest("no network")

    @unittest.expectedFailure
    def test_known_gap(self):
        self.assertEqual(5, 2 + 2)

    @unittest.expectedFailure
    def test_gap_closed(self):
        with self.subTest("ST-FIX-INV04-T was a gap"):
            self.assertEqual(4, 2 + 2)
'''


class RunnerChecks(unittest.TestCase):
    def test_results(self):
        with self.subTest("ST-ASR-DS01-T03 The Python runner counts only a real pass as passed, and names each subtest by its description"):
            with tempfile.TemporaryDirectory() as folder:
                suite = Path(folder) / "suite"
                suite.mkdir()
                (suite / "test_fixture.py").write_text(SUITE)
                output = Path(folder) / "results.json"
                with contextlib.redirect_stderr(io.StringIO()):
                    code = runner.main(["--start", str(suite), "--output", str(output)])
                sys.modules.pop("test_fixture", None)
                self.assertEqual(0, code)
                document = json.loads(output.read_text())
            self.assertEqual(1, document["format"])
            results = {entry["name"]: (entry["result"], entry["detail"]) for entry in document["tests"]}
            self.assertEqual(("passed", ""), results["[ST-FIX-INV01-T holds]"])
            self.assertEqual("failed", results["[ST-FIX-INV02-T breaks]"][0])
            self.assertIn("5 != 4", results["[ST-FIX-INV02-T breaks]"][1])
            self.assertEqual(("skipped", "no network"), results["[ST-FIX-INV03-T waits]"])
            self.assertEqual(("failed", "a subtest failed"), results["test_cases"])
            self.assertEqual("skipped", results["test_known_gap"][0])
            self.assertTrue(results["test_known_gap"][1].startswith("expected failure"))
            self.assertEqual(("failed", "unexpected success"), results["test_gap_closed"])
            self.assertEqual(("failed", "unexpected success"), results["[ST-FIX-INV04-T was a gap]"])
            for entry in document["tests"]:
                self.assertIn(entry["result"], ("passed", "failed", "skipped"))


if __name__ == "__main__":
    unittest.main()
