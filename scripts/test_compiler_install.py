#!/usr/bin/env python3
"""Check compiler installation with small files and no network or compiler execution."""

import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
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
        with self.subTest("In process: Installation verifies both signatures before it puts the binary in place"):
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
        with self.subTest("ST-DIS-INV08-T A changed asset or a failed signature leaves no installation"):
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
            with self.subTest("ST-VO43-C01-T A revision that is not a full commit ID is refused before any download"):
                with self.assertRaises(installer.CheckError):
                    installer.install("main", self.output, self.gh)
                api.assert_not_called()
            with self.subTest("In process: Installation refuses a platform other than Linux amd64 before any download"):
                with patch.object(installer.platform, "machine", return_value="aarch64"):
                    with self.assertRaises(installer.CheckError):
                        self.install()
                api.assert_not_called()
            with self.subTest("ST-DIS-INV07-T An installation refuses an existing result folder and keeps what it holds"):
                self.output.mkdir(); (self.output / "keep").write_text("retained")
                with self.assertRaises(installer.CheckError):
                    self.install()
                self.assertEqual((self.output / "keep").read_text(), "retained")
                api.assert_not_called()
            api.assert_not_called()

    def test_api_has_fixed_host_headers_environment_and_process_bounds(self):
        with patch.object(installer.verification.control, "command", return_value=b"data") as command:
            installer.api(self.gh, installer.API + "assets/100", self.root, limit=100, binary=True)
        args, cwd, environment = command.call_args.args
        self.assertEqual(args[1:6], ["api", "--hostname", "github.com", "--method", "GET"])
        self.assertIn("Accept: application/octet-stream", args)
        self.assertEqual(environment["GH_HOST"], "github.com")
        self.assertEqual(command.call_args.kwargs, {"timeout": 90, "limit": 100})

    def test_metadata_failure_identifies_stage_without_private_output_or_result(self):
        with patch.object(installer, "api", side_effect=installer.CheckError("private-token\n/private/path")), \
                patch.object(installer.verification, "authenticate") as authenticate:
            with self.assertRaises(installer.CheckError) as caught:
                self.install()
        self.assertEqual(str(caught.exception),
                         "Compiler release metadata retrieval failed; private command output is withheld")
        authenticate.assert_not_called()
        self.assertFalse(self.output.exists())
        self.assertEqual(list(self.root.glob(".stego-*-*")), [])

    def test_asset_failures_name_only_the_fixed_asset_and_leave_no_result(self):
        for name in installer.ASSETS:
            endpoint = next(installer.API + "assets/" + str(asset["id"])
                            for asset in self.release["assets"] if asset["name"] == name)
            def fetch(gh, selected, directory, **options):
                if selected == endpoint:
                    raise installer.CheckError("private-token\n/private/path")
                return self.fetch(gh, selected, directory, **options)
            with self.subTest(asset=name), patch.object(installer, "api", side_effect=fetch), \
                    patch.object(installer.verification, "authenticate") as authenticate:
                with self.assertRaises(installer.CheckError) as caught:
                    self.install()
                self.assertEqual(str(caught.exception), "Compiler asset download failed for " + name +
                                 "; private command output is withheld")
                authenticate.assert_not_called()
                self.assertFalse(self.output.exists())
                self.assertEqual(list(self.root.glob(".stego-*-*")), [])

    def test_signature_failures_identify_fixed_subject_without_publishing_a_result(self):
        package = self.root / "package"
        package.mkdir()
        for name, data in self.files.items():
            (package / name).write_bytes(data)
        for use_package in [False, True]:
            for subject in [installer.verification.BINARY, "build.json"]:
                signed = []
                def authenticate(gh, path, bundle, revision, environment):
                    signed.append(path.name)
                    if path.name == subject:
                        raise installer.CheckError("private-token\n/private/path")
                with self.subTest(package=use_package, subject=subject), \
                        patch.object(installer, "api", side_effect=self.fetch) as api, \
                        patch.object(installer.verification, "authenticate", side_effect=authenticate):
                    with self.assertRaises(installer.CheckError) as caught:
                        installer.install(self.revision, self.output, self.gh, package if use_package else None)
                    self.assertEqual(str(caught.exception), "Compiler signature verification failed for " + subject +
                                     "; private command output is withheld")
                    self.assertEqual(signed, [installer.verification.BINARY] if subject == installer.verification.BINARY
                                     else [installer.verification.BINARY, "build.json"])
                    if use_package:
                        api.assert_not_called()
                    self.assertFalse(self.output.exists())
                    self.assertEqual(list(self.root.glob(".stego-*-*")), [])


# The installer as a process, as an operator runs it. GitHub stands behind the fake gh of test_release_publishing.py,
# which answers from fixed records; its attestation check is the local stand-in for the provider's trusted signer.
# The publish-release job of proof-run.yml publishes the release first, as CI does. No test reaches GitHub.
publishing_spec = importlib.util.spec_from_file_location(
    "publishing", Path(__file__).with_name("test_release_publishing.py"))
publishing = importlib.util.module_from_spec(publishing_spec)
try:
    publishing_spec.loader.exec_module(publishing)
except ModuleNotFoundError as missing:
    # The fixture reads the workflows with PyYAML; a runner without it skips the process tests.
    publishing = None
    PUBLISHING_MISSING = str(missing)
SCRIPTS = Path(__file__).resolve().parent

# A machine that reports aarch64: Python on it answers platform.machine() with aarch64. The installer runs
# unchanged; only the host's answer differs.
AARCH64 = "import platform\nplatform.machine = lambda: 'aarch64'\n"


@unittest.skipUnless(publishing is not None and sys.platform == "linux" and shutil.which("jq") and shutil.which("unzip"),
                     "the publish job and the installer run as Linux processes with jq and unzip")
class InstallationAsAProcess(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.world = publishing.World(self.root)
        empty = self.root / "workspace"
        empty.mkdir()
        run = publishing.run_job(self.world, "publish-release", empty)
        self.assertEqual(run.status, 0, run.log)
        self.world.load()
        self.output = self.root / "installed"

    def install(self, prefix=(), environment=None):
        self.world.save()
        self.world.clear_calls()
        done = subprocess.run([*prefix, sys.executable, "-B", str(SCRIPTS / "install-compiler.py"),
                               "--revision", self.world.commit, "--output", str(self.output),
                               "--gh", str(self.world.gh), "--release"],
                              cwd=self.root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=120,
                              env=dict({"PATH": "/usr/bin:/bin", "HOME": str(self.root)}, **(environment or {})))
        return done.returncode, done.stdout.decode()

    def test_the_qualified_platform_is_enforced(self):
        with self.subTest("ST-DIS-UC09-R01-T01 Installation refuses a platform other than Linux amd64 before any download"):
            machine = self.root / "aarch64"
            machine.mkdir()
            (machine / "sitecustomize.py").write_text(AARCH64)
            for label, prefix, environment in [("aarch64", (), {"PYTHONPATH": str(machine)}),
                                               ("i686, as the kernel reports it", ("setarch", "linux32"), None)]:
                if prefix and shutil.which(prefix[0]) is None:
                    continue
                with self.subTest(machine=label):
                    status, log = self.install(prefix, environment)
                    self.assertNotEqual(status, 0, log)
                    self.assertIn("This compiler package requires Linux amd64", log)
                    self.assertEqual(self.world.calls(), [])
                    self.assertFalse(self.output.exists())
                    self.assertEqual(list(self.root.glob(".stego-*")), [])

    def test_identity_is_verified_before_the_binary_is_used(self):
        with self.subTest("ST-DIS-UC09-R02-T01 Installation verifies both signatures before it puts the binary in place"):
            self.world.state["watch"] = str(self.output)
            status, log = self.install()
            self.assertEqual(status, 0, log)
            checks = [json.loads(line) for line in (self.world.bin / "watch.jsonl").read_text().splitlines()]
            self.assertEqual([(check["subject"], check["bundle"]) for check in checks],
                             [(installer.verification.BINARY, True), ("build.json", True)])
            self.assertFalse(any(check["exists"] for check in checks), checks)
            calls = self.world.calls()
            first_check = next(index for index, call in enumerate(calls) if call[:2] == ["attestation", "verify"])
            downloads = [index for index, call in enumerate(calls) if call[:1] == ["api"] and "/assets/" in call[-1]]
            self.assertEqual(len(downloads), 4)
            self.assertLess(max(downloads), first_check)
            release = self.world.state["api"]["repos/jsell-rh/stego/releases/tags/" + self.world.tag]
            for asset in release["assets"]:
                downloaded = (self.world.bin / ("asset-" + str(asset["id"]))).read_bytes()
                if asset["name"] in [installer.verification.BINARY, "build.json"]:
                    self.assertEqual((self.output / asset["name"]).read_bytes(), downloaded)
            self.assertEqual((self.output / installer.verification.BINARY).read_bytes(), self.world.binary)
            self.assertEqual(json.loads(log)["artifact"]["sha256"], hashlib.sha256(self.world.binary).hexdigest())
            # A signature that fails leaves no result folder.
            self.output = self.root / "refused"
            self.world.state["signers"][installer.verification.PREDICATE] = ".github/workflows/other.yml"
            status, log = self.install()
            self.assertNotEqual(status, 0, log)
            self.assertIn("Compiler signature verification failed for " + installer.verification.BINARY, log)
            self.assertFalse(self.output.exists())


if __name__ == "__main__":
    unittest.main()
