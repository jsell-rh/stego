#!/usr/bin/env python3
"""Check image delivery boundaries without executing a compiler or image."""

import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("delivery", Path(__file__).with_name("application-images.py"))
delivery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(delivery)


class DeliveryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.images = self.root / "images"
        self.images.mkdir()
        self.output = self.root / "published"
        self.compiler = self.root / "compiler"
        self.compiler.write_bytes(b"compiler fixture; never executed")
        self.compiler_sha = hashlib.sha256(self.compiler.read_bytes()).hexdigest()
        self.token = self.root / "token"
        self.token.write_bytes(b"fixture.token.value")
        self.ca = self.root / "ca"
        self.ca.write_bytes(b"CA fixture")
        self.items = [{"name": name, "module": ".", "target": "out", "entrypoint": "service",
                       "build_record_sha256": "a" * 64, "image_record_sha256": "b" * 64,
                       "manifest_sha256": ("c" if name == "api" else "d") * 64} for name in ["api", "console"]]
        self.save_set()
        self.calls = []

    def save_set(self):
        data = json.dumps({"format": 1, "images": self.items}).encode()
        (self.images / "images.json").write_bytes(data)
        self.expected = hashlib.sha256(data).hexdigest()

    def publish(self, **kwargs):
        options = dict(images=self.images, expected=self.expected, source=self.root / "source",
                       compiler=self.compiler, compiler_sha256=self.compiler_sha, repository="registry.test/instance",
                       ca=self.ca, ca_sha256="e" * 64, output=self.output, token_file=self.token, username="serviceaccount")
        options.update(kwargs)
        return delivery.publish(**options)

    def command(self, args, cwd, env, **limits):
        self.calls.append(args[1:3])
        self.assertEqual(Path(args[0]).read_bytes(), self.compiler.read_bytes())
        self.assertEqual(Path(args[0]).stat().st_mode & 0o777, 0o700)
        self.assertEqual(set(env), {"PATH", "LANG", "LC_ALL", "HOME", "TMPDIR", "GOMAXPROCS", "GOMEMLIMIT"})
        self.assertEqual(limits, {"timeout": 310, "limit": 1 << 20})
        if args[1:3] != ["image", "publish"]:
            self.assertFalse(list(cwd.glob("credentials.json")))
            return b""
        self.assertEqual(self.calls[:4], [["build", "verify-source"], ["image", "verify"]] * 2)
        credential = Path(args[args.index("--credentials") + 1])
        self.assertEqual(credential.stat().st_mode & 0o777, 0o600)
        self.assertEqual(credential.read_bytes(), b'{"username":"serviceaccount","password":"fixture.token.value"}\n')
        work = Path(args[args.index("--work") + 1])
        work.mkdir()
        selected = next(item for item in self.items if item["name"] == work.name)
        repository = "registry.test/instance/" + work.name
        receipt = {"format": 1, "operation": "publish", "repository": repository,
                   "reference": repository + "@sha256:" + selected["manifest_sha256"],
                   "image_record_sha256": selected["image_record_sha256"], "registry_ca_sha256": "e" * 64,
                   "token_origins": [], "blob_origins": [], "image_contents_verified": True}
        (work / "registry.json").write_text(json.dumps(receipt))
        return b""

    def test_preflight_all_images_before_credentials_or_publication(self):
        with patch.object(delivery.records.verification.control, "command", side_effect=self.command):
            result = self.publish()
        self.assertEqual(set(result["images"]), {"api", "console"})
        self.assertTrue(all("@sha256:" in value for value in result["images"].values()))
        self.assertTrue((self.output / "source-check.json").is_file())
        self.assertEqual(list(self.output.glob(".private-*")), [])
        self.assertNotIn(b"fixture.token.value", (self.output / "publication.json").read_bytes())

    def test_wrong_compiler_or_set_digest_never_executes(self):
        for key in ["expected", "compiler_sha256"]:
            with self.subTest(key=key), patch.object(delivery.records.verification.control, "command") as command:
                with self.assertRaises(delivery.CheckError):
                    self.publish(**{key: "0" * 64, "output": self.root / key})
                command.assert_not_called()
                self.assertEqual(list((self.root / key).glob(".private-*")), [])

    def test_source_rejection_prevents_all_registry_calls(self):
        with patch.object(delivery.records.verification.control, "command", side_effect=delivery.CheckError("Source differs")) as command:
            with self.assertRaisesRegex(delivery.CheckError, "Source differs"):
                self.publish()
            self.assertEqual(command.call_count, 1)
        self.assertFalse((self.output / "publication.json").exists())
        self.assertEqual(list(self.output.glob(".private-*")), [])

    def test_failed_second_publication_keeps_receipt_but_no_complete_result_or_credentials(self):
        def fail_second(args, *other, **kwargs):
            if args[1:3] == ["image", "publish"] and args[args.index("--repository") + 1].endswith("/console"):
                raise delivery.CheckError("Registry failed")
            return self.command(args, *other, **kwargs)
        with patch.object(delivery.records.verification.control, "command", side_effect=fail_second):
            with self.assertRaisesRegex(delivery.CheckError, "Registry failed"):
                self.publish()
        self.assertTrue((self.output / "api/registry.json").is_file())
        self.assertFalse((self.output / "publication.json").exists())
        self.assertEqual(list(self.output.glob(".private-*")), [])

    def test_wrong_receipt_is_not_returned_as_a_verified_reference(self):
        def change(args, *other, **kwargs):
            self.command(args, *other, **kwargs)
            if args[1:3] == ["image", "publish"]:
                path = Path(args[args.index("--work") + 1]) / "registry.json"
                value = json.loads(path.read_text())
                value["reference"] = "registry.test/other@sha256:" + "f" * 64
                path.write_text(json.dumps(value))
        with patch.object(delivery.records.verification.control, "command", side_effect=change):
            with self.assertRaisesRegex(delivery.CheckError, "receipt differs"):
                self.publish()
        self.assertFalse((self.output / "publication.json").exists())

    def test_manifest_paths_duplicates_and_unknown_fields_are_rejected(self):
        original = copy.deepcopy(self.items)
        for key, value in [("name", "../escape"), ("target", "../out"), ("entrypoint", "etc"),
                           ("build_record_sha256", "short"), ("command", "execute")]:
            self.items = copy.deepcopy(original)
            self.items[0][key] = value
            self.save_set()
            with self.subTest(key=key), patch.object(delivery.records.verification.control, "command") as command:
                with self.assertRaises((delivery.CheckError, ValueError)):
                    self.publish(output=self.root / (key + "-result"))
                command.assert_not_called()
        self.items = [original[0], original[0]]
        self.save_set()
        with self.assertRaises(ValueError):
            self.publish()

    def test_projected_token_and_invalid_token_bytes(self):
        link = self.root / "projected"
        link.symlink_to(self.token)
        with patch.object(delivery.records.verification.control, "command", side_effect=self.command):
            self.publish(token_file=link)
        for value in [b"", b"token\n", b"token\r", b"token\x00", b"x" * ((8 << 10) + 1)]:
            self.token.write_bytes(value)
            with self.subTest(value=value[:16]), self.assertRaises(delivery.CheckError):
                delivery.token_credentials(self.token, "serviceaccount", self.root / "credentials")
        self.assertFalse((self.root / "credentials").exists())

    def stage_inputs(self):
        declared, policy = self.root / "declaration.json", self.root / "policy.json"
        declared.write_text(json.dumps({"format": 1, "images": [{k: item[k] for k in delivery.SELECTION} for item in self.items]}))
        selection = {key: "selected" for key in delivery.records.REUSABLE_FIELDS - {"module", "target", "entrypoint"}}
        selection["format"] = 2
        policy.write_text(json.dumps(selection))
        return declared, policy

    def stage_image(self, inputs, bundle, policy, target, gh, image):
        self.assertEqual(inputs, self.images / ("consumer-image-" + target.name + "-123-1") / "image-evidence/first")
        self.assertEqual(bundle, self.images / ("authenticated-records-" + target.name + "-123-1") / "provenance.jsonl")
        self.assertEqual(image, inputs / "oci")
        selected = json.loads(policy.read_text())
        self.assertEqual(selected["module"], ".")
        self.assertEqual(selected["target"], "out")
        target.mkdir()
        item = next(item for item in self.items if item["name"] == target.name)
        (target / "image.json").write_text(json.dumps({"manifest": {"sha256": item["manifest_sha256"]}}))
        return item

    def test_stage_uses_declared_targets_and_independent_policy(self):
        declared, policy = self.stage_inputs()
        with patch.object(delivery.records, "verify", side_effect=self.stage_image) as verify:
            result = delivery.stage(declared, policy, self.images, "123", "1", self.output, Path("/usr/bin/gh"))
        self.assertEqual(verify.call_count, 2)
        self.assertEqual(result, {"format": 1, "images": self.items})
        self.assertEqual(json.loads((self.output / "images.json").read_text()), result)

    def test_stage_failure_does_not_leave_a_partial_set(self):
        declared, policy = self.stage_inputs()
        def fail(inputs, bundle, policy, target, gh, image):
            if target.name == "console":
                raise delivery.CheckError("Signature rejected")
            return self.stage_image(inputs, bundle, policy, target, gh, image)
        with patch.object(delivery.records, "verify", side_effect=fail):
            with self.assertRaisesRegex(delivery.CheckError, "Signature rejected"):
                delivery.stage(declared, policy, self.images, "123", "1", self.output, Path("/usr/bin/gh"))
        self.assertFalse(self.output.exists())
        self.assertEqual(list(self.root.glob(".stego-images-*")), [])


if __name__ == "__main__":
    unittest.main()
