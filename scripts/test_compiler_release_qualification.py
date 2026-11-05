#!/usr/bin/env python3
"""Check compiler release qualification with fixed records and no network."""

from datetime import datetime, timedelta, timezone
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


spec = importlib.util.spec_from_file_location(
    "qualification", Path(__file__).with_name("qualify-compiler-release.py"))
qualification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(qualification)
CheckError = qualification.CheckError
BINARY = qualification.BINARY

# gh 2.96.0 attestation verify --format json for the 94d285d7 release, trimmed
# to the fields the checks read. signature_result puts other values in a copy.
SIGNATURE_RESULT = json.loads("""[{"verificationResult": {
  "mediaType": "application/vnd.dev.sigstore.verificationresult+json;version=0.1",
  "signature": {"certificate": {
    "subjectAlternativeName": "https://github.com/jsell-rh/stego/.github/workflows/compiler-artifact.yml@refs/heads/main",
    "issuer": "https://token.actions.githubusercontent.com",
    "githubWorkflowTrigger": "workflow_dispatch",
    "githubWorkflowSHA": "94d285d7a13466d4bb53a3ddf8737888f29fea5a",
    "githubWorkflowRef": "refs/heads/main",
    "buildSignerURI": "https://github.com/jsell-rh/stego/.github/workflows/compiler-artifact.yml@refs/heads/main",
    "buildSignerDigest": "94d285d7a13466d4bb53a3ddf8737888f29fea5a",
    "runnerEnvironment": "github-hosted",
    "sourceRepositoryURI": "https://github.com/jsell-rh/stego",
    "sourceRepositoryDigest": "94d285d7a13466d4bb53a3ddf8737888f29fea5a",
    "sourceRepositoryRef": "refs/heads/main",
    "buildTrigger": "workflow_dispatch",
    "runInvocationURI": "https://github.com/jsell-rh/stego/actions/runs/36128796855/attempts/1"}},
  "statement": {"predicateType": "https://slsa.dev/provenance/v1"}}}]""")


def signature_result(revision, run, attempt=1):
    result = json.loads(json.dumps(SIGNATURE_RESULT[0]))
    certificate = result["verificationResult"]["signature"]["certificate"]
    for name in ["githubWorkflowSHA", "buildSignerDigest", "sourceRepositoryDigest"]:
        certificate[name] = revision
    certificate["runInvocationURI"] = ("https://github.com/" + qualification.REPOSITORY
                                       + "/actions/runs/" + str(run)
                                       + "/attempts/" + str(attempt))
    return result


def signer_policy(gh, path, revision, bundle):
    source = [] if bundle is None else ["--bundle", str(bundle)]
    return [str(gh), "attestation", "verify", str(path), *source,
            "--hostname", "github.com", "--repo", "jsell-rh/stego",
            "--signer-digest", revision,
            "--cert-identity",
            "https://github.com/jsell-rh/stego/.github/workflows/compiler-artifact.yml@refs/heads/main",
            "--cert-oidc-issuer", "https://token.actions.githubusercontent.com",
            "--source-ref", "refs/heads/main", "--source-digest", revision,
            "--predicate-type", "https://slsa.dev/provenance/v1", "--digest-alg", "sha256",
            "--deny-self-hosted-runners", "--format", "json"]


DECISION_PREDICATE = "https://github.com/jsell-rh/stego/assure/release-decision/v1"


def decision_result(revision, **predicate):
    # A verified release decision, in the shape of gh attestation verify.
    result = json.loads(json.dumps(SIGNATURE_RESULT[0]))
    certificate = result["verificationResult"]["signature"]["certificate"]
    for name in ["subjectAlternativeName", "buildSignerURI"]:
        certificate[name] = ("https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml"
                             "@refs/heads/main")
    for name in ["githubWorkflowSHA", "buildSignerDigest", "sourceRepositoryDigest"]:
        certificate[name] = revision
    document = {"commit": revision, "kind": "release", "passed": True}
    document.update(predicate)
    result["verificationResult"]["statement"] = {"predicateType": DECISION_PREDICATE,
                                                 "predicate": document}
    return result


def decision_policy(gh, path, revision):
    return [str(gh), "attestation", "verify", str(path),
            "--hostname", "github.com", "--repo", "jsell-rh/stego",
            "--signer-digest", revision,
            "--cert-identity",
            "https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml@refs/heads/main",
            "--cert-oidc-issuer", "https://token.actions.githubusercontent.com",
            "--source-ref", "refs/heads/main", "--source-digest", revision,
            "--predicate-type", DECISION_PREDICATE, "--digest-alg", "sha256",
            "--deny-self-hosted-runners", "--format", "json"]


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
        self.newer_runs = []
        self.jobs = {"checks": self.job_records(qualification.CHECKS_JOB_NAMES),
                     "artifact": self.job_records(qualification.ARTIFACT_JOB_NAMES)}
        self.artifacts = self.artifact_records()
        self.release = self.release_record()
        self.signing_runs = {(name, source): self.artifact_run["id"]
                             for name in [BINARY, "build.json"]
                             for source in ["bundle", "store"]}
        self.signature_calls = []
        self.decisions = [decision_result(self.revision)]
        # Every qualification first verifies the release decision through
        # GitHub CLI. The fixture answers for the decision and the signatures.
        command = patch.object(qualification.control, "command", self.signature_command)
        command.start()
        self.addCleanup(command.stop)

    def run_record(self, identity, path, branch):
        return {"id": identity, "run_attempt": 1, "path": path, "event": "workflow_dispatch",
                "head_branch": branch, "head_sha": self.revision,
                "status": "completed", "conclusion": "success",
                "updated_at": "2026-09-25T11:30:00Z"}

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
                json.dumps({"total_count": 2 + len(self.newer_runs),
                            "workflow_runs": self.newer_runs
                            + [self.checks_run, self.artifact_run]}),
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

    def signature_command(self, args, cwd, env, **keyword):
        # Stands in for GitHub CLI: one signature per file and source, and the
        # release decisions attested at the commit.
        path = Path(args[3])
        if DECISION_PREDICATE in args:
            self.assertEqual(path.read_bytes(), self.revision.encode())
            self.assertEqual(args, decision_policy(self.gh, path, self.revision))
            if not self.decisions:
                raise CheckError("no attestation")
            return json.dumps(self.decisions).encode()
        self.signature_calls.append(args)
        source = "bundle" if "--bundle" in args else "store"
        self.assertEqual(args, signer_policy(self.gh, path, self.revision,
                                             path.parent / "provenance.jsonl"
                                             if source == "bundle" else None))
        signers = self.signing_runs[(path.name, source)]
        signers = signers if isinstance(signers, list) else [signers]
        return json.dumps([signature_result(self.revision, *(signer if isinstance(signer, tuple)
                                                            else (signer, 1)))
                           for signer in signers]).encode()

    def with_tag(self, gh, path, directory, **keyword):
        if path.endswith("matching-refs/tags/" + self.tag):
            return json.dumps([{"ref": "refs/tags/" + self.tag,
                                "object": {"type": "commit",
                                           "sha": self.revision}}]).encode()
        return self.api(None, path, directory, **keyword)

    def verify_release(self):
        with patch.object(qualification, "api", self.with_tag), \
                patch.object(qualification.installer, "install", self.install), \
                patch.object(qualification.control, "command", self.signature_command):
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
        with self.subTest("In process: Release verification cross-checks every asset digest and the release notes"):
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

    def test_qualification_requires_a_passed_release_decision(self):
        with patch.object(qualification, "api", self.api):
            result = self.qualify()
        self.assertEqual(result["release_decision"]["commit"], self.revision)
        self.assertTrue(result["release_decision"]["passed"])
        self.output.joinpath("qualification.json").unlink()
        self.output.rmdir()
        other = decision_result(self.revision)
        other["verificationResult"]["signature"]["certificate"]["sourceRepositoryDigest"] = "b" * 40
        for decisions, message in [
                ([], "No release decision attested at"),
                ([decision_result(self.revision, passed=False)], "No passed release decision"),
                ([decision_result(self.revision, commit="b" * 40)], "No passed release decision"),
                ([decision_result(self.revision, kind="change")], "No passed release decision"),
                ([other], "names another commit"),
                ([{"verificationResult": {}}], "no certificate or statement")]:
            with self.subTest(message=message, decisions=len(decisions)):
                self.decisions = decisions
                with patch.object(qualification, "api", self.api):
                    with self.assertRaisesRegex(CheckError, message):
                        self.qualify()
                with self.assertRaisesRegex(CheckError, message):
                    self.verify_release()
                self.assertFalse(self.output.exists())

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
        with self.subTest("In process: Qualification fails when a required checks job is missing"):
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
        with self.subTest("In process: Verification fails when the release tag points at another commit"):
            def with_other_tag(gh, path, directory, **keyword):
                if path.endswith("matching-refs/tags/" + self.tag):
                    return json.dumps([{"ref": "refs/tags/" + self.tag,
                                        "object": {"type": "commit",
                                                   "sha": "b" * 40}}]).encode()
                return self.api(None, path, directory, **keyword)
            with patch.object(qualification, "api", with_other_tag), \
                    patch.object(qualification.installer, "install", self.install), \
                    patch.object(qualification.control, "command", self.signature_command):
                with self.assertRaises(CheckError):
                    self.qualify(verify_release=True)

    def test_release_without_evidence_notes_fails(self):
        notes = self.release["body"]
        digest = hashlib.sha256(b"fixture compiler; do not execute").hexdigest()
        for body in ["no run identifiers", notes.replace("run 202", "run"),
                     notes.replace(digest, "")]:
            with self.subTest(body=body):
                self.release["body"] = body
                with self.assertRaisesRegex(CheckError, "release notes do not retain"):
                    self.verify_release()

    def test_bundle_with_two_signing_runs_fails(self):
        self.signing_runs[(BINARY, "bundle")] = [202, 203]
        with self.assertRaisesRegex(CheckError, "exactly one signing run"):
            self.verify_release()

    def test_tag_triggered_checks_run_qualifies_after_publication(self):
        self.checks_run["head_branch"] = self.tag
        result = self.verify_release()
        self.assertEqual(result["compiler_checks"]["head_branch"], self.tag)

    def test_release_verification_proves_removed_artifacts_by_signature(self):
        with self.subTest("In process: Release verification proves the signed artifact run by the release signatures after its artifacts expire"):
            self.artifacts = []
            result = self.verify_release()
            self.assertEqual(result["signed_artifact"]["artifacts"], [])
            self.assertFalse(result["signed_artifact"]["artifacts_retained"])
            self.assertEqual(result["release"]["signatures_name_signed_artifact_run"], 202)

    def test_signature_from_another_run_fails_release_verification(self):
        with self.subTest("In process: Release verification fails when a release signature names another signed artifact run"):
            for key in list(self.signing_runs):
                for artifacts in [self.artifact_records(), []]:
                    with self.subTest(file=key[0], source=key[1], artifacts=len(artifacts)):
                        self.artifacts = artifacts
                        self.signing_runs = dict.fromkeys(self.signing_runs, 202)
                        self.signing_runs[key] = 303
                        with self.assertRaisesRegex(
                                CheckError, "does not name the signed artifact run"):
                            self.verify_release()
                        self.assertFalse(self.output.exists())
            self.artifacts = self.artifact_records()
            self.signing_runs = dict.fromkeys(self.signing_runs, (202, 1))
            self.artifact_run["run_attempt"] = 2
            with self.assertRaisesRegex(CheckError, "signed by attempt 1 of CI run 202"):
                self.verify_release()
            self.assertFalse(self.output.exists())

    def test_release_verification_takes_the_run_its_signatures_name(self):
        with self.subTest("In process: Release verification proves the signed artifact run by the release signatures after its artifacts expire"):
            newer = self.run_record(203, qualification.ARTIFACT_WORKFLOW, "main")
            self.newer_runs = [newer]
            self.artifacts = []
            result = self.verify_release()
            self.assertEqual(result["signed_artifact"]["run_id"], 202)
            self.assertEqual(result["release"]["signing_run"], {"id": 202, "attempt": 1})
            self.output.joinpath("qualification.json").unlink()
            self.output.rmdir()
            self.signing_runs = dict.fromkeys(self.signing_runs, 404)
            self.release["body"] += " 404"
            with self.assertRaisesRegex(CheckError, "No signed artifact run exists"):
                self.verify_release()

    def test_release_verification_uses_the_installer_signer_policy(self):
        self.verify_release()
        checked = sorted((Path(args[3]).name, "--bundle" in args)
                         for args in self.signature_calls)
        self.assertEqual(checked, [("build.json", False), ("build.json", True),
                                   (BINARY, False), (BINARY, True)])
        installer_calls = []
        def record(args, cwd, env, **keyword):
            installer_calls.append(args)
            return b"[]"
        path = Path(self.signature_calls[0][3])
        with patch.object(qualification.installer.verification.control, "command", record):
            qualification.installer.verification.authenticate(
                self.gh, path, path.parent / "provenance.jsonl", self.revision, {})
        bundled = [args for args in self.signature_calls if args[3] == str(path)
                   and "--bundle" in args]
        self.assertEqual(installer_calls, bundled)

    def test_missing_signed_artifacts_fail(self):
        with self.subTest("In process: Qualification fails when the signed artifact run holds no artifacts or only some of them"):
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
            # An empty list before the retention time is not expiry.
            self.artifacts = []
            recent = datetime.now(timezone.utc) - timedelta(days=6, hours=23)
            self.artifact_run["updated_at"] = recent.strftime("%Y-%m-%dT%H:%M:%SZ")
            with self.assertRaisesRegex(CheckError, "missing required artifacts"):
                self.verify_release()
            for invalid in [None, "2026-09-25", 7]:
                self.artifact_run["updated_at"] = invalid
                with self.assertRaisesRegex(CheckError, "run time is invalid"):
                    self.verify_release()
            self.assertFalse(self.output.exists())


class SignatureRunChecks(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.path = self.root / BINARY
        self.path.write_bytes(b"fixture compiler; do not execute")
        self.revision = "94d285d7a13466d4bb53a3ddf8737888f29fea5a"

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

    def test_real_signature_result_names_its_run(self):
        runs, calls = self.runs(SIGNATURE_RESULT)
        self.assertEqual(runs, {(36128796855, 1)})
        self.assertEqual(calls, [signer_policy(self.root / "gh", self.path,
                                               self.revision, None)])
        bundle = self.root / "provenance.jsonl"
        _, calls = self.runs(SIGNATURE_RESULT, bundle)
        self.assertEqual(calls, [signer_policy(self.root / "gh", self.path,
                                               self.revision, bundle)])
        runs, _ = self.runs([signature_result(self.revision, 202),
                             signature_result(self.revision, 404)])
        self.assertEqual(runs, {(202, 1), (404, 1)})

    def test_invalid_signature_results_fail(self):
        no_run = signature_result(self.revision, 202)
        del no_run["verificationResult"]["signature"]["certificate"]["runInvocationURI"]
        other_repository = signature_result(self.revision, 202)
        other_repository["verificationResult"]["signature"]["certificate"][
            "runInvocationURI"] = "https://github.com/other/stego/actions/runs/202/attempts/1"
        for refused in [[], {}, [no_run], [signature_result("b" * 40, 202)],
                        [other_repository], [{"verificationResult": {}}], ["text"],
                        CheckError("not signed")]:
            with self.subTest(refused=refused):
                with self.assertRaises(CheckError):
                    self.runs(refused)


# The qualification script as a process. GitHub stands behind the fake gh of test_release_publishing.py, which
# answers from fixed records: the release decision and the signatures (the local stand-in for the provider's
# trusted signer), the runs, their jobs and artifacts, the tags and the releases. The publish-release job of
# proof-run.yml publishes the release first, as CI does. No test reaches GitHub.
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
COMPILER_PREDICATE = "https://slsa.dev/provenance/v1"


@unittest.skipUnless(publishing is not None and sys.platform == "linux" and shutil.which("jq") and shutil.which("unzip"),
                     "the publish job and the script run as Linux processes with jq and unzip")
class QualificationAsAProcess(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.count = 0

    def world(self, published=False):
        self.count += 1
        folder = self.root / ("world-" + str(self.count))
        folder.mkdir()
        world = publishing.World(folder)
        if published:
            empty = folder / "workspace"
            empty.mkdir()
            run = publishing.run_job(world, "publish-release", empty)
            self.assertEqual(run.status, 0, run.log)
            world.load()
        return world

    def qualify(self, world, *extra):
        world.save()
        world.clear_calls()
        output = world.root / ("qualification-" + str(len(list(world.root.glob("qualification-*")))))
        done = subprocess.run([sys.executable, "-B", str(SCRIPTS / "qualify-compiler-release.py"), *extra,
                               "--revision", world.commit, "--output", str(output), "--gh", str(world.gh)],
                              cwd=world.root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=120,
                              env={"PATH": "/usr/bin:/bin", "HOME": str(world.root)})
        return done.returncode, done.stdout.decode(), output

    def refused(self, world, message, *extra):
        status, log, output = self.qualify(world, *extra)
        self.assertNotEqual(status, 0, log)
        self.assertIn(message, log)
        self.assertFalse(output.exists(), log)
        self.assertEqual(list(world.root.glob(".stego-*")), [])
        return log

    def api(self, world):
        return world.state["api"]

    def prefix(self, world):
        return "repos/" + qualification.REPOSITORY + "/"

    def runs_key(self, world):
        return self.prefix(world) + "actions/runs?head_sha=" + world.commit + "&per_page=100"

    def test_qualification_is_at_one_exact_commit(self):
        with self.subTest("ST-DIS-UC08-R01-T01 Verification fails when the release tag points at another commit"):
            world = self.world(published=True)
            status, log, _ = self.qualify(world, "--verify-release")
            self.assertEqual(status, 0, log)
            self.api(world)[self.prefix(world) + "git/matching-refs/tags/" + world.tag] = [
                {"ref": "refs/tags/" + world.tag, "object": {"type": "commit", "sha": "b" * 40}}]
            self.refused(world, "The compiler tag does not point at the source commit", "--verify-release")

        with self.subTest("ST-DIS-UC08-R01-T02 Release verification fails when a release signature names another signed artifact run"):
            for name, data in [("stego-linux-amd64", "binary"), ("build.json", "build")]:
                for source in ["bundle", "store"]:
                    world = self.world(published=True)
                    key = COMPILER_PREDICATE + " " + publishing.sha256(getattr(world, data))
                    other = [world.signature(303)]
                    if source == "bundle":
                        world.state["bundle_attestations"] = {key: other}
                    else:
                        world.state["attestations"][key] = other
                    # Whichever signature names run 303, the two no longer name one signing run.
                    message = "does not name the signed artifact run"
                    with self.subTest(file=name, source=source):
                        self.refused(world, message, "--verify-release")
            world = self.world(published=True)
            runs = self.api(world)[self.runs_key(world)]["workflow_runs"]
            runs[1]["run_attempt"] = 2
            self.refused(world, "The release was signed by attempt 1 of CI run 202, not by its successful attempt",
                         "--verify-release")

        with self.subTest("ST-DIS-UC08-R01-T01 A run at a descendant or an ancestor commit does not qualify the commit"):
            for other in ["d" * 40, "a" * 40]:
                world = self.world()
                self.api(world)[self.runs_key(world)]["workflow_runs"][0]["head_sha"] = other
                self.refused(world, "No compiler checks run exists at the source commit")

    def test_a_missing_result_is_not_a_pass(self):
        with self.subTest("ST-DIS-UC08-R02-T01 Qualification fails when a required checks job is missing"):
            for missing in qualification.CHECKS_JOB_NAMES:
                world = self.world()
                names = [name for name in qualification.CHECKS_JOB_NAMES if name != missing]
                self.api(world)[self.prefix(world) + "actions/runs/101/jobs?per_page=100"] = world.jobs(names)
                with self.subTest(missing=missing):
                    self.refused(world, "CI run 101 is missing required jobs: " + missing)

        with self.subTest("ST-DIS-UC08-R02-T02 Qualification fails when the signed artifact run holds no artifacts or only some of them"):
            names = ["compiler-attestation-", "compiler-linux-amd64-", "compiler-provenance-"]
            artifacts_key = lambda world: self.prefix(world) + "actions/runs/202/artifacts?per_page=100"
            # Before publication, an empty list fails.
            world = self.world()
            self.api(world)[artifacts_key(world)] = {"total_count": 0, "artifacts": []}
            self.refused(world, "The signed artifact run is missing required artifacts: "
                         + ", ".join(name + world.commit for name in names))
            # After publication, an empty list less than seven days after the run fails.
            world = self.world(published=True)
            recent = (datetime.now(timezone.utc) - timedelta(days=6)).strftime("%Y-%m-%dT%H:%M:%SZ")
            self.api(world)[self.runs_key(world)]["workflow_runs"][1]["updated_at"] = recent
            self.api(world)[artifacts_key(world)] = {"total_count": 0, "artifacts": []}
            self.refused(world, "The signed artifact run is missing required artifacts: "
                         + ", ".join(name + world.commit for name in names), "--verify-release")
            # After publication, a partial list fails and names what is missing.
            world = self.world(published=True)
            listed = self.api(world)[artifacts_key(world)]
            listed["artifacts"] = listed["artifacts"][:2]
            listed["total_count"] = 2
            self.refused(world, "The signed artifact run is missing required artifacts: compiler-provenance-"
                         + world.commit, "--verify-release")

    def test_the_published_release_is_verified_as_a_consumer_sees_it(self):
        with self.subTest("ST-DIS-UC08-R03-T01 Release verification cross-checks every asset digest and the release notes"):
            world = self.world(published=True)
            status, log, output = self.qualify(world, "--verify-release")
            self.assertEqual(status, 0, log)
            record = json.loads((output / "qualification.json").read_text())
            self.assertEqual(json.loads(log), record)
            release = self.api(world)[self.prefix(world) + "releases/tags/" + world.tag]
            self.assertEqual(record["release"]["assets"],
                             {asset["name"]: asset["digest"] for asset in release["assets"]})
            self.assertEqual(record["release"]["assets"]["stego-linux-amd64"],
                             "sha256:" + publishing.sha256(world.binary))
            self.assertEqual(record["release"]["assets"]["build.json"], "sha256:" + publishing.sha256(world.build))
            self.assertTrue(record["release"]["notes_retain_signed_artifact_run"])
            self.assertTrue(record["release"]["notes_retain_compiler_sha256"])
            # The consumer path: the installer downloads each asset by its ID and checks both signatures with the
            # release bundle, as install-compiler.py does for an operator.
            calls = world.calls()
            downloads = sorted(call[-1] for call in calls if call[:1] == ["api"] and "/releases/assets/" in call[-1])
            self.assertEqual(downloads, sorted(self.prefix(world) + "releases/assets/" + str(asset["id"])
                                               for asset in release["assets"]))
            bundled = sorted(Path(call[2]).name for call in calls
                             if call[:2] == ["attestation", "verify"] and "--bundle" in call
                             and COMPILER_PREDICATE in call)
            self.assertIn("build.json", bundled)
            self.assertIn("stego-linux-amd64", bundled)
            self.assertEqual(record["release"]["verified"]["artifact"]["sha256"], publishing.sha256(world.binary))
            # Notes that lose the run or the compiler digest fail.
            for change in [lambda body: body.replace("202", "two hundred and two"),
                           lambda body: body.replace(publishing.sha256(world.binary), "")]:
                world = self.world(published=True)
                record = self.api(world)[self.prefix(world) + "releases/tags/" + world.tag]
                record["body"] = change(record["body"])
                self.refused(world, "The release notes do not retain", "--verify-release")

        with self.subTest("ST-DIS-UC08-R03-T02 Release verification proves the signed artifact run by the release signatures after its artifacts expire"):
            world = self.world(published=True)
            old = (datetime.now(timezone.utc) - timedelta(days=30)).strftime("%Y-%m-%dT%H:%M:%SZ")
            runs = self.api(world)[self.runs_key(world)]
            runs["workflow_runs"][1]["updated_at"] = old
            runs["workflow_runs"].insert(0, world.run(203, ".github/workflows/compiler-artifact.yml"))
            runs["total_count"] = len(runs["workflow_runs"])
            self.api(world)[self.prefix(world) + "actions/runs/202/artifacts?per_page=100"] = {
                "total_count": 0, "artifacts": []}
            status, log, output = self.qualify(world, "--verify-release")
            self.assertEqual(status, 0, log)
            record = json.loads((output / "qualification.json").read_text())
            self.assertEqual(record["signed_artifact"]["run_id"], 202)
            self.assertEqual(record["release"]["signing_run"], {"id": 202, "attempt": 1})
            self.assertEqual(record["signed_artifact"]["artifacts"], [])
            self.assertFalse(record["signed_artifact"]["artifacts_retained"])

    def test_a_release_is_one_immutable_tag_per_commit_with_four_assets(self):
        with self.subTest("ST-DIS-INV10-T A release is one immutable tag per commit with four assets"):
            world = self.world(published=True)
            self.api(world)[self.prefix(world) + "git/matching-refs/tags/" + world.tag][0]["object"]["sha"] = "b" * 40
            self.refused(world, "The compiler tag does not point at the source commit", "--verify-release")
            for change, message in [
                    (lambda assets: assets.pop(), "The release must contain exactly the four compiler inputs"),
                    (lambda assets: assets.append(dict(assets[0], id=399, name="extra")),
                     "The release must contain exactly the four compiler inputs"),
                    (lambda assets: assets[1].update(name="other.json"),
                     "The release contains an unknown or repeated input")]:
                world = self.world(published=True)
                change(self.api(world)[self.prefix(world) + "releases/tags/" + world.tag]["assets"])
                self.refused(world, message, "--verify-release")
            world = self.world(published=True)
            self.api(world)[self.prefix(world) + "releases/tags/" + world.tag]["immutable"] = False
            self.refused(world, "Select a published immutable release for the exact source commit", "--verify-release")

    def test_qualify_flow(self):
        with self.subTest("ST-DIS-UC08-FL-T A release is qualified at exactly its commit, verified as a consumer installs it, and each refused path fails"):
            world = self.world()
            status, log, output = self.qualify(world)
            self.assertEqual(status, 0, log)
            record = json.loads((output / "qualification.json").read_text())
            self.assertEqual(record["source_revision"], world.commit)
            self.assertEqual(record["release_decision"]["commit"], world.commit)
            self.assertTrue(record["release_decision"]["passed"])
            self.assertEqual(record["compiler_checks"]["run_id"], 101)
            self.assertEqual(record["signed_artifact"]["run_id"], 202)
            decision = [call for call in world.calls() if publishing.DECISION_PREDICATE in call]
            self.assertEqual(decision[0][decision[0].index("--source-digest") + 1], world.commit)
            qualified = {artifact["name"]: artifact["digest"] for artifact in record["signed_artifact"]["artifacts"]}
            # The release is published, and verification installs it as a consumer does.
            empty = world.root / "workspace"
            empty.mkdir()
            run = publishing.run_job(world, "publish-release", empty)
            self.assertEqual(run.status, 0, run.log)
            world.load()
            status, log, output = self.qualify(world, "--verify-release")
            self.assertEqual(status, 0, log)
            verified = json.loads((output / "qualification.json").read_text())
            self.assertEqual(verified["signed_artifact"]["artifacts"], record["signed_artifact"]["artifacts"])
            self.assertEqual(set(verified["release"]["assets"]), set(publishing.ASSETS))
            self.assertEqual(qualified["compiler-linux-amd64-" + world.commit],
                             self.api(world)[self.prefix(world) + "actions/runs/202/artifacts?per_page=100"]["artifacts"][0]["digest"])
            self.assertEqual(verified["release"]["assets"]["stego-linux-amd64"],
                             "sha256:" + verified["release"]["verified"]["artifact"]["sha256"])
            # The refused paths.
            world = self.world()
            self.api(world)[self.runs_key(world)]["workflow_runs"][0]["head_sha"] = "d" * 40
            self.refused(world, "No compiler checks run exists at the source commit")
            world = self.world()
            jobs = self.api(world)[self.prefix(world) + "actions/runs/101/jobs?per_page=100"]["jobs"]
            jobs[0]["conclusion"] = None
            self.refused(world, "CI run 101 has a job that did not succeed: " + jobs[0]["name"])
            world = self.world(published=True)
            self.api(world)[self.prefix(world) + "git/matching-refs/tags/" + world.tag][0]["object"]["sha"] = "b" * 40
            self.refused(world, "The compiler tag does not point at the source commit", "--verify-release")
            world = self.world()
            world.decide()
            self.refused(world, "No release decision attested at " + world.commit)


if __name__ == "__main__":
    unittest.main()
