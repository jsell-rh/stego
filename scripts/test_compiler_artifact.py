#!/usr/bin/env python3
"""Check artifact input rejection and process bounds without building Go code."""

import importlib.util
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("artifact", Path(__file__).with_name("check-compiler-artifact.py"))
artifact = importlib.util.module_from_spec(spec)
spec.loader.exec_module(artifact)


class ArtifactChecks(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def test_inventory_tracks_bytes_and_executable_bits(self):
        file = self.root / "input"
        file.write_bytes(b"first")
        file.chmod(0o644)
        before = artifact.inventory(self.root)
        file.write_bytes(b"other")
        changed = artifact.inventory(self.root)
        self.assertNotEqual(before["sha256"], changed["sha256"])
        file.chmod(0o755)
        self.assertNotEqual(changed["sha256"], artifact.inventory(self.root)["sha256"])

    def test_inventory_rejects_links_and_size_limits(self):
        file = self.root / "input"
        file.write_bytes(b"data")
        for target in [file, self.root]:
            link = self.root / "link"
            link.symlink_to(target)
            with self.assertRaises(artifact.CheckError):
                artifact.inventory(self.root)
            link.unlink()
        with self.assertRaises(artifact.CheckError):
            artifact.inventory(self.root, max_bytes=3)
        with self.assertRaises(artifact.CheckError):
            artifact.inventory(self.root, max_files=0)

    def test_environment_excludes_ambient_settings_and_credentials(self):
        with patch.dict(os.environ, {"GOFLAGS": "-toolexec=private-command", "GIT_CONFIG_COUNT": "9",
                                     "GH_TOKEN": "private-token", "GOPROXY": "https://private.invalid",
                                     "LD_PRELOAD": "/private.so", "CC": "private-compiler"}):
            env = artifact.environment(self.root, Path("/toolchain"))
        self.assertEqual(env["GOFLAGS"], "")
        self.assertEqual(env["GOWORK"], "off")
        self.assertEqual(env["GOENV"], "off")
        self.assertEqual(env["GOTOOLCHAIN"], "local")
        self.assertEqual(env["GIT_CONFIG_COUNT"], "0")
        self.assertEqual(env["GOPROXY"], "https://proxy.golang.org")
        self.assertFalse({"GH_TOKEN", "LD_PRELOAD", "CC"} & env.keys())
        self.assertNotIn("private", json.dumps(env))

    def test_build_rejects_untrusted_sdk_before_executing_any_command(self):
        source = self.root / "source"
        source.mkdir()
        go = self.root / "sdk" / "bin" / "go"
        go.parent.mkdir(parents=True)
        go.write_text("#!/bin/sh\necho 'go version go1.26.11 linux/amd64'\n")
        go.chmod(0o755)
        with patch.object(artifact, "command") as command:
            with self.assertRaisesRegex(artifact.CheckError, "pinned official release"):
                artifact.build(source, "a" * 40, self.root / "work", self.root / "output", go)
            command.assert_not_called()
        self.assertFalse((self.root / "output").exists())

    def test_module_records_require_checksums_and_unique_paths(self):
        value = {"Path": "example.org/module", "Version": "v1.0.0",
                 "Sum": "h1:" + base64.b64encode(b"a" * 32).decode(),
                 "GoModSum": "h1:" + base64.b64encode(b"b" * 32).decode()}
        encoded = json.dumps(value).encode()
        self.assertEqual(artifact.module_records(encoded), [value])
        for data in [b"", b"[]", encoded + b"\n" + encoded,
                     json.dumps(dict(value, Error="private error")).encode(),
                     json.dumps(dict(value, Replace={"Path": "../local"})).encode(),
                     json.dumps(dict(value, Sum="")).encode(),
                     json.dumps(dict(value, Sum="h1:" + "B" * 43 + "=")).encode(),
                     json.dumps(dict(value, Sum="unchecked")).encode()]:
            with self.subTest(data=data), self.assertRaises(artifact.CheckError):
                artifact.module_records(data)

    def test_command_limits_output_and_withholds_diagnostics(self):
        for code, limit in [("print('x' * 1000)", 16),
                            ("print('private diagnostic'); raise SystemExit(1)", 1024)]:
            with self.assertRaises(artifact.CheckError) as caught:
                artifact.command([sys.executable, "-c", code], self.root, {}, limit=limit)
            self.assertNotIn("private diagnostic", str(caught.exception))

    def test_timeout_stops_descendants_after_parent_exit(self):
        marker = self.root / "child.json"
        child = ("import json,os,time; from pathlib import Path; "
                 f"Path({str(marker)!r}).write_text(json.dumps([os.getpid(),os.getpgrp()])); time.sleep(10)")
        parent = ("import subprocess,sys,time,os; from pathlib import Path; "
                  f"subprocess.Popen([sys.executable,'-c',{child!r}]); "
                  f"marker=Path({str(marker)!r})\n"
                  "while not marker.exists(): time.sleep(.005)\n"
                  "os._exit(0)")
        try:
            with self.assertRaises(artifact.CheckError):
                artifact.command([sys.executable, "-c", parent], self.root, {}, timeout=1)
            self.assertTrue(marker.is_file(), "The child did not start")
            pid, group = json.loads(marker.read_text())
            status = Path(f"/proc/{pid}/stat")
            deadline = time.monotonic() + 1
            while status.exists():
                if status.read_text().rsplit(")", 1)[1].split()[0] == "Z":
                    break
                if time.monotonic() >= deadline:
                    self.fail("The child remains active after the command timeout")
                time.sleep(.01)
        finally:
            if marker.exists():
                pid, group = json.loads(marker.read_text())
                try:
                    if os.getpgid(pid) == group:
                        os.killpg(group, 9)
                except ProcessLookupError:
                    pass

    def test_source_rejects_wrong_revision_changed_untracked_and_ignored_files(self):
        git = shutil.which("git", path="/usr/bin:/bin")
        env = artifact.environment(self.root, Path("/unused-toolchain"))
        def run(*args):
            return artifact.command([git, *args], self.root, env)
        run("init", "-q")
        (self.root / "input").write_text("original\n")
        (self.root / ".gitignore").write_text("ignored\n")
        run("add", "input", ".gitignore")
        run("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture")
        revision = run("rev-parse", "HEAD").decode().strip()
        artifact.clean_source(git, self.root, revision, env)
        with self.assertRaises(artifact.CheckError):
            artifact.clean_source(git, self.root, "0" * 40, env)
        for name in ["input", "untracked", "ignored"]:
            with self.subTest(name=name):
                path = self.root / name
                path.write_text("changed\n")
                with self.assertRaises(artifact.CheckError):
                    artifact.clean_source(git, self.root, revision, env)
                if name == "input":
                    path.write_text("original\n")
                else:
                    path.unlink()


WORKFLOW = Path(__file__).resolve().parent.parent / ".github" / "workflows" / "compiler-artifact.yml"


def job_condition(expression, context):
    """Evaluate a job condition of the form a == 'x' && b != 'y', as GitHub Actions does for these terms."""
    result = True
    for term in expression.split("&&"):
        found = re.fullmatch(r"\s*([\w.]+) (==|!=) '([^']*)'\s*", term)
        if found is None:
            raise AssertionError("the test does not evaluate the term " + term)
        value = context[found.group(1)]
        result = result and ((value == found.group(3)) == (found.group(2) == "=="))
    return result


@unittest.skipUnless(shutil.which("bash"), "the step runs with bash, as GitHub Actions runs it")
class OnlyTheMainBranchSigningJobSigns(unittest.TestCase):
    """The artifact workflow's job graph, read from compiler-artifact.yml, with its digest step run as a process.

    GitHub Actions is the hosted CI provider: the test is its local stand-in. It evaluates each job's condition
    for the events GitHub sends, reads the permissions each job holds, and runs the step that guards the signature
    with bash and the real python3, as a runner does. No test reaches GitHub.
    """

    def setUp(self):
        import yaml
        data = yaml.safe_load(WORKFLOW.read_text())
        self.workflow = data
        self.jobs = data["jobs"]
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def guard(self, binary, record):
        """Run the step that requires the exact bytes the build job checked, with the digests it gave."""
        steps = self.jobs["provenance"]["steps"]
        step = next(step for step in steps if step.get("name") == "Require the exact bytes checked by the build job")
        artifact = self.root / "compiler-artifact"
        artifact.mkdir(exist_ok=True)
        (artifact / "stego-linux-amd64").write_bytes(b"fixture compiler; do not execute\n")
        (artifact / "build.json").write_bytes(b'{"format": 1}\n')
        script = self.root / "step.sh"
        script.write_text(step["run"])
        environment = {"PATH": "/usr/bin:/bin", "RUNNER_TEMP": str(self.root), "BINARY_SHA256": binary,
                       "RECORD_SHA256": record}
        done = subprocess.run([shutil.which("bash"), "-e", str(script)], cwd=self.root, env=environment,
                              stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)
        return done.returncode, done.stdout.decode(), artifact

    def test_only_the_main_branch_signing_job_signs(self):
        with self.subTest("ST-DIS-INV11-T Only the main-branch signing job signs, and only the bytes it fetched"):
            provenance = self.jobs["provenance"]
            condition = provenance["if"]
            base = {"github.repository": "jsell-rh/stego"}
            events = [
                ("a pull request", {"github.event_name": "pull_request", "github.ref": "refs/pull/7/merge"}, False),
                ("a push to another branch", {"github.event_name": "push", "github.ref": "refs/heads/topic"}, False),
                ("a dispatch of another branch", {"github.event_name": "workflow_dispatch",
                                                  "github.ref": "refs/heads/topic"}, False),
                ("a push to main", {"github.event_name": "push", "github.ref": "refs/heads/main"}, True),
                ("a dispatch of main", {"github.event_name": "workflow_dispatch", "github.ref": "refs/heads/main"},
                 True)]
            for label, context, signs in events:
                with self.subTest(event=label):
                    self.assertEqual(job_condition(condition, dict(base, **context)), signs, label)
            self.assertFalse(job_condition(condition, {"github.repository": "fork/stego", "github.event_name": "push",
                                                       "github.ref": "refs/heads/main"}))
            # The signing job holds the only attestation permission, and the workflow grants only read access.
            self.assertEqual(self.workflow["permissions"], {"contents": "read"})
            self.assertEqual(provenance["permissions"], {"contents": "read", "id-token": "write",
                                                         "attestations": "write"})
            for name, job in self.jobs.items():
                if name != "provenance":
                    self.assertNotIn("id-token", job.get("permissions", {}), name)
                    self.assertNotIn("attestations", job.get("permissions", {}), name)
            # It signs only after a successful build, the artifact it fetched by its immutable ID, at the paths it
            # checked, and after the step that requires the build job's digests.
            self.assertEqual(provenance["needs"], "linux-amd64")
            steps = provenance["steps"]
            names = [step.get("name") for step in steps]
            fetch = steps[names.index("Retrieve the checked artifact from this run")]
            self.assertEqual(fetch["with"]["artifact-ids"], "${{ needs.linux-amd64.outputs.artifact-id }}")
            guard = names.index("Require the exact bytes checked by the build job")
            sign = names.index("Sign the compiler and build record")
            self.assertLess(names.index("Retrieve the checked artifact from this run"), guard)
            self.assertLess(guard, sign)
            self.assertRegex(steps[sign]["uses"], r"^actions/attest@[0-9a-f]{40}")
            self.assertNotIn("if", steps[sign])
            self.assertEqual(steps[guard]["env"], {
                "BINARY_SHA256": "${{ needs.linux-amd64.outputs.binary-sha256 }}",
                "RECORD_SHA256": "${{ needs.linux-amd64.outputs.record-sha256 }}"})
            self.assertEqual(steps[sign]["with"]["subject-path"].splitlines(),
                             ["${{ runner.temp }}/compiler-artifact/stego-linux-amd64",
                              "${{ runner.temp }}/compiler-artifact/build.json"])
            # The guard passes when the fetched bytes are the ones the build job checked, and fails otherwise, so a
            # job whose fetched digests differ never reaches the signing step.
            binary = hashlib.sha256(b"fixture compiler; do not execute\n").hexdigest()
            record = hashlib.sha256(b'{"format": 1}\n').hexdigest()
            status, log, _ = self.guard(binary, record)
            self.assertEqual(status, 0, log)
            for changed in [("0" * 64, record), (binary, "0" * 64), ("not-a-digest", record)]:
                status, log, _ = self.guard(*changed)
                self.assertNotEqual(status, 0, changed)
            status, log, _ = self.guard("0" * 64, record)
            self.assertIn("The downloaded bytes differ from the checked build", log)


if __name__ == "__main__":
    unittest.main()
