#!/usr/bin/env python3
"""Check artifact input rejection and process bounds without building Go code."""

import importlib.util
import base64
import json
import os
from pathlib import Path
import shutil
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


if __name__ == "__main__":
    unittest.main()
