#!/usr/bin/env python3
"""Check compiler installation with small files and no network or compiler execution."""

import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("installer", Path(__file__).with_name("install-compiler.py"))
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


class InstallationChecks(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.output = self.root / "installed"
        self.revision = "a" * 40
        self.gh = Path(sys.executable)
        binary = b"fixture compiler; do not execute"
        artifact = {"name": installer.verification.BINARY, "size": len(binary),
                    "sha256": hashlib.sha256(binary).hexdigest()}
        record = {"format": 1, "source_revision": self.revision, "artifact": artifact,
                  "version": {"build": {"goos": "linux", "goarch": "amd64",
                              "revision": self.revision, "vcs": "git", "source_state": "clean"}}}
        self.files = {installer.verification.BINARY: binary,
                      "build.json": json.dumps(record).encode(), "provenance.jsonl": b"fixture signatures"}
        self.files["SHA256SUMS"] = "".join(
            hashlib.sha256(self.files[name]).hexdigest() + "  " + name + "\n"
            for name in [installer.verification.BINARY, "build.json"]).encode()
        self.release = {"id": 1, "tag_name": "compiler-" + self.revision,
                        "target_commitish": self.revision, "draft": False,
                        "prerelease": False, "immutable": True, "assets": []}
        for identity, (name, data) in enumerate(self.files.items(), 100):
            self.release["assets"].append({"id": identity, "name": name, "size": len(data),
                                          "digest": "sha256:" + hashlib.sha256(data).hexdigest(),
                                          "state": "uploaded", "browser_download_url": "https://untrusted.invalid"})

    def fetch(self, gh, endpoint, directory, *, limit, binary=False):
        self.assertEqual(gh, self.gh)
        self.assertNotEqual(directory, self.output)
        self.assertFalse(self.output.exists())
        if endpoint == installer.API + "tags/compiler-" + self.revision:
            self.assertFalse(binary)
            self.assertEqual(limit, 1 << 20)
            return json.dumps(self.release).encode()
        for asset in self.release["assets"]:
            if endpoint == installer.API + "assets/" + str(asset["id"]):
                self.assertTrue(binary)
                self.assertEqual(limit, asset["size"])
                return self.files[asset["name"]]
        self.fail("Unexpected API path")

    def install(self):
        return installer.install(self.revision, self.output, self.gh)

    def test_release_requires_both_signatures_and_publishes_captured_bytes(self):
        signed = []
        def authenticate(gh, path, bundle, revision, environment):
            self.assertFalse(self.output.exists())
            self.assertEqual(revision, self.revision)
            self.assertEqual(bundle.read_bytes(), self.files["provenance.jsonl"])
            signed.append(path.name)
        with patch.object(installer, "api", side_effect=self.fetch), \
                patch.object(installer.verification, "authenticate", side_effect=authenticate):
            result = self.install()
        self.assertEqual(signed, [installer.verification.BINARY, "build.json"])
        self.assertEqual(result["source_revision"], self.revision)
        self.assertEqual((self.output / installer.verification.BINARY).read_bytes(),
                         self.files[installer.verification.BINARY])
        self.assertEqual(list(self.root.glob(".stego-download-*")), [])

    def test_mutable_draft_other_revision_and_repeated_assets_are_rejected(self):
        bad = []
        for field, value in [("immutable", False), ("draft", True), ("prerelease", True),
                             ("target_commitish", "main"), ("tag_name", "compiler-latest"), ("id", True)]:
            record = copy.deepcopy(self.release); record[field] = value; bad.append(record)
        record = copy.deepcopy(self.release); record["assets"][1] = record["assets"][0]; bad.append(record)
        for field, value in [("id", True), ("size", True), ("size", 65 << 20),
                             ("state", "new"), ("digest", "sha256:wrong"), ("name", "../compiler")]:
            record = copy.deepcopy(self.release); record["assets"][0][field] = value; bad.append(record)
        for record in bad:
            with self.subTest(record=record), self.assertRaises(installer.CheckError):
                installer.release_assets(record, self.revision)

    def test_changed_asset_and_failed_signature_leave_no_result(self):
        original = self.files[installer.verification.BINARY]
        self.files[installer.verification.BINARY] = b"x" * len(original)
        with patch.object(installer, "api", side_effect=self.fetch), \
                patch.object(installer.verification, "authenticate") as authenticate:
            with self.assertRaises(installer.CheckError):
                self.install()
            authenticate.assert_not_called()
        self.files[installer.verification.BINARY] = original
        with patch.object(installer, "api", side_effect=self.fetch), \
                patch.object(installer.verification, "authenticate", side_effect=installer.CheckError("denied")):
            with self.assertRaises(installer.CheckError):
                self.install()
        self.assertFalse(self.output.exists())
        self.assertEqual(list(self.root.glob(".stego-*-*")), [])

    def test_explicit_package_does_not_download_or_trust_a_saved_verified_record(self):
        package = self.root / "package"; package.mkdir()
        for name, data in self.files.items():
            (package / name).write_bytes(data)
        (package / "verified.json").write_text('{"trusted":true}')
        with patch.object(installer, "api") as api, \
                patch.object(installer.verification, "authenticate") as authenticate:
            installer.install(self.revision, self.output, self.gh, package)
        api.assert_not_called()
        self.assertEqual(authenticate.call_count, 2)

    def test_existing_output_bad_revision_and_platform_fail_before_download(self):
        with patch.object(installer, "api") as api:
            with self.assertRaises(installer.CheckError):
                installer.install("main", self.output, self.gh)
            with patch.object(installer.platform, "machine", return_value="aarch64"):
                with self.assertRaises(installer.CheckError):
                    self.install()
            self.output.mkdir(); (self.output / "keep").write_text("retained")
            with self.assertRaises(installer.CheckError):
                self.install()
            self.assertEqual((self.output / "keep").read_text(), "retained")
            api.assert_not_called()

    def test_api_has_fixed_host_headers_environment_and_process_bounds(self):
        with patch.object(installer.verification.control, "command", return_value=b"data") as command:
            installer.api(self.gh, installer.API + "assets/100", self.root, limit=100, binary=True)
        args, cwd, environment = command.call_args.args
        self.assertEqual(args[1:6], ["api", "--hostname", "github.com", "--method", "GET"])
        self.assertIn("Accept: application/octet-stream", args)
        self.assertEqual(environment["GH_HOST"], "github.com")
        self.assertEqual(command.call_args.kwargs, {"timeout": 90, "limit": 100})


if __name__ == "__main__":
    unittest.main()
