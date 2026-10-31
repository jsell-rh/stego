#!/usr/bin/env python3
"""Run STEGO's Python suites and write each test's result as JSON for assure record.

Usage: run-python-tests.py --output <file> [--start <folder>]

The runner finds every test_*.py file in the start folder (scripts by default), runs it with unittest, and
writes one JSON document to the output file:

  {"format": 1, "started": "<UTC time>", "finished": "<UTC time>",
   "tests": [{"test": "<unittest ID>", "name": "<own name>", "result": "passed|failed|skipped",
              "elapsed": <seconds or null>, "detail": "<why it failed or was skipped>"}]}

There is one entry for each test method and one for each subtest. The name of a test method is the method's
name; the name of a subtest is its description, which carries the IDs of the test records it proves. assure record
takes the IDs from the name (ST-ASR-DS01). An error counts as failed, an expected failure as passed, and an
unexpected success as failed. The runner exits 0 when it wrote the document, whatever the results, and 2 when
it was called wrongly. It writes no bytecode.
"""

import argparse
import datetime
import json
import os
from pathlib import Path
import sys
import time
import traceback
import unittest

FORMAT = 1


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")


class Results(unittest.TestResult):
    """A unittest result that keeps one entry per test method and subtest."""

    def __init__(self):
        super().__init__()
        self.entries = []
        self.started = {}
        self.recorded = set()
        self.subtest_failed = set()

    def startTest(self, test):
        super().startTest(test)
        self.started[test.id()] = time.monotonic()

    def stopTest(self, test):
        if test.id() not in self.recorded:
            if test.id() in self.subtest_failed:
                self.entry(test, "failed", "a subtest failed")
            else:
                self.entry(test, "passed", "")
        super().stopTest(test)

    def entry(self, test, result, detail):
        subtest = hasattr(test, "test_case")
        if subtest:
            name = test._subDescription()
            elapsed = None
        else:
            self.recorded.add(test.id())
            name = getattr(test, "_testMethodName", test.id())
            start = self.started.get(test.id())
            elapsed = round(time.monotonic() - start, 3) if start is not None else None
        self.entries.append({"test": test.id(), "name": name, "result": result,
                             "elapsed": elapsed, "detail": detail})

    def addSuccess(self, test):
        super().addSuccess(test)
        self.entry(test, "passed", "")

    def addFailure(self, test, err):
        super().addFailure(test, err)
        self.entry(test, "failed", self.describe(err))

    def addError(self, test, err):
        super().addError(test, err)
        self.entry(test, "failed", self.describe(err))

    def addSkip(self, test, reason):
        super().addSkip(test, reason)
        self.entry(test, "skipped", reason)

    def addExpectedFailure(self, test, err):
        super().addExpectedFailure(test, err)
        self.entry(test, "passed", "")

    def addUnexpectedSuccess(self, test):
        super().addUnexpectedSuccess(test)
        self.entry(test, "failed", "unexpected success")

    def addSubTest(self, test, subtest, err):
        super().addSubTest(test, subtest, err)
        if err is None:
            self.entry(subtest, "passed", "")
        else:
            self.subtest_failed.add(test.id())
            self.entry(subtest, "failed", self.describe(err))

    @staticmethod
    def describe(err):
        return "".join(traceback.format_exception_only(err[0], err[1])).strip()


def main(argv):
    parser = argparse.ArgumentParser(description="Run the Python suites and write their results as JSON.")
    parser.add_argument("--output", required=True, help="the JSON file to write")
    parser.add_argument("--start", default=str(Path(__file__).resolve().parent),
                        help="the folder that holds the test_*.py files")
    arguments = parser.parse_args(argv)
    sys.dont_write_bytecode = True
    start = os.path.abspath(arguments.start)
    results = Results()
    started = now()
    for path in sorted(Path(start).glob("test_*.py")):
        suite = unittest.defaultTestLoader.discover(start, pattern=path.name, top_level_dir=start)
        suite.run(results)
    document = {"format": FORMAT, "started": started, "finished": now(), "tests": results.entries}
    with open(arguments.output, "w", encoding="utf-8") as output:
        json.dump(document, output, indent=1)
        output.write("\n")
    print(f"{len(results.entries)} Python test results written to {arguments.output}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
