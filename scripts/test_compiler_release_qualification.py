#!/usr/bin/env python3
"""Check compiler release qualification with fixed records and no network."""

import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "qualification", Path(__file__).with_name("qualify-compiler-release.py"))
qualification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(qualification)
CheckError = qualification.CheckError
BINARY = qualification.BINARY


class QualificationChecks(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.output = self.root / "qualification"
        self.gh = self.root / "gh"
        self.gh.write_text("#!/bin/sh\nexit 0\n")
        self.gh.chmod(0o755)
        self.revision = "a" * 40
        self.tag = "compiler-" + self.revision
        self.checks_run = self.run_record(101, qualification.CHECKS_WORKFLOW, "main")
        self.artifact_run = self.run_record(202, qualification.ARTIFACT_WORKFLOW, "main")
        self.jobs = {"checks": self.job_records(qualification.CHECKS_JOB_NAMES),
                     "artifact": self.job_records(qualification.ARTIFACT_JOB_NAMES)}
        self.artifacts = self.artifact_records()
        self.release = self.release_record()
        self.signing_runs = {"bundle": [self.artifact_run["id"]],
                             "store": [self.artifact_run["id"]]}

    def run_record(self, identity, path, branch):
        return {"id": identity, "path": path, "event": "workflow_dispatch",
                "head_branch": branch, "head_sha": self.revision,
                "status": "completed", "conclusion": "success"}

    def job_records(self, names):
        return [{"name": name, "conclusion": "success"} for name in names]

    def artifact_records(self):
        found = []
        for prefix, size in [("compiler-linux-amd64-", 42046207),
                             ("compiler-attestation-", 6893),
                             ("compiler-provenance-", 20141390)]:
            found.append({"name": prefix + self.revision, "size_in_bytes": size,
                          "digest": "sha256:" + "0" * 64, "expired": False})
        return found

    def release_record(self):
        binary = b"fixture compiler; do not execute"
        record = {"format": 1, "source_revision": self.revision,
                  "artifact": {"name": BINARY, "size": len(binary),
                               "sha256": hashlib.sha256(binary).hexdigest()},
                  "version": {"build": {"goos": "linux", "goarch": "amd64",
                                        "revision": self.revision, "vcs": "git",
                                        "source_state": "clean"}}}
        build_bytes = json.dumps(record).encode()
        sha256sums = "".join(hashlib.sha256(data).hexdigest() + "  " + name + "\n"
                             for name, data in [(BINARY, binary),
                                                ("build.json", build_bytes)]).encode()
        files = {BINARY: binary, "build.json": build_bytes,
                 "SHA256SUMS": sha256sums, "provenance.jsonl": b"fixture signatures"}
        assets = []
        for identity, (name, data) in enumerate(files.items(), 300):
            assets.append({"id": identity, "name": name, "size": len(data),
                           "digest": "sha256:" + hashlib.sha256(data).hexdigest(),
                           "state": "uploaded"})
        notes = ("Compiler release from main at " + self.revision
                 + ". Signed artifact run " + str(self.artifact_run["id"])
                 + ". Binary sha256: " + files[BINARY].hex()[:0]
                 + hashlib.sha256(binary).hexdigest())
        return {"id": 999, "tag_name": self.tag, "target_commitish": self.revision,
                "draft": False, "prerelease": False, "immutable": True,
                "assets": assets, "body": notes}

    def api(self, gh, path, directory, *, limit=4 << 20):
        runs = {"repos/" + qualification.REPOSITORY + "/actions/runs?head_sha="
                + self.revision + "&per_page=" + str(qualification.RUN_PAGE):
                json.dumps({"total_count": 2,
                            "workflow_runs": [self.checks_run, self.artifact_run]}),
                "repos/" + qualification.REPOSITORY + "/actions/runs/101/jobs?per_page="
                + str(qualification.RUN_PAGE):
                json.dumps({"total_count": len(self.jobs["checks"]),
                            "jobs": self.jobs["checks"]}),
                "repos/" + qualification.REPOSITORY + "/actions/runs/202/jobs?per_page="
                + str(qualification.RUN_PAGE):
                json.dumps({"total_count": len(self.jobs["artifact"]),
                            "jobs": self.jobs["artifact"]}),
                "repos/" + qualification.REPOSITORY + "/actions/runs/202/artifacts?per_page="
                + str(qualification.RUN_PAGE):
                json.dumps({"total_count": len(self.artifacts),
                            "artifacts": self.artifacts}),
                "repos/" + qualification.REPOSITORY + "/git/matching-refs/tags/"
                + self.tag: json.dumps([]),
                "repos/" + qualification.REPOSITORY + "/releases/tags/" + self.tag:
                json.dumps(self.release)}
        if path in runs:
            return runs[path].encode()
        raise AssertionError("Unexpected API path: " + path)

    def install(self, revision, package, gh):
        package.mkdir(parents=True)
        binary = b"fixture compiler; do not execute"
        record = {"format": 1, "source_revision": self.revision,
                  "artifact": {"name": BINARY, "size": len(binary),
                               "sha256": hashlib.sha256(binary).hexdigest()},
                  "version": {"build": {"goos": "linux", "goarch": "amd64",
                                        "revision": self.revision, "vcs": "git",
                                        "source_state": "clean"}}}
        build_bytes = json.dumps(record).encode()
        (package / BINARY).write_bytes(binary)
        (package / "build.json").write_bytes(build_bytes)
        (package / "SHA256SUMS").write_text(
            "".join(hashlib.sha256(data).hexdigest() + "  " + name + "\n"
                    for name, data in [(BINARY, binary), ("build.json", build_bytes)]))
        (package / "provenance.jsonl").write_bytes(b"fixture signatures")
        return {"format": 1, "repository": qualification.REPOSITORY,
                "source_revision": self.revision,
                "artifact": {"name": BINARY, "size": len(binary),
                             "sha256": hashlib.sha256(binary).hexdigest()},
                "build_record_sha256": hashlib.sha256(build_bytes).hexdigest(),
                "bundle_sha256": hashlib.sha256(b"fixture signatures").hexdigest()}

    def attested_runs(self, gh, path, revision, bundle=None):
        self.assertEqual(revision, self.revision)
        self.assertIn(path.name, [BINARY, "build.json"])
        return set(self.signing_runs["bundle" if bundle is not None else "store"])

    def with_tag(self, gh, path, directory, **keyword):
        if path.endswith("matching-refs/tags/" + self.tag):
            return json.dumps([{"ref": "refs/tags/" + self.tag,
                                "object": {"type": "commit",
                                           "sha": self.revision}}]).encode()
        return self.api(None, path, directory, **keyword)

    def verify_release(self):
        with patch.object(qualification, "api", self.with_tag), \
                patch.object(qualification.installer, "install", self.install), \
                patch.object(qualification, "attested_runs", self.attested_runs):
            return self.qualify(verify_release=True)

    def qualify(self, **keyword):
        settings = {"revision": self.revision, "output": self.output,
                    "gh": self.gh}
        settings.update(keyword)
        return qualification.qualify(**settings)

    def test_prepare_qualification_records_both_run_sets(self):
        with patch.object(qualification, "api", self.api):
            result = self.qualify()
        self.assertEqual(result["mode"], "prepare")
        self.assertEqual(result["compiler_checks"]["run_id"], 101)
        self.assertEqual(result["signed_artifact"]["run_id"], 202)
        self.assertEqual(len(result["compiler_checks"]["jobs"]),
                         len(qualification.CHECKS_JOB_NAMES))
        self.assertEqual(len(result["signed_artifact"]["jobs"]),
                         len(qualification.ARTIFACT_JOB_NAMES))
        self.assertEqual(len(result["signed_artifact"]["artifacts"]), 3)
        self.assertIsNone(result["release"])
        self.assertTrue((self.output / "qualification.json").exists())

    def test_release_verification_cross_checks_assets_and_notes(self):
        with self.subTest("ST-DIS-UC08-R03-T01 Release verification cross-checks every asset digest and the release notes"):
            result = self.verify_release()
            self.assertEqual(result["mode"], "verify-release")
            release = result["release"]
            self.assertEqual(release["tag_name"], self.tag)
            self.assertTrue(release["immutable"])
            self.assertTrue(release["notes_retain_signed_artifact_run"])
            self.assertTrue(release["notes_retain_compiler_sha256"])
            self.assertEqual(release["assets"][BINARY],
                             self.release["assets"][0]["digest"])
            self.assertEqual(release["signatures_name_signed_artifact_run"], 202)
            self.assertTrue(result["signed_artifact"]["artifacts_retained"])

    def test_missing_checks_run_fails(self):
        def without_checks(gh, path, directory, **keyword):
            if "actions/runs?head_sha=" in path:
                return json.dumps({"total_count": 1,
                                   "workflow_runs": [self.artifact_run]}).encode()
            return self.api(None, path, directory, **keyword)
        with patch.object(qualification, "api", without_checks):
            with self.assertRaises(CheckError):
                self.qualify()

    def test_incomplete_checks_job_set_fails(self):
        with self.subTest("ST-DIS-UC08-R02-T01 Qualification fails when a required checks job is missing"):
            self.jobs["checks"] = self.jobs["checks"][:-1]
            with patch.object(qualification, "api", self.api):
                with self.assertRaises(CheckError):
                    self.qualify()

    def test_skipped_provenance_job_fails(self):
        for job in self.jobs["artifact"]:
            if job["name"] == "provenance":
                job["conclusion"] = "skipped"
        with patch.object(qualification, "api", self.api):
            with self.assertRaises(CheckError):
                self.qualify()

    def test_failed_compiler_job_fails(self):
        self.jobs["checks"][2]["conclusion"] = "failure"
        with patch.object(qualification, "api", self.api):
            with self.assertRaises(CheckError):
                self.qualify()

    def test_missing_provenance_artifact_fails(self):
        self.artifacts = [a for a in self.artifacts
                          if not a["name"].startswith("compiler-provenance-")]
        with patch.object(qualification, "api", self.api):
            with self.assertRaises(CheckError):
                self.qualify()

    def test_extra_artifact_fails(self):
        with self.subTest("ST-STD-TRUST-R01-T02 Qualification fails when the signed artifact run holds a second artifact"):
            self.artifacts.append({"name": "generator-rest-api-" + self.revision,
                                   "size_in_bytes": 1024,
                                   "digest": "sha256:" + "0" * 64, "expired": False})
            with patch.object(qualification, "api", self.api):
                with self.assertRaisesRegex(CheckError, "unknown or repeated artifact"):
                    self.qualify()

    def test_expired_artifact_fails_prepare_mode(self):
        for artifact in self.artifacts:
            artifact["expired"] = True
        with patch.object(qualification, "api", self.api):
            with self.assertRaises(CheckError):
                self.qualify()

    def test_existing_tag_fails_prepare_mode(self):
        def with_tag(gh, path, directory, **keyword):
            if path.endswith("matching-refs/tags/" + self.tag):
                return json.dumps([{"ref": "refs/tags/" + self.tag,
                                    "object": {"type": "commit",
                                               "sha": self.revision}}]).encode()
            return self.api(None, path, directory, **keyword)
        with patch.object(qualification, "api", with_tag):
            with self.assertRaises(CheckError):
                self.qualify()

    def test_tag_pointing_elsewhere_fails_release_verification(self):
        with self.subTest("ST-DIS-UC08-R01-T01 Verification fails when the release tag points at another commit"):
            def with_other_tag(gh, path, directory, **keyword):
                if path.endswith("matching-refs/tags/" + self.tag):
                    return json.dumps([{"ref": "refs/tags/" + self.tag,
                                        "object": {"type": "commit",
                                                   "sha": "b" * 40}}]).encode()
                return self.api(None, path, directory, **keyword)
            with patch.object(qualification, "api", with_other_tag), \
                    patch.object(qualification.installer, "install", self.install), \
                    patch.object(qualification, "attested_runs", self.attested_runs):
                with self.assertRaises(CheckError):
                    self.qualify(verify_release=True)

    def test_release_without_evidence_notes_fails(self):
        self.release["body"] = "no run identifiers"
        with self.assertRaises(CheckError):
            self.verify_release()

    def test_tag_triggered_checks_run_qualifies_after_publication(self):
        self.checks_run["head_branch"] = self.tag
        result = self.verify_release()
        self.assertEqual(result["compiler_checks"]["head_branch"], self.tag)

    def test_release_verification_proves_removed_artifacts_by_signature(self):
        with self.subTest("ST-DIS-UC08-R03-T02 Release verification proves the signed artifact run by the release signatures after its artifacts expire"):
            self.artifacts = []
            result = self.verify_release()
            self.assertEqual(result["signed_artifact"]["artifacts"], [])
            self.assertFalse(result["signed_artifact"]["artifacts_retained"])
            self.assertEqual(result["release"]["signatures_name_signed_artifact_run"], 202)

    def test_signature_from_another_run_fails_release_verification(self):
        with self.subTest("ST-DIS-UC08-R01-T02 Release verification fails when a release signature names another signed artifact run"):
            for source in ["bundle", "store"]:
                for artifacts in [self.artifact_records(), []]:
                    self.artifacts = artifacts
                    self.signing_runs = {"bundle": [202], "store": [202]}
                    self.signing_runs[source] = [303]
                    with self.assertRaisesRegex(CheckError, "does not name the signed artifact run"):
                        self.verify_release()
                    self.assertFalse(self.output.exists())

    def test_missing_signed_artifacts_fail(self):
        with self.subTest("ST-DIS-UC08-R02-T02 Qualification fails when the signed artifact run holds no artifacts or only some of them"):
            self.artifacts = []
            with patch.object(qualification, "api", self.api):
                with self.assertRaises(CheckError) as refused:
                    self.qualify()
            self.assertEqual(str(refused.exception),
                             "The signed artifact run is missing required artifacts: "
                             + ", ".join(prefix + self.revision for prefix in
                                         ["compiler-attestation-", "compiler-linux-amd64-",
                                          "compiler-provenance-"]))
            self.artifacts = [a for a in self.artifact_records()
                              if not a["name"].startswith("compiler-provenance-")]
            with self.assertRaises(CheckError) as refused:
                self.verify_release()
            self.assertEqual(str(refused.exception),
                             "The signed artifact run is missing required artifacts: "
                             "compiler-provenance-" + self.revision)
            self.assertFalse(self.output.exists())


class SignatureRunChecks(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.path = self.root / BINARY
        self.path.write_bytes(b"fixture compiler; do not execute")
        self.revision = "a" * 40

    def result(self, run, revision=None):
        uri = ("https://github.com/" + qualification.REPOSITORY + "/actions/runs/"
               + str(run) + "/attempts/1") if run is not None else None
        certificate = {"sourceRepositoryDigest": revision or self.revision}
        if uri is not None:
            certificate["runInvocationURI"] = uri
        return {"verificationResult": {"signature": {"certificate": certificate}}}

    def runs(self, output, bundle=None):
        calls = []
        def command(args, cwd, env, **keyword):
            calls.append(args)
            if isinstance(output, Exception):
                raise output
            return json.dumps(output).encode()
        with patch.object(qualification.control, "command", command):
            runs = qualification.attested_runs(self.root / "gh", self.path,
                                               self.revision, bundle)
        return runs, calls

    def test_signature_runs(self):
        with self.subTest("ST-DIS-UC08-R01-T02 Release verification fails when a release signature names another signed artifact run"):
            runs, calls = self.runs([self.result(202), self.result(404)])
            self.assertEqual(runs, {202, 404})
            self.assertNotIn("--bundle", calls[0])
            self.assertIn("--source-digest", calls[0])
            self.assertEqual(calls[0][calls[0].index("--source-digest") + 1], self.revision)
            _, calls = self.runs([self.result(202)], self.root / "provenance.jsonl")
            self.assertEqual(calls[0][calls[0].index("--bundle") + 1],
                             str(self.root / "provenance.jsonl"))
            for refused in [[], {}, [self.result(None)], [self.result(202, "b" * 40)],
                            [{"verificationResult": {}}], ["text"],
                            CheckError("not signed")]:
                with self.assertRaises(CheckError):
                    self.runs(refused)
            uri = "https://github.com/other/stego/actions/runs/202/attempts/1"
            other = self.result(202)
            other["verificationResult"]["signature"]["certificate"]["runInvocationURI"] = uri
            with self.assertRaises(CheckError):
                self.runs([other])


if __name__ == "__main__":
    unittest.main()
