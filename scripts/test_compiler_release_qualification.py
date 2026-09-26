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
        def with_tag(gh, path, directory, **keyword):
            if path.endswith("matching-refs/tags/" + self.tag):
                return json.dumps([{"ref": "refs/tags/" + self.tag,
                                    "object": {"type": "commit",
                                               "sha": self.revision}}]).encode()
            return self.api(None, path, directory, **keyword)
        with patch.object(qualification, "api", with_tag), \
                patch.object(qualification.installer, "install", self.install):
            result = self.qualify(verify_release=True)
        self.assertEqual(result["mode"], "verify-release")
        release = result["release"]
        self.assertEqual(release["tag_name"], self.tag)
        self.assertTrue(release["immutable"])
        self.assertTrue(release["notes_retain_signed_artifact_run"])
        self.assertTrue(release["notes_retain_compiler_sha256"])
        self.assertEqual(release["assets"][BINARY],
                         self.release["assets"][0]["digest"])

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
        def with_other_tag(gh, path, directory, **keyword):
            if path.endswith("matching-refs/tags/" + self.tag):
                return json.dumps([{"ref": "refs/tags/" + self.tag,
                                    "object": {"type": "commit",
                                               "sha": "b" * 40}}]).encode()
            return self.api(None, path, directory, **keyword)
        with patch.object(qualification, "api", with_other_tag), \
                patch.object(qualification.installer, "install", self.install):
            with self.assertRaises(CheckError):
                self.qualify(verify_release=True)

    def test_release_without_evidence_notes_fails(self):
        self.release["body"] = "no run identifiers"
        def with_tag(gh, path, directory, **keyword):
            if path.endswith("matching-refs/tags/" + self.tag):
                return json.dumps([{"ref": "refs/tags/" + self.tag,
                                    "object": {"type": "commit",
                                               "sha": self.revision}}]).encode()
            return self.api(None, path, directory, **keyword)
        with patch.object(qualification, "api", with_tag), \
                patch.object(qualification.installer, "install", self.install):
            with self.assertRaises(CheckError):
                self.qualify(verify_release=True)

    def test_tag_triggered_checks_run_qualifies_after_publication(self):
        self.checks_run["head_branch"] = self.tag
        def with_tag(gh, path, directory, **keyword):
            if path.endswith("matching-refs/tags/" + self.tag):
                return json.dumps([{"ref": "refs/tags/" + self.tag,
                                    "object": {"type": "commit",
                                               "sha": self.revision}}]).encode()
            return self.api(None, path, directory, **keyword)
        with patch.object(qualification, "api", with_tag), \
                patch.object(qualification.installer, "install", self.install):
            result = self.qualify(verify_release=True)
        self.assertEqual(result["compiler_checks"]["head_branch"], self.tag)


if __name__ == "__main__":
    unittest.main()
