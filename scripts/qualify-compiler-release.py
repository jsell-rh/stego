#!/usr/bin/env python3
"""Qualify a compiler release from its passed release decision and completed CI runs."""

import argparse
from datetime import datetime, timedelta, timezone
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import tempfile


spec = importlib.util.spec_from_file_location(
    "verification", Path(__file__).with_name("verify-compiler-artifact.py"))
verification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verification)
control = verification.control
CheckError = verification.CheckError
REPOSITORY = verification.REPOSITORY
BINARY = verification.BINARY

installer_spec = importlib.util.spec_from_file_location(
    "installer", Path(__file__).with_name("install-compiler.py"))
installer = importlib.util.module_from_spec(installer_spec)
installer_spec.loader.exec_module(installer)

CHECKS_WORKFLOW = ".github/workflows/checks.yml"
# The release gate's decision (ST-DIS-UC08-R04): the proof run workflow at
# main signs a passed release decision with the commit as its subject.
DECISION_WORKFLOW = ".github/workflows/proof-run.yml"
DECISION_PREDICATE = "https://github.com/jsell-rh/stego/assure/release-decision/v1"
ARTIFACT_WORKFLOW = ".github/workflows/compiler-artifact.yml"
CHECKS_JOB_NAMES = ["examples (user-management)", "examples (user-management-rhsso)",
                    "compiler", "postgres-provisioning", "keycloak-provider",
                    "resource-state-storage"]
ARTIFACT_JOB_NAMES = ["linux-amd64", "provenance"]
# The signed artifact run must come from the main branch because its provenance
# job signs only on main-branch events that are not pull requests.
CHECKS_BRANCHES = ["main"]
RUN_PAGE = 100
RELEASE_NOTES_LIMIT = 65536
# The retention-days of the three artifacts in compiler-artifact.yml.
ARTIFACT_RETENTION = timedelta(days=7)
RUN_INVOCATION = re.compile(r"https://github\.com/" + re.escape(REPOSITORY)
                            + r"/actions/runs/([1-9][0-9]{0,18})/attempts/([1-9][0-9]{0,3})")


def api(gh, path, directory, *, limit=4 << 20):
    return control.command(
        [str(gh), "api", "--hostname", "github.com", "--method", "GET",
         "-H", "Accept: application/vnd.github+json",
         "-H", "X-GitHub-Api-Version: 2026-03-10", path],
        directory, verification.verifier_environment(), timeout=90, limit=limit)


def fetch(gh, path, directory):
    data = json.loads(api(gh, path, directory), object_pairs_hook=verification.unique_object)
    if not isinstance(data, dict):
        raise CheckError("A GitHub API response is not an object")
    return data


def run_record(run):
    if not isinstance(run, dict):
        raise CheckError("A CI run record is invalid")
    for name in ["id", "run_attempt", "path", "event", "head_branch", "head_sha", "status",
                 "conclusion"]:
        if name not in run:
            raise CheckError("A CI run record is incomplete")
    if (type(run["id"]) is not int or run["id"] <= 0
            or type(run["run_attempt"]) is not int or run["run_attempt"] <= 0):
        raise CheckError("A CI run identity is invalid")
    return run


def run_age(run):
    updated = run.get("updated_at")
    try:
        if not isinstance(updated, str):
            raise ValueError(updated)
        moment = datetime.strptime(updated, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    except ValueError:
        raise CheckError("A CI run time is invalid") from None
    return datetime.now(timezone.utc) - moment


def runs_at(gh, revision, directory):
    record = fetch(gh, "repos/" + REPOSITORY + "/actions/runs?head_sha=" + revision
                   + "&per_page=" + str(RUN_PAGE), directory)
    total, runs = record.get("total_count"), record.get("workflow_runs")
    if type(total) is not int or total < 0 or not isinstance(runs, list):
        raise CheckError("The CI run list is invalid")
    if total > RUN_PAGE:
        raise CheckError("The source commit has more CI runs than one bounded page")
    if len(runs) != total:
        raise CheckError("The CI run list is incomplete")
    return [run_record(run) for run in runs]


def select_run(runs, workflow, branches, label, identity=None):
    # Without an identity, take the newest successful run. With one, take
    # only that run: the run that the release signatures name.
    candidates = [run for run in runs
                  if run["path"] == workflow and run["head_branch"] in branches
                  and (identity is None or run["id"] == identity)]
    if not candidates:
        raise CheckError("No " + label + " run exists at the source commit")
    for run in candidates:
        if (run["status"] == "completed" and run["conclusion"] == "success"
                and run["event"] in ["push", "workflow_dispatch"]):
            return run
    raise CheckError("No completed successful " + label
                     + " run exists at the source commit")


def run_jobs(gh, run, directory):
    record = fetch(gh, "repos/" + REPOSITORY + "/actions/runs/"
                   + str(run["id"]) + "/jobs?per_page=" + str(RUN_PAGE), directory)
    total, jobs = record.get("total_count"), record.get("jobs")
    if type(total) is not int or total < 0 or not isinstance(jobs, list):
        raise CheckError("A CI job list is invalid")
    if total > RUN_PAGE:
        raise CheckError("A CI run has more jobs than one bounded page")
    if len(jobs) != total:
        raise CheckError("A CI job list is incomplete")
    names = []
    for job in jobs:
        if not isinstance(job, dict) or not isinstance(job.get("name"), str):
            raise CheckError("A CI job record is invalid")
        names.append(job["name"])
    return jobs, names


def require_jobs(jobs, names, expected, run_id):
    missing = [name for name in expected if name not in names]
    if missing:
        raise CheckError("CI run " + str(run_id) + " is missing required jobs: "
                         + ", ".join(missing))
    for job in jobs:
        if job["name"] in expected and job.get("conclusion") != "success":
            raise CheckError("CI run " + str(run_id) + " has a job that did not succeed: "
                             + job["name"])
    return [{"name": job["name"], "conclusion": job["conclusion"]}
            for job in jobs if job["name"] in expected]


def run_artifacts(gh, run, directory):
    record = fetch(gh, "repos/" + REPOSITORY + "/actions/runs/"
                   + str(run["id"]) + "/artifacts?per_page=" + str(RUN_PAGE), directory)
    total, artifacts = record.get("total_count"), record.get("artifacts")
    if type(total) is not int or total < 0 or not isinstance(artifacts, list):
        raise CheckError("A CI artifact list is invalid")
    if total > RUN_PAGE:
        raise CheckError("A CI run has more artifacts than one bounded page")
    if len(artifacts) != total:
        raise CheckError("A CI artifact list is incomplete")
    return artifacts


def require_artifacts(artifacts, revision, run_id, require_current, removable=False):
    limits = {"compiler-linux-amd64-" + revision: 100 << 20,
              "compiler-attestation-" + revision: 4 << 20,
              "compiler-provenance-" + revision: 100 << 20}
    found = {}
    for artifact in artifacts:
        if not isinstance(artifact, dict):
            raise CheckError("A CI artifact record is invalid")
        name = artifact.get("name")
        if not isinstance(name, str) or name not in limits or name in found:
            raise CheckError("The signed artifact run holds an unknown or repeated artifact")
        size, digest, expired = (artifact.get("size_in_bytes"), artifact.get("digest"),
                                 artifact.get("expired"))
        if (type(size) is not int or not 0 < size <= limits[name]
                or not isinstance(digest, str)
                or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest)
                or type(expired) is not bool):
            raise CheckError("A signed artifact identity, size, digest, or state is invalid")
        if require_current and expired:
            raise CheckError("A signed artifact of CI run " + str(run_id)
                             + " expired before qualification")
        found[name] = {"name": name, "digest": digest, "size_in_bytes": size,
                       "expired": expired}
    if not found and removable:
        # GitHub removes an expired artifact from the run's list. After
        # publication, verify_published_release requires the signatures of the
        # release to name this run instead. A partial list still fails.
        return []
    missing = sorted(set(limits) - set(found))
    if missing:
        raise CheckError("The signed artifact run is missing required artifacts: "
                         + ", ".join(missing))
    return [found[name] for name in limits]


def tag_ref(gh, revision, directory):
    data = json.loads(api(gh, "repos/" + REPOSITORY + "/git/matching-refs/tags/compiler-"
                          + revision, directory), object_pairs_hook=verification.unique_object)
    if not isinstance(data, list):
        raise CheckError("The tag reference list is invalid")
    return data


def attested_runs(gh, path, revision, bundle=None):
    """Return the (run, attempt) pairs whose signatures authenticate one file."""
    try:
        # The installer's signer policy. Without a bundle, GitHub CLI reads the
        # attestation store of the repository. With the release bundle, it
        # reads what a consumer reads.
        raw = verification.authenticate(gh, path, bundle, revision,
                                        verification.verifier_environment())
    except CheckError:
        raise CheckError("No signature authenticates " + path.name + " at the source commit"
                         + ("" if bundle is None else " in the release bundle")) from None
    results = json.loads(raw, object_pairs_hook=verification.unique_object)
    if not isinstance(results, list) or not results:
        raise CheckError("The signature verification result for " + path.name + " is invalid")
    runs = set()
    for result in results:
        outcome = result.get("verificationResult") if isinstance(result, dict) else None
        signature = outcome.get("signature") if isinstance(outcome, dict) else None
        certificate = signature.get("certificate") if isinstance(signature, dict) else None
        if not isinstance(certificate, dict):
            raise CheckError("A signature of " + path.name + " has no certificate")
        if certificate.get("sourceRepositoryDigest") != revision:
            raise CheckError("A signature of " + path.name + " names another commit")
        invocation = certificate.get("runInvocationURI")
        match = (RUN_INVOCATION.fullmatch(invocation)
                 if isinstance(invocation, str) else None)
        if match is None:
            raise CheckError("A signature of " + path.name + " does not name its signing run")
        runs.add((int(match.group(1)), int(match.group(2))))
    return runs


def release_decision(gh, revision, stage):
    """Verify the passed release decision attested at exactly the commit."""
    # The subject of the decision is the commit ID. The file holds exactly its
    # bytes, so its SHA-256 is the subject digest that GitHub CLI verifies.
    subject = stage / "release-decision-subject"
    subject.write_bytes(revision.encode())
    try:
        raw = verification.authenticate_subject(
            gh, subject, None, revision, verification.verifier_environment(), REPOSITORY,
            DECISION_WORKFLOW, verification.REFERENCE, predicate=DECISION_PREDICATE)
    except CheckError:
        raise CheckError("No release decision attested at " + revision + " by "
                         + DECISION_WORKFLOW + " at " + verification.REFERENCE
                         + " verifies; a release without a passed decision is not qualified") from None
    results = json.loads(raw, object_pairs_hook=verification.unique_object)
    if not isinstance(results, list) or not results:
        raise CheckError("The release decision verification result is invalid")
    for result in results:
        outcome = result.get("verificationResult") if isinstance(result, dict) else None
        signature = outcome.get("signature") if isinstance(outcome, dict) else None
        certificate = signature.get("certificate") if isinstance(signature, dict) else None
        statement = outcome.get("statement") if isinstance(outcome, dict) else None
        if not isinstance(certificate, dict) or not isinstance(statement, dict):
            raise CheckError("A release decision signature has no certificate or statement")
        if certificate.get("sourceRepositoryDigest") != revision:
            raise CheckError("A release decision signature names another commit")
        if statement.get("predicateType") != DECISION_PREDICATE:
            raise CheckError("A release decision statement has another predicate type")
        predicate = statement.get("predicate")
        if (isinstance(predicate, dict) and predicate.get("commit") == revision
                and predicate.get("kind") == "release" and predicate.get("passed") is True):
            return {"predicate_type": DECISION_PREDICATE,
                    "signer": "https://github.com/" + REPOSITORY + "/" + DECISION_WORKFLOW
                    + "@" + verification.REFERENCE,
                    "commit": revision, "kind": "release", "passed": True}
    raise CheckError("No passed release decision is attested at " + revision)


def qualify(revision, output, gh, verify_release=False):
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise CheckError("Select a full lowercase source commit ID")
    if not gh.is_absolute() or not gh.is_file() or not os.access(gh, os.X_OK):
        raise CheckError("Select an installed GitHub CLI by its absolute path")
    output = output.absolute()
    if output.exists() or output.is_symlink() or not output.parent.is_dir():
        raise CheckError("The result directory must be new and its parent must exist")
    branches = list(CHECKS_BRANCHES)
    if verify_release:
        # A published tag also triggers the full checks at the exact commit.
        branches.append("compiler-" + revision)
    with tempfile.TemporaryDirectory(prefix=".stego-qualification-", dir=output.parent) as temporary:
        stage = Path(temporary)
        decision = release_decision(gh, revision, stage)
        runs = runs_at(gh, revision, stage)
        checks = select_run(runs, CHECKS_WORKFLOW, branches, "compiler checks")
        release = None
        if verify_release:
            # A published release names its signed artifact run in its
            # signatures. Another successful run at the commit is not it.
            release = verify_published_release(gh, revision, stage)
            signer = release["signing_run"]
            artifact = select_run(runs, ARTIFACT_WORKFLOW, CHECKS_BRANCHES,
                                  "signed artifact", signer["id"])
            if artifact["run_attempt"] != signer["attempt"]:
                raise CheckError("The release was signed by attempt " + str(signer["attempt"])
                                 + " of CI run " + str(artifact["id"])
                                 + ", not by its successful attempt")
        else:
            artifact = select_run(runs, ARTIFACT_WORKFLOW, CHECKS_BRANCHES,
                                  "signed artifact")
        checks_jobs, checks_names = run_jobs(gh, checks, stage)
        checks_result = require_jobs(checks_jobs, checks_names, CHECKS_JOB_NAMES,
                                     checks["id"])
        artifact_jobs, artifact_names = run_jobs(gh, artifact, stage)
        artifact_result = require_jobs(artifact_jobs, artifact_names,
                                       ARTIFACT_JOB_NAMES, artifact["id"])
        # An empty list is expiry only when the retention time has passed since
        # the run; earlier, it is a deletion or a fault, and it fails.
        removable = verify_release and run_age(artifact) > ARTIFACT_RETENTION
        artifacts = require_artifacts(run_artifacts(gh, artifact, stage), revision,
                                      artifact["id"], require_current=not verify_release,
                                      removable=removable)
        if not verify_release:
            refs = tag_ref(gh, revision, stage)
            if refs:
                raise CheckError("The compiler tag already exists; never replace a release")
        result = {
            "format": 1,
            "mode": "verify-release" if verify_release else "prepare",
            "repository": REPOSITORY,
            "source_revision": revision,
            "release_decision": decision,
            "compiler_checks": {
                "run_id": checks["id"], "event": checks["event"],
                "head_branch": checks["head_branch"], "conclusion": checks["conclusion"],
                "jobs": checks_result},
            "signed_artifact": {
                "run_id": artifact["id"], "run_attempt": artifact["run_attempt"],
                "event": artifact["event"],
                "head_branch": artifact["head_branch"], "conclusion": artifact["conclusion"],
                "jobs": artifact_result, "artifacts": artifacts,
                "artifacts_retained": bool(artifacts)},
            "release": release,
            "downloaded_compiler_executed": False,
        }
        output.mkdir(mode=0o700)
        (output / "qualification.json").write_text(json.dumps(result, indent=2) + "\n")
        return result


def verify_published_release(gh, revision, stage):
    refs = tag_ref(gh, revision, stage)
    if len(refs) != 1:
        raise CheckError("The compiler release tag must exist exactly once")
    target = refs[0].get("object") if isinstance(refs[0], dict) else None
    if (not isinstance(target, dict) or target.get("type") != "commit"
            or target.get("sha") != revision):
        raise CheckError("The compiler tag does not point at the source commit")
    # Download and authenticate the published release exactly as a consumer does.
    package = stage / "release-verification"
    verified = installer.install(revision, package, gh)
    raw = api(gh, "repos/" + REPOSITORY + "/releases/tags/compiler-" + revision, stage)
    record = json.loads(raw, object_pairs_hook=verification.unique_object)
    assets = installer.release_assets(record, revision)
    binary_digest = "sha256:" + verified["artifact"]["sha256"]
    build_digest = "sha256:" + verified["build_record_sha256"]
    if assets[BINARY]["digest"] != binary_digest:
        raise CheckError("The release compiler digest differs from the verified bytes")
    if assets["build.json"]["digest"] != build_digest:
        raise CheckError("The release build record digest differs from the verified bytes")
    # Run artifacts expire. The signatures name the run and the attempt that
    # signed these bytes. The release bundle is what a consumer verifies, and
    # it must name one signing run for both files.
    signers = attested_runs(gh, package / BINARY, revision, package / "provenance.jsonl")
    if len(signers) != 1:
        raise CheckError("The release bundle must name exactly one signing run")
    signer = next(iter(signers))
    if signer not in attested_runs(gh, package / "build.json", revision,
                                   package / "provenance.jsonl"):
        raise CheckError("The signature of build.json in the release bundle"
                         " does not name the signed artifact run")
    # The attestation store is checked too: GitHub lets an owner delete an
    # attestation, and a deletion is a revocation that the bundle cannot show.
    for name in [BINARY, "build.json"]:
        if signer not in attested_runs(gh, package / name, revision):
            raise CheckError("The signature of " + name + " in the attestation store"
                             " does not name the signed artifact run")
    notes = record.get("body")
    if not isinstance(notes, str) or not 0 < len(notes) <= RELEASE_NOTES_LIMIT:
        raise CheckError("The release record must retain its evidence notes")
    if str(signer[0]) not in notes:
        raise CheckError("The release notes do not retain the signed artifact run")
    if verified["artifact"]["sha256"] not in notes:
        raise CheckError("The release notes do not retain the compiler digest")
    return {
        "id": record["id"], "tag_name": record["tag_name"],
        "immutable": record["immutable"],
        "assets": {name: assets[name]["digest"] for name in assets},
        "notes_retain_signed_artifact_run": True,
        "notes_retain_compiler_sha256": True,
        "signatures_name_signed_artifact_run": signer[0],
        "signing_run": {"id": signer[0], "attempt": signer[1]},
        "verified": verified,
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--gh", type=Path, required=True)
    parser.add_argument("--verify-release", action="store_true",
                        help="Verify an already published release instead of a candidate")
    args = parser.parse_args()
    def interrupted(_number, _frame):
        raise CheckError("Compiler release qualification was interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    os.umask(0o077)
    print(json.dumps(qualify(args.revision, args.output, args.gh,
                             verify_release=args.verify_release), indent=2))


if __name__ == "__main__":
    try:
        main()
    except (CheckError, OSError, ValueError, TypeError, AttributeError,
            subprocess.TimeoutExpired) as error:
        raise SystemExit(str(error)) from None
