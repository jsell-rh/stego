#!/usr/bin/env python3
"""Check consumer verification failure paths without a compiler build or network."""

import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("verification", Path(__file__).with_name("verify-compiler-artifact.py"))
verification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verification)


class VerificationChecks(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.inputs = self.root / "inputs"
        self.inputs.mkdir()
        self.output = self.root / "verified"
        self.bundle = self.root / "bundle.jsonl"
        self.bundle.write_text("fixture bundle")
        self.revision = "a" * 40
        self.binary = self.inputs / verification.BINARY
        self.binary.write_bytes(b"fixture compiler")
        self.record = {"format": 1, "source_revision": self.revision,
                       "artifact": {"name": verification.BINARY,
                                    "size": self.binary.stat().st_size,
                                    "sha256": verification.control.digest(self.binary)},
                       "version": {"build": {"goos": "linux", "goarch": "amd64",
                                    "revision": self.revision, "vcs": "git", "source_state": "clean"}}}
        self.write_record()

    def write_record(self):
        (self.inputs / "build.json").write_text(json.dumps(self.record))
        (self.inputs / "SHA256SUMS").write_text("".join(
            f"{verification.control.digest(self.inputs / name)}  {name}\n"
            for name in [verification.BINARY, "build.json"]))

    def verify(self, revision=None):
        return verification.verify(self.inputs, self.bundle, revision or self.revision,
                                   self.output, Path(sys.executable))

    def test_both_signatures_precede_publication_and_keep_exact_policy(self):
        calls = []
        def check(args, cwd, env, **limits):
            self.assertFalse(self.output.exists())
            self.assertNotEqual(cwd, self.inputs)
            for option, value in [("--repo", "jsell-rh/stego"),
                                  ("--source-digest", self.revision), ("--signer-digest", self.revision),
                                  ("--source-ref", "refs/heads/main"),
                                  ("--predicate-type", "https://slsa.dev/provenance/v1"),
                                  ("--hostname", "github.com"),
                                  ("--cert-identity", "https://github.com/jsell-rh/stego/.github/workflows/compiler-artifact.yml@refs/heads/main")]:
                self.assertEqual(args[args.index(option) + 1], value)
            self.assertIn("--deny-self-hosted-runners", args)
            self.assertEqual(limits, {"timeout": 90, "limit": 4 << 20})
            calls.append(Path(args[3]).name)
            return b"verified"
        with patch.object(verification.control, "command", side_effect=check):
            result = self.verify()
        self.assertEqual(calls, [verification.BINARY, "build.json"])
        self.assertEqual(result["artifact"], self.record["artifact"])
        self.assertEqual((self.output / verification.BINARY).read_bytes(), b"fixture compiler")
        self.assertEqual(self.output.stat().st_mode & 0o777, 0o700)
        self.assertTrue((self.output / "verified.json").is_file())

    def test_either_signature_failure_leaves_no_output(self):
        for failed_call in [1, 2]:
            calls = 0
            def authenticate(*args):
                nonlocal calls
                calls += 1
                if calls == failed_call:
                    raise verification.CheckError("Signature rejected")
            with self.subTest(failed_call=failed_call), patch.object(verification, "authenticate", side_effect=authenticate):
                with self.assertRaises(verification.CheckError):
                    self.verify()
                self.assertFalse(self.output.exists())
                self.assertEqual(list(self.root.glob(".stego-verify-*")), [])

    def test_input_mutation_cannot_replace_verified_snapshot(self):
        def authenticate(*args):
            self.binary.write_bytes(b"changed after capture")
            self.bundle.write_bytes(b"changed bundle")
        with patch.object(verification, "authenticate", side_effect=authenticate):
            self.verify()
        self.assertEqual((self.output / verification.BINARY).read_bytes(), b"fixture compiler")
        self.assertEqual((self.output / "provenance.jsonl").read_text(), "fixture bundle")

    def test_signed_record_must_match_selected_source_and_bytes(self):
        for key, value in [("source_revision", "b" * 40), ("format", 2), ("format", True),
                           ("artifact", dict(self.record["artifact"], sha256="0" * 64)),
                           ("version", {"build": {}}), ("version", None)]:
            original = self.record[key]
            self.record[key] = value
            self.write_record()
            with self.subTest(key=key, value=value), patch.object(verification, "authenticate"):
                with self.assertRaises(verification.CheckError):
                    self.verify()
                self.assertFalse(self.output.exists())
            self.record[key] = original

    def test_duplicate_record_fields_and_bad_checksums_fail(self):
        record = self.inputs / "build.json"
        record.write_text('{"format":1,"format":1}')
        with patch.object(verification, "authenticate"):
            with self.assertRaises(verification.CheckError):
                self.verify()
            self.write_record()
            (self.inputs / "SHA256SUMS").write_text("not the authenticated hashes")
            with self.assertRaises(verification.CheckError):
                self.verify()
        self.assertFalse(self.output.exists())

    def test_links_fifo_and_oversized_inputs_fail(self):
        link = self.root / "link"
        link.symlink_to(self.binary)
        fifo = self.root / "fifo"
        os.mkfifo(fifo)
        for path, limit in [(link, 100), (fifo, 100), (self.binary, 1)]:
            with self.subTest(path=path), self.assertRaises((OSError, verification.CheckError)):
                verification.regular_bytes(path, limit)

    def test_output_is_never_overwritten(self):
        self.output.mkdir()
        retained = self.output / "keep"
        retained.write_text("retained")
        with self.assertRaises(verification.CheckError):
            self.verify()
        self.assertEqual(retained.read_text(), "retained")

    def test_revision_and_environment_cannot_change_trust_policy(self):
        for revision in ["main", "A" * 40, "a" * 39]:
            with self.assertRaises(verification.CheckError):
                self.verify(revision)
        with patch.dict(os.environ, {"GH_HOST": "other.invalid", "GH_DEBUG": "api",
                                     "SSL_CERT_FILE": "/untrusted", "HTTPS_PROXY": "http://untrusted",
                                     "LD_PRELOAD": "/untrusted", "PATH": "/untrusted", "GH_TOKEN": "auth"}):
            env = verification.verifier_environment()
        self.assertEqual(env["GH_HOST"], "github.com")
        self.assertEqual(env["PATH"], "/usr/bin:/bin")
        self.assertEqual(env["GH_TOKEN"], "auth")
        self.assertFalse({"GH_DEBUG", "SSL_CERT_FILE", "HTTPS_PROXY", "LD_PRELOAD"} & env.keys())


if __name__ == "__main__":
    unittest.main()
