#!/usr/bin/env python3
"""Check application record policy without network access or executable builds."""

import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("records", Path(__file__).with_name("verify-application-records.py"))
records = importlib.util.module_from_spec(spec)
spec.loader.exec_module(records)


class RecordChecks(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.inputs = self.root / "inputs"
        self.inputs.mkdir()
        self.bundle = self.root / "bundle.jsonl"
        self.bundle.write_text("fixture bundle")
        self.output = self.root / "verified"
        self.policy_path = self.root / "policy.json"
        self.policy = {"format": 1, "repository": "example/consumer", "workflow": ".github/workflows/images.yml",
                       "reference": "refs/heads/main", "revision": "a" * 40, "compiler_revision": "b" * 40,
                       "compiler_sha256": "c" * 64, "application_revision": "d" * 40,
                       "module": ".", "target": "out", "entrypoint": "service", "trust_store_sha256": "e" * 64}
        compiler = {"revision": "b" * 40, "source_state": "clean", "vcs": "git", "goos": "linux", "goarch": "amd64"}
        artifact = {"sha256": "f" * 64, "size": 100}
        compiler_artifact = {"sha256": "c" * 64, "size": 200}
        self.build = {"format": 1, "source_revision": "d" * 40, "module": ".", "target": "out",
                      "build_compiler": compiler, "build_compiler_artifact": compiler_artifact, "artifact": artifact}
        self.image = {"format": 1, "packer_compiler": copy.deepcopy(compiler), "packer_compiler_artifact": copy.deepcopy(compiler_artifact),
                      "application": copy.deepcopy(artifact), "trust_store": {"sha256": "e" * 64, "size": 300}, "entrypoint": "service"}
        self.write()

    def write(self):
        self.policy_path.write_text(json.dumps(self.policy))
        build = json.dumps(self.build).encode()
        (self.inputs / "build.json").write_bytes(build)
        self.image["build_record_sha256"] = hashlib.sha256(build).hexdigest()
        (self.inputs / "image.json").write_text(json.dumps(self.image))

    def verify(self):
        return records.verify(self.inputs, self.bundle, self.policy_path, self.output, Path(sys.executable))

    def test_exact_consumer_policy_and_both_signatures_precede_output(self):
        calls = []
        def command(args, cwd, env, **limits):
            self.assertFalse(self.output.exists())
            self.assertNotEqual(cwd, self.inputs)
            for key, value in [("--repo", "example/consumer"), ("--signer-digest", "a" * 40),
                               ("--source-digest", "a" * 40), ("--source-ref", "refs/heads/main"),
                               ("--cert-identity", "https://github.com/example/consumer/.github/workflows/images.yml@refs/heads/main"),
                               ("--predicate-type", "https://slsa.dev/provenance/v1"),
                               ("--cert-oidc-issuer", "https://token.actions.githubusercontent.com"),
                               ("--hostname", "github.com"), ("--digest-alg", "sha256")]:
                self.assertEqual(args[args.index(key) + 1], value)
            self.assertIn("--deny-self-hosted-runners", args)
            self.assertEqual(limits, {"timeout": 90, "limit": 4 << 20})
            self.assertNotIn("HTTPS_PROXY", env)
            calls.append(Path(args[3]).name)
        with patch.object(records.verification.control, "command", side_effect=command):
            result = self.verify()
        self.assertEqual(calls, ["build.json", "image.json"])
        self.assertTrue(result["records_authenticated"])
        self.assertFalse(result["image_contents_checked"])
        self.assertFalse(result["registry_publication_checked"])
        self.assertEqual(self.output.stat().st_mode & 0o777, 0o700)
        self.assertEqual((self.output / "image.json").stat().st_mode & 0o777, 0o600)

    def test_either_signature_failure_leaves_no_output(self):
        for failure in [1, 2]:
            count = 0
            def authenticate(*args):
                nonlocal count
                count += 1
                if count == failure:
                    raise records.CheckError("Signature rejected")
            with self.subTest(failure=failure), patch.object(records.verification, "authenticate_subject", side_effect=authenticate):
                with self.assertRaisesRegex(records.CheckError, "Signature rejected"):
                    self.verify()
                self.assertFalse(self.output.exists())
                self.assertEqual(list(self.root.glob(".stego-records-*")), [])

    def test_private_snapshot_prevents_input_replacement(self):
        original = (self.inputs / "image.json").read_bytes()
        def authenticate(*args):
            (self.inputs / "image.json").write_bytes(b"replacement")
            self.policy_path.write_bytes(b"replacement")
            self.bundle.write_bytes(b"replacement")
        with patch.object(records.verification, "authenticate_subject", side_effect=authenticate):
            self.verify()
        self.assertEqual((self.output / "image.json").read_bytes(), original)
        self.assertEqual((self.output / "provenance.jsonl").read_text(), "fixture bundle")

    def test_signed_records_must_match_application_compiler_and_trust_selection(self):
        original_build, original_image = copy.deepcopy(self.build), copy.deepcopy(self.image)
        cases = [
            ("build", "source_revision", "f" * 40, "application selection"),
            ("build", "module", "console", "application selection"),
            ("build", "target", "other", "application selection"),
            ("build", "format", True, "record format"),
            ("image", "format", 2, "record format"),
            ("build", "build_compiler", {}, "compiler source"),
            ("image", "packer_compiler", dict(self.image["packer_compiler"], source_state="modified"), "compiler source"),
            ("image", "packer_compiler_artifact", {"sha256": "0" * 64}, "compiler bytes"),
            ("image", "application", {"sha256": "0" * 64}, "executable records"),
            ("image", "trust_store", {"sha256": "0" * 64}, "CA selection"),
            ("image", "entrypoint", "other", "entry point"),
        ]
        for selected, key, value, reason in cases:
            self.build, self.image = copy.deepcopy(original_build), copy.deepcopy(original_image)
            getattr(self, selected)[key] = value
            self.write()
            with self.subTest(key=key), patch.object(records.verification, "authenticate_subject"):
                with self.assertRaisesRegex(records.CheckError, reason):
                    self.verify()
                self.assertFalse(self.output.exists())

    def test_cross_record_digest_and_compiler_size_must_match(self):
        self.image["packer_compiler_artifact"]["size"] += 1
        self.write()
        with patch.object(records.verification, "authenticate_subject"):
            with self.assertRaisesRegex(records.CheckError, "compilers differ"):
                self.verify()
        self.image["packer_compiler_artifact"]["size"] -= 1
        self.write()
        self.image["build_record_sha256"] = "0" * 64
        (self.inputs / "image.json").write_text(json.dumps(self.image))
        with patch.object(records.verification, "authenticate_subject"):
            with self.assertRaisesRegex(records.CheckError, "does not bind"):
                self.verify()

    def test_policy_is_explicit_and_cannot_change_verifier_options(self):
        original = copy.deepcopy(self.policy)
        cases = [("format", True), ("extra", "unused"), ("repository", "https://github.com/example/repo"),
                 ("workflow", ".github/workflows/../../other.yml"), ("reference", "refs/pull/1/merge"),
                 ("reference", "refs/heads/main@{1}"), ("revision", "main"), ("compiler_sha256", ""),
                 ("module", "../escape"), ("target", "/absolute"), ("entrypoint", "etc")]
        for key, value in cases:
            self.policy = dict(original, **{key: value})
            self.policy_path.write_text(json.dumps(self.policy))
            with self.subTest(key=key), patch.object(records.verification, "authenticate_subject") as auth:
                with self.assertRaises(records.CheckError):
                    self.verify()
                auth.assert_not_called()
                self.assertFalse(self.output.exists())

    def test_links_duplicate_fields_and_large_records_are_rejected(self):
        path = self.inputs / "image.json"
        original = path.read_bytes()
        path.unlink()
        path.symlink_to(self.inputs / "build.json")
        with patch.object(records.verification, "authenticate_subject") as auth:
            with self.assertRaises(OSError):
                self.verify()
            auth.assert_not_called()
            path.unlink()
            path.write_bytes(b"x" * ((64 << 10) + 1))
            with self.assertRaisesRegex(records.CheckError, "bounded regular file"):
                self.verify()
            path.write_bytes(b'{"format":1,"format":1}')
            with self.assertRaisesRegex(records.CheckError, "repeated field"):
                self.verify()
            path.write_bytes(original)
        self.assertFalse(self.output.exists())

    def test_existing_output_is_not_changed(self):
        self.output.mkdir()
        marker = self.output / "marker"
        marker.write_text("preserve")
        with patch.object(records.verification, "authenticate_subject") as auth:
            with self.assertRaisesRegex(records.CheckError, "must be new"):
                self.verify()
            auth.assert_not_called()
        self.assertEqual(marker.read_text(), "preserve")


if __name__ == "__main__":
    unittest.main()
