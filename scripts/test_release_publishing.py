#!/usr/bin/env python3
"""Check that a passed release decision publishes the compiler release, and that qualification reads it.

The publish-release and qualify-release jobs of proof-run.yml run here as GitHub Actions runs them: each step's
script runs with bash -e, in order, with the job's environment. GitHub stands behind a fake gh on the PATH, which
answers from fixed records: the release decision and the compiler signatures, the runs and their artifacts, the
tags and the releases. jq, unzip and coreutils are the real tools. No test reaches GitHub.
"""

import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
import zipfile

import yaml


ROOT = Path(__file__).resolve().parent.parent
WORKFLOWS = ROOT / ".github" / "workflows"
REPOSITORY = "jsell-rh/stego"
DECISION_PREDICATE = "https://github.com/jsell-rh/stego/assure/release-decision/v1"
COMPILER_PREDICATE = "https://slsa.dev/provenance/v1"
ARTIFACT_RUN = 202
CHECKS_RUN = 101
ASSETS = ["stego-linux-amd64", "build.json", "SHA256SUMS", "provenance.jsonl"]

spec = importlib.util.spec_from_file_location(
    "qualification", Path(__file__).with_name("qualify-compiler-release.py"))
qualification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(qualification)

# The fake GitHub CLI. It reads its records from state.json beside it and appends each call to calls.jsonl, so it
# works also under the fixed environment that the Python scripts give GitHub CLI. It writes nothing to standard
# error on success, since the scripts read standard output and standard error together.
FAKE_GH = r'''#!PYTHON
import hashlib, json, sys
from pathlib import Path

here = Path(__file__).resolve().parent
state = json.loads((here / "state.json").read_text())
args = sys.argv[1:]
with (here / "calls.jsonl").open("a") as log:
    log.write(json.dumps(args) + "\n")


def save():
    (here / "state.json").write_text(json.dumps(state))


def fail(message):
    sys.stderr.write(message + "\n")
    sys.exit(1)


def value(name):
    return args[args.index(name) + 1] if name in args else None


def options(start, takes_value):
    found, positional, index = {}, [], start
    while index < len(args):
        if args[index] in takes_value:
            found.setdefault(args[index], []).append(args[index + 1])
            index += 2
        else:
            if args[index].startswith("-"):
                found.setdefault(args[index], []).append(None)
            else:
                positional.append(args[index])
            index += 1
    return found, positional


def sha256(data):
    return hashlib.sha256(data).hexdigest()


if args[:2] == ["attestation", "verify"]:
    path = Path(args[2])
    predicate = value("--predicate-type")
    workflow = state["signers"].get(predicate)
    if workflow is None:
        fail("no attestation of predicate type " + str(predicate))
    commit = state["commit"]
    wanted = {"--hostname": "github.com", "--repo": "jsell-rh/stego", "--signer-digest": commit,
              "--source-digest": commit, "--source-ref": "refs/heads/main",
              "--cert-identity": "https://github.com/jsell-rh/stego/" + workflow + "@refs/heads/main",
              "--cert-oidc-issuer": "https://token.actions.githubusercontent.com",
              "--digest-alg": "sha256", "--format": "json"}
    for name, expected in wanted.items():
        if value(name) != expected:
            fail("the signer policy does not hold " + name + " " + expected)
    if "--deny-self-hosted-runners" not in args:
        fail("the signer policy allows self-hosted runners")
    bundle = value("--bundle")
    if bundle is not None and sha256(Path(bundle).read_bytes()) != state["bundle"]:
        fail("the bundle does not verify")
    results = state["attestations"].get(predicate + " " + sha256(path.read_bytes()))
    if not results:
        fail("no attestation verifies")
    print(json.dumps(results))
elif args[:1] == ["api"]:
    found, positional = options(1, {"--hostname", "--method", "-H", "-f", "-F"})
    path = positional[0]
    if path == "graphql":
        fields = dict(item.split("=", 1) for item in found.get("-f", []) if not item.startswith("query="))
        release = state["latest"].get(fields.get("tag"))
        print(json.dumps({"data": {"repository": {"release": None if release is None
                                                  else {"isLatest": release}}}}))
        sys.exit(0)
    answer = state["api"].get(path)
    if isinstance(answer, dict) and "sequence" in answer:
        sequence = answer["sequence"]
        answer = sequence[0]
        if len(sequence) > 1:
            sequence.pop(0)
            save()
    if answer is None:
        fail("gh: Not Found (HTTP 404)")
    if isinstance(answer, dict) and "file" in answer:
        sys.stdout.buffer.write((here / answer["file"]).read_bytes())
    else:
        print(json.dumps(answer))
elif args[:2] == ["release", "create"]:
    found, files = options(2, {"--repo", "--target", "--title", "--notes-file", "--notes"})
    tag, files = files[0], files[1:]
    target = found["--target"][0]
    assets = []
    for number, name in enumerate(files, 301):
        data = Path(name).read_bytes()
        if state["upload_changes"] and number == 301:
            data += b"changed in upload"
        (here / ("asset-" + str(number))).write_bytes(data)
        state["api"]["repos/jsell-rh/stego/releases/assets/" + str(number)] = {"file": "asset-" + str(number)}
        assets.append({"id": number, "name": Path(name).name, "size": len(data),
                       "digest": "sha256:" + sha256(data), "state": "uploaded"})
    url = "https://github.com/jsell-rh/stego/releases/tag/" + tag
    record = {"id": 900, "tag_name": tag, "target_commitish": target, "draft": False,
              "prerelease": False, "immutable": state["immutable"], "html_url": url,
              "name": found["--title"][0], "assets": assets,
              "body": Path(found["--notes-file"][0]).read_text()}
    state["api"]["repos/jsell-rh/stego/releases/tags/" + tag] = record
    state["api"]["repos/jsell-rh/stego/releases?per_page=100"].append(record)
    state["api"]["repos/jsell-rh/stego/git/matching-refs/tags/" + tag] = [
        {"ref": "refs/tags/" + tag, "object": {"type": "commit", "sha": target}}]
    state["latest"][tag] = "--latest=false" not in found or state["latest_anyway"]
    save()
    print(url)
else:
    fail("the fake gh does not know " + " ".join(args))
'''


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def workflow(name):
    data = yaml.safe_load((WORKFLOWS / name).read_text())
    # YAML 1.1 reads the key on as true.
    if True in data:
        data["on"] = data.pop(True)
    return data


def archive(files):
    stream = io.BytesIO()
    with zipfile.ZipFile(stream, "w") as bundle:
        for name, data in files.items():
            bundle.writestr(zipfile.ZipInfo(name, (2026, 11, 1, 0, 0, 0)), data)
    return stream.getvalue()


class World:
    """GitHub as the fake gh sees it: a commit whose release gate passed and whose signed artifact run succeeded."""

    def __init__(self, root, commit="c0ffee" + "1" * 34):
        self.root = root
        self.commit = commit
        self.tag = "compiler-" + commit
        self.bin = root / "bin"
        self.bin.mkdir()
        self.gh = self.bin / "gh"
        self.gh.write_text(FAKE_GH.replace("PYTHON", sys.executable, 1))
        self.gh.chmod(0o755)
        self.binary = b"fixture compiler; do not execute\n"
        record = {"format": 1, "source_revision": commit,
                  "artifact": {"name": "stego-linux-amd64", "sha256": sha256(self.binary),
                               "size": len(self.binary)},
                  "version": {"build": {"goos": "linux", "goarch": "amd64", "revision": commit,
                                        "vcs": "git", "source_state": "clean"}}}
        self.build = json.dumps(record).encode()
        self.sums = "".join(sha256(data) + "  " + name + "\n" for name, data in
                            [("stego-linux-amd64", self.binary), ("build.json", self.build)]).encode()
        self.bundle = b'{"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json"}\n'
        self.archives = {
            "compiler-linux-amd64": {"stego-linux-amd64": self.binary, "build.json": self.build,
                                     "SHA256SUMS": self.sums},
            "compiler-attestation": {"attestation.json": self.bundle}}
        self.state = {
            "commit": commit, "bundle": sha256(self.bundle), "immutable": True, "latest": {},
            "latest_anyway": False, "upload_changes": False,
            "signers": {DECISION_PREDICATE: ".github/workflows/proof-run.yml",
                        COMPILER_PREDICATE: ".github/workflows/compiler-artifact.yml"},
            "attestations": {}, "api": {}}
        self.decide(passed=True)
        for name, data in [("stego-linux-amd64", self.binary), ("build.json", self.build)]:
            self.state["attestations"][COMPILER_PREDICATE + " " + sha256(data)] = [self.signature(ARTIFACT_RUN)]
        api = self.state["api"]
        prefix = "repos/" + REPOSITORY + "/"
        api[prefix + "actions/runs?head_sha=" + commit + "&per_page=100"] = self.runs(
            self.run(CHECKS_RUN, ".github/workflows/checks.yml"),
            self.run(ARTIFACT_RUN, ".github/workflows/compiler-artifact.yml"))
        api[prefix + "actions/runs/" + str(CHECKS_RUN) + "/jobs?per_page=100"] = self.jobs(
            qualification.CHECKS_JOB_NAMES)
        api[prefix + "actions/runs/" + str(ARTIFACT_RUN) + "/jobs?per_page=100"] = self.jobs(
            qualification.ARTIFACT_JOB_NAMES)
        self.store_artifacts()
        api[prefix + "git/matching-refs/tags/" + self.tag] = []
        api[prefix + "releases?per_page=100"] = []
        self.save()

    def store_artifacts(self, change=None):
        artifacts = []
        for number, kind in [(9001, "compiler-linux-amd64"), (9002, "compiler-attestation")]:
            data = archive(self.archives[kind])
            (self.bin / (kind + ".zip")).write_bytes(data)
            self.state["api"]["repos/" + REPOSITORY + "/actions/artifacts/" + str(number) + "/zip"] = {
                "file": kind + ".zip"}
            artifacts.append({"id": number, "name": kind + "-" + self.commit, "size_in_bytes": len(data),
                              "digest": "sha256:" + sha256(data), "expired": False})
        artifacts.append({"id": 9003, "name": "compiler-provenance-" + self.commit, "size_in_bytes": 4096,
                          "digest": "sha256:" + "5" * 64, "expired": False})
        if change:
            change(artifacts)
        self.state["api"]["repos/" + REPOSITORY + "/actions/runs/" + str(ARTIFACT_RUN)
                          + "/artifacts?per_page=100"] = {"total_count": len(artifacts), "artifacts": artifacts}

    def run(self, identity, path, **change):
        record = {"id": identity, "run_attempt": 1, "path": path, "event": "push", "head_branch": "main",
                  "head_sha": self.commit, "status": "completed", "conclusion": "success",
                  "updated_at": "2026-11-01T12:00:00Z"}
        record.update(change)
        return record

    @staticmethod
    def runs(*runs):
        return {"total_count": len(runs), "workflow_runs": list(runs)}

    @staticmethod
    def jobs(names):
        return {"total_count": len(names), "jobs": [{"name": name, "conclusion": "success"} for name in names]}

    def certificate(self, workflow, run):
        return {"subjectAlternativeName": "https://github.com/" + REPOSITORY + "/" + workflow + "@refs/heads/main",
                "issuer": "https://token.actions.githubusercontent.com",
                "sourceRepositoryDigest": self.commit, "sourceRepositoryRef": "refs/heads/main",
                "runInvocationURI": "https://github.com/" + REPOSITORY + "/actions/runs/" + str(run) + "/attempts/1"}

    def signature(self, run):
        return {"verificationResult": {
            "signature": {"certificate": self.certificate(".github/workflows/compiler-artifact.yml", run)},
            "statement": {"predicateType": COMPILER_PREDICATE}}}

    def decide(self, **predicate):
        """Keep the one release decision attested at the commit; with no fields, keep none."""
        key = DECISION_PREDICATE + " " + sha256(self.commit.encode())
        if not predicate:
            self.state["attestations"].pop(key, None)
            return
        document = {"commit": self.commit, "kind": "release", "passed": True, "reasons": [], "codeFaults": []}
        document.update(predicate)
        self.state["attestations"][key] = [{"verificationResult": {
            "signature": {"certificate": self.certificate(".github/workflows/proof-run.yml", 7000)},
            "statement": {"predicateType": DECISION_PREDICATE, "predicate": document}}}]

    def save(self):
        (self.bin / "state.json").write_text(json.dumps(self.state))

    def load(self):
        self.state = json.loads((self.bin / "state.json").read_text())
        return self.state

    def calls(self):
        log = self.bin / "calls.jsonl"
        return [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []

    def clear_calls(self):
        (self.bin / "calls.jsonl").unlink(missing_ok=True)


class Run:
    """The result of one job: its exit status, its log, its job outputs, and the outputs of each step."""

    def __init__(self, status, log, outputs, steps):
        self.status, self.log, self.outputs, self.steps = status, log, outputs, steps


def resolve(text, needs, steps):
    """Resolve the expressions a job of the test may use: the token and the outputs of steps and needs."""
    def one(match):
        expression = match.group(1).strip()
        if expression == "github.token":
            return "fixture-token"
        found = re.fullmatch(r"(steps|needs)\.([\w-]+)\.outputs\.([\w-]+)", expression)
        if found is None:
            raise AssertionError("the test does not resolve ${{ " + expression + " }}")
        source = steps if found.group(1) == "steps" else needs
        return source.get(found.group(2), {}).get(found.group(3), "")
    return re.sub(r"\$\{\{(.*?)\}\}", one, str(text))


def run_job(world, name, folder, needs=None, override=None):
    """Run each step of the job with bash -e, as GitHub Actions does with no shell named, until one fails."""
    job = workflow("proof-run.yml")["jobs"][name]
    temp = Path(tempfile.mkdtemp(prefix=name + "-", dir=world.root))
    summary = temp / "summary"
    summary.write_text("")
    environment = {"PATH": str(world.bin) + os.pathsep + os.environ.get("PATH", "/usr/bin:/bin"),
                   "HOME": str(world.root), "LANG": "C.UTF-8", "COMMIT": world.commit,
                   "RUNNER_TEMP": str(temp), "GITHUB_WORKSPACE": str(folder),
                   "GITHUB_STEP_SUMMARY": str(summary)}
    for key, text in job.get("env", {}).items():
        environment[key] = resolve(text, needs or {}, {})
    steps, log, status = {}, [], 0
    for index, step in enumerate(job["steps"]):
        if "uses" in step:
            continue
        output = temp / ("output-" + str(index))
        output.write_text("")
        env = dict(environment, GITHUB_OUTPUT=str(output))
        for key, text in step.get("env", {}).items():
            env[key] = resolve(text, needs or {}, steps)
        env.update(override or {})
        script = temp / ("step-" + str(index) + ".sh")
        script.write_text(step["run"])
        done = subprocess.run([shutil.which("bash"), "-e", str(script)], cwd=folder, env=env, stdin=subprocess.DEVNULL,
                              stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=120)
        log.append(done.stdout.decode())
        if "id" in step:
            steps[step["id"]] = dict(line.split("=", 1) for line in output.read_text().splitlines() if line)
        if done.returncode != 0:
            status = done.returncode
            break
    outputs = {key: resolve(text, needs or {}, steps) for key, text in job.get("outputs", {}).items()}
    return Run(status, "".join(log), outputs, steps)


@unittest.skipUnless(shutil.which("jq") and shutil.which("unzip"), "the jobs need jq and unzip, as the runner has")
class ReleasePublishing(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.world = World(self.root)
        # The publish job checks out nothing: it runs in an empty working folder.
        self.empty = self.root / "workspace"
        self.empty.mkdir()

    def publish(self, **override):
        self.world.save()
        self.world.clear_calls()
        return run_job(self.world, "publish-release", self.empty, override=override or None)

    def created(self):
        return [call for call in self.world.calls() if call[:2] == ["release", "create"]]

    def assert_nothing_published(self, run, message):
        self.assertNotEqual(run.status, 0, run.log)
        self.assertIn(message, run.log)
        self.assertEqual(self.created(), [], run.log)
        self.assertEqual(run.outputs["release"], "")

    def assert_published(self, run):
        self.assertEqual(run.status, 0, run.log)
        self.assertEqual(run.outputs, {"release": self.world.tag})
        self.assertIn("Compiler release published: https://github.com/jsell-rh/stego/releases/tag/"
                      + self.world.tag + "\n", run.log)

    def test_publish_and_qualify_flow(self):
        with self.subTest("ST-DIS-UC11-FL01-T A passed release decision publishes the commit's release, which qualifies through the consumer path, and every refused path publishes nothing"):
            # Main path: the decision passed and the signed artifact run succeeded.
            run = self.publish()
            self.assert_published(run)
            calls = self.world.calls()
            self.assertEqual(calls[0][:2], ["attestation", "verify"])
            self.assertIn(DECISION_PREDICATE, calls[0])
            release = self.world.load()["api"]["repos/jsell-rh/stego/releases/tags/" + self.world.tag]
            self.assertEqual(sorted(asset["name"] for asset in release["assets"]), sorted(ASSETS))
            self.assertFalse(self.world.state["latest"][self.world.tag])
            # The qualify-release job hears the job output and qualifies the published release.
            qualified = run_job(self.world, "qualify-release", ROOT,
                                needs={"publish-release": run.outputs})
            self.assertEqual(qualified.status, 0, qualified.log)
            record = json.loads(self.qualification_folder().joinpath("qualification.json").read_text())
            self.assertEqual(record["mode"], "verify-release")
            self.assertTrue(record["release_decision"]["passed"])
            self.assertEqual(record["release"]["tag_name"], self.world.tag)
            self.assertEqual(record["release"]["signing_run"], {"id": ARTIFACT_RUN, "attempt": 1})

        with self.subTest("ST-DIS-UC11-FL01-T A refused or absent release decision publishes nothing"):
            for change in [{"passed": False}, {}]:
                self.world = self.new_world("refused-" + str(len(change)))
                self.world.decide(**change)
                self.assert_nothing_published(self.publish(), "Nothing is published.")

        with self.subTest("ST-DIS-UC11-FL01-T An existing tag stops publishing and names the tag"):
            self.world = self.new_world("tagged")
            self.world.state["api"]["repos/jsell-rh/stego/git/matching-refs/tags/" + self.world.tag] = [
                {"ref": "refs/tags/" + self.world.tag, "object": {"type": "commit", "sha": self.world.commit}}]
            self.assert_nothing_published(self.publish(), "The tag " + self.world.tag + " exists.")

        with self.subTest("ST-DIS-UC11-FL01-T A mismatched asset digest stops before the release"):
            self.world = self.new_world("mismatched")
            self.world.store_artifacts(lambda artifacts: artifacts[0].update(digest="sha256:" + "0" * 64))
            self.assert_nothing_published(self.publish(), "does not match the digest that run 202 lists for it")

    def new_world(self, name):
        folder = self.root / name
        folder.mkdir()
        return World(folder)

    def qualification_folder(self):
        folders = sorted(self.world.root.glob("qualify-release-*"))
        self.assertEqual(len(folders), 1)
        return folders[0] / "qualification"

    def test_only_a_gated_commit_is_published(self):
        with self.subTest("ST-DIS-UC11-R01-T01 Publishing starts only from a passed release decision attested at the commit by proof-run.yml at main"):
            refused = [
                ("no decision", {}, "No release decision attested at"),
                ("a refused decision", {"passed": False}, "No passed release decision is attested"),
                ("a decision about another commit", {"commit": "b" * 40}, "No passed release decision is attested"),
                ("a change decision", {"kind": "change"}, "No passed release decision is attested")]
            for label, change, message in refused:
                self.world = self.new_world(label.replace(" ", "-"))
                self.world.decide(**change)
                run = self.publish()
                self.assert_nothing_published(run, message)
                # Nothing but the decision was read: the job stops before it reads a run or a release.
                self.assertEqual([call[:2] for call in self.world.calls()], [["attestation", "verify"]], label)
            # A passed decision that another workflow signed is not the trusted signer's.
            self.world = self.new_world("other-signer")
            self.world.state["signers"][DECISION_PREDICATE] = ".github/workflows/gate.yml"
            self.assert_nothing_published(self.publish(), "No release decision attested at")
            # The trusted signer's passed decision at the commit starts publishing.
            self.world = self.new_world("passed")
            self.assert_published(self.publish())

    def test_the_release_is_the_commits_own(self):
        with self.subTest("ST-DIS-UC11-R02-T01 The release is compiler-<full commit> at the full commit with exactly the four assets, each fetched by artifact ID and verified before upload"):
            run = self.publish()
            self.assert_published(run)
            created = self.created()
            self.assertEqual(len(created), 1)
            create = created[0]
            self.assertEqual(create[2], "compiler-" + self.world.commit)
            self.assertEqual(create[create.index("--target") + 1], self.world.commit)
            self.assertEqual([Path(item).name for item in create[-4:]], ASSETS)
            downloads = [call[-1] for call in self.world.calls() if call[0] == "api" and call[-1].endswith("/zip")]
            self.assertEqual(downloads, ["repos/jsell-rh/stego/actions/artifacts/9001/zip",
                                         "repos/jsell-rh/stego/actions/artifacts/9002/zip"])
            release = self.world.load()["api"]["repos/jsell-rh/stego/releases/tags/" + self.world.tag]
            published = {asset["name"]: asset["digest"] for asset in release["assets"]}
            self.assertEqual(published, {
                "stego-linux-amd64": "sha256:" + sha256(self.world.binary),
                "build.json": "sha256:" + sha256(self.world.build),
                "SHA256SUMS": "sha256:" + sha256(self.world.sums),
                "provenance.jsonl": "sha256:" + sha256(self.world.bundle)})
            verified = [call for call in self.world.calls() if call[:2] == ["attestation", "verify"]
                        and COMPILER_PREDICATE in call]
            self.assertEqual(sorted(Path(call[2]).name for call in verified), ["build.json", "stego-linux-amd64"])
            self.assertTrue(all("--bundle" in call for call in verified))
            self.assertIn(str(ARTIFACT_RUN), release["body"])
            self.assertIn(sha256(self.world.binary), release["body"])

            def other_digest(artifacts):
                artifacts[1]["digest"] = "sha256:" + "0" * 64

            def two_archives(artifacts):
                artifacts.append(dict(artifacts[0], id=9004))

            changes = [
                ("a mismatched archive digest", lambda world: world.store_artifacts(other_digest),
                 "does not match the digest that run 202 lists for it"),
                ("a second artifact of one name", lambda world: world.store_artifacts(two_archives),
                 "does not hold exactly one artifact compiler-linux-amd64-"),
                ("an expired artifact", lambda world: world.store_artifacts(
                    lambda artifacts: artifacts[0].update(expired=True)), "expired or has no valid digest"),
                ("an extra file in the archive", lambda world: (
                    world.archives["compiler-linux-amd64"].update({"notes.txt": b"x"}), world.store_artifacts()),
                 ""),
                ("a checksum list that differs", lambda world: (
                    world.archives["compiler-linux-amd64"].update({"SHA256SUMS": b"0  stego-linux-amd64\n"}),
                    world.store_artifacts()), "SHA256SUMS of run 202 does not match"),
                ("a signature from another run", lambda world: world.state["attestations"].update({
                    COMPILER_PREDICATE + " " + sha256(world.binary): [world.signature(303)]}),
                 "does not name run 202, attempt 1"),
                ("no signature", lambda world: world.state["attestations"].pop(
                    COMPILER_PREDICATE + " " + sha256(world.build)), "The signature of build.json from run 202 does not verify"),
                ("another bundle", lambda world: world.state.update(bundle="0" * 64),
                 "The signature of stego-linux-amd64 from run 202 does not verify"),
                ("a build record of another commit", lambda world: (
                    world.archives["compiler-linux-amd64"].update(self.rebuilt(world, source_revision="b" * 40)),
                    world.store_artifacts()), "The signed build record does not describe this executable")]
            for label, change, message in changes:
                self.world = self.new_world(re.sub(r"\W+", "-", label))
                change(self.world)
                run = self.publish()
                self.assertNotEqual(run.status, 0, label)
                self.assertIn(message, run.log, label)
                self.assertEqual(self.created(), [], label)

    @staticmethod
    def rebuilt(world, **change):
        # A build record that differs, with its signature and a checksum list that match it, so that only the
        # record's own content can refuse it.
        record = json.loads(world.build)
        record.update(change)
        world.build = json.dumps(record).encode()
        world.state["attestations"][COMPILER_PREDICATE + " " + sha256(world.build)] = [world.signature(ARTIFACT_RUN)]
        world.sums = "".join(sha256(data) + "  " + name + "\n" for name, data in
                             [("stego-linux-amd64", world.binary), ("build.json", world.build)]).encode()
        return {"build.json": world.build, "SHA256SUMS": world.sums}

    def artifact_runs(self, *runs):
        self.world.state["api"]["repos/jsell-rh/stego/actions/runs?head_sha=" + self.world.commit
                                + "&per_page=100"] = self.world.runs(*runs)

    def test_the_signed_artifact_run_is_found_and_awaited(self):
        with self.subTest("ST-DIS-UC11-R02-T02 The job takes the newest successful compiler-artifact.yml run on main at the commit from a push or a dispatch, waits for it within a bound, and stops naming the runs when none can succeed"):
            artifact = ".github/workflows/compiler-artifact.yml"
            world = lambda name: self.new_world(name)
            # Decoys with higher IDs than the signed run 202: each one that a filter lets through is taken first,
            # and has no artifacts, so publishing fails.
            self.world = world("decoys")
            run = self.world.run
            self.artifact_runs(
                run(ARTIFACT_RUN, artifact), run(150, artifact),
                run(300, artifact, conclusion="failure"),
                run(301, artifact, head_branch="feature"),
                run(302, artifact, event="pull_request"),
                run(303, ".github/workflows/application-build.yml"),
                run(304, artifact, status="in_progress", conclusion=None))
            published = self.publish()
            self.assert_published(published)
            self.assertIn("The signed artifact run is 202, attempt 1.", published.log)
            self.assertIn("https://github.com/jsell-rh/stego/actions/runs/202/attempts/1",
                          self.world.load()["api"]["repos/jsell-rh/stego/releases/tags/" + self.world.tag]["body"])
            # A dispatch of main counts as a push does.
            self.world = world("dispatch")
            self.artifact_runs(self.world.run(ARTIFACT_RUN, artifact, event="workflow_dispatch"))
            self.assert_published(self.publish())
            # The run is in progress at the first look and succeeds at the second: the job waits for it.
            self.world = world("waits")
            runs = "repos/jsell-rh/stego/actions/runs?head_sha=" + self.world.commit + "&per_page=100"
            self.world.state["api"][runs] = {"sequence": [
                self.world.runs(self.world.run(ARTIFACT_RUN, artifact, status="in_progress", conclusion=None)),
                self.world.runs(self.world.run(ARTIFACT_RUN, artifact))]}
            self.assert_published(self.publish(RUN_WAIT_SECONDS="60", RUN_POLL_SECONDS="0"))
            self.assertEqual(sum(1 for call in self.world.calls() if call[-1] == runs), 2)
            # A run still in progress at the bound fails the job, naming the run.
            self.world = world("bound")
            self.artifact_runs(self.world.run(ARTIFACT_RUN, artifact, status="in_progress", conclusion=None))
            self.assert_nothing_published(self.publish(RUN_WAIT_SECONDS="0", RUN_POLL_SECONDS="0"),
                                          "No run of compiler-artifact.yml at " + self.world.commit
                                          + " on main succeeded within 0 seconds. Runs at the commit: 202 (push on"
                                          " main, in_progress, no conclusion).")
            # Every run has completed and none can be taken: the job stops at once, naming the runs.
            stopped = [
                ("a failed run", [run(ARTIFACT_RUN, artifact, conclusion="failure")],
                 "202 (push on main, completed, failure)"),
                ("a pull request run", [run(ARTIFACT_RUN, artifact, event="pull_request")],
                 "202 (pull_request on main, completed, success)"),
                ("a run on another branch", [run(ARTIFACT_RUN, artifact, head_branch="feature")],
                 "202 (push on feature, completed, success)")]
            for label, runs_found, named in stopped:
                self.world = world(re.sub(r"\W+", "-", label))
                self.artifact_runs(*runs_found)
                started = time.monotonic()
                self.assert_nothing_published(self.publish(RUN_WAIT_SECONDS="30", RUN_POLL_SECONDS="1"),
                                              "Every run of compiler-artifact.yml at " + self.world.commit
                                              + " has completed, and none succeeded on main from a push or a"
                                              " dispatch. Runs at the commit: " + named + ".")
                self.assertLess(time.monotonic() - started, 20, label)

    def test_immutable_and_never_the_latest(self):
        with self.subTest("ST-DIS-UC11-R03-T01 The release is created not latest and must be immutable; an existing tag or release stops publishing and names the tag"):
            run = self.publish()
            self.assert_published(run)
            self.assertIn("--latest=false", self.created()[0])
            tag = self.world.tag
            refused = [
                ("an existing tag", lambda world: world.state["api"].update({
                    "repos/jsell-rh/stego/git/matching-refs/tags/" + tag: [
                        {"ref": "refs/tags/" + tag, "object": {"type": "commit", "sha": "b" * 40}}]}),
                 "The tag " + tag + " exists.", False),
                ("an existing release", lambda world: world.state["api"].update({
                    "repos/jsell-rh/stego/releases?per_page=100": [{"tag_name": tag, "draft": True}]}),
                 "A release " + tag + " exists.", False),
                ("a release that is not immutable", lambda world: world.state.update(immutable=False),
                 "The release " + tag + " is not immutable. The owner must enable release immutability in the repository"
                 " settings, then delete this release and its tag; the commit needs a new merge commit", True),
                ("a release that is the latest", lambda world: world.state.update(latest_anyway=True),
                 "The release " + tag + " is the latest release, and a compiler release is never the latest. The owner"
                 " must mark the previous latest release as latest explicitly (make_latest)", True),
                ("an upload that changes an asset", lambda world: world.state.update(upload_changes=True),
                 "The release " + tag + " is not exactly the four verified assets at", True)]
            for label, change, message, created in refused:
                self.world = self.new_world(re.sub(r"\W+", "-", label))
                change(self.world)
                run = self.publish()
                self.assertNotEqual(run.status, 0, label)
                self.assertIn(message, run.log, label)
                self.assertEqual(len(self.created()), int(created), label)
                self.assertEqual(run.outputs["release"], "", label)

    def test_every_gated_commit_on_main(self):
        with self.subTest("ST-DIS-UC11-R04-T01 Each push to main whose gate passed publishes in the same run; a refused decision skips publishing and leaves the run green; no decision fails it"):
            proof = workflow("proof-run.yml")
            self.assertEqual(proof["on"]["push"], {"branches": ["main"]})
            jobs = proof["jobs"]
            self.assertEqual(jobs["gate-release"]["if"],
                             "${{ !cancelled() && github.event_name == 'push' && github.ref == 'refs/heads/main' }}")
            self.assertEqual(jobs["publish-release"]["needs"], ["gate-release", "attest-release"])
            self.assertEqual(jobs["publish-release"]["if"], "${{ needs.gate-release.outputs.passed == 'true' }}")
            self.assertNotIn("continue-on-error", jobs["publish-release"])
            # A push has a concurrency group of its own, so no newer push replaces an older commit's pending run.
            for name in ["proof-run.yml", "compiler-artifact.yml", "checks.yml"]:
                self.assertIn("github.event_name == 'push' && github.sha", workflow(name)["concurrency"]["group"],
                              name)
            # The signed artifact run that publishing takes exists for every push to main, whatever it changes.
            self.assertEqual(workflow("compiler-artifact.yml")["on"]["push"], {"branches": ["main"]})
            # The checks that qualification needs run on every push too, also one that changes only documents.
            self.assertIsNone(workflow("checks.yml")["on"]["push"])
            # The gate step: passed=true runs the publish job, passed=false skips it with a green step, and no
            # decision fails the run.
            gate = next(step for step in jobs["gate-release"]["steps"] if step.get("id") == "gate")
            for code, status, outputs in [(0, 0, "passed=true\n"), (1, 0, "passed=false\n"), (2, 2, "")]:
                folder = self.root / ("gate-" + str(code))
                (folder / "release").mkdir(parents=True)
                assure = folder / "release" / "assure"
                assure.write_text("#!/bin/sh\necho decision\nexit " + str(code) + "\n")
                assure.chmod(0o755)
                output = folder / "output"
                output.write_text("")
                done = subprocess.run(["bash", "-e", "-c", gate["run"]], cwd=folder, stdout=subprocess.DEVNULL,
                                      env={"PATH": os.environ["PATH"], "COMMIT": self.world.commit,
                                           "RUNNER_TEMP": str(folder), "GITHUB_WORKSPACE": str(folder),
                                           "GITHUB_OUTPUT": str(output)})
                self.assertEqual((done.returncode, output.read_text()), (status, outputs), code)
            # A run whose gate passed publishes, in that run's publish job.
            self.assert_published(self.publish())

    def test_the_publish_job_runs_no_repository_code(self):
        with self.subTest("ST-DIS-UC11-R05-T01 The only job that can write releases checks out nothing, runs only shell with gh and jq, and publishes from an empty folder"):
            jobs = workflow("proof-run.yml")["jobs"]
            job = jobs["publish-release"]
            self.assertEqual(set(job), {"name", "needs", "if", "runs-on", "timeout-minutes", "permissions", "env",
                                        "outputs", "steps"})
            self.assertEqual(job["permissions"], {"contents": "write", "actions": "read"})
            self.assertEqual(job["env"], {"GH_TOKEN": "${{ github.token }}", "REPOSITORY": "jsell-rh/stego"})
            writers = [name for name, other in jobs.items()
                       if (other.get("permissions") or {}).get("contents") == "write"]
            self.assertEqual(writers, ["publish-release"])
            self.assertEqual([step["name"] for step in job["steps"]], [
                "Verify the passed release decision at the commit", "Find the signed artifact run at the commit",
                "Fetch the four assets by their artifact IDs and verify them", "Publish the release"])
            for step in job["steps"]:
                self.assertEqual(set(step) - {"name", "id", "env", "run"}, set(), step["name"])
                code = "\n".join(line for line in step["run"].splitlines() if not line.lstrip().startswith("#"))
                for banned in ["GITHUB_WORKSPACE", "scripts/", "python", "node", "npm", "go run", "go build",
                               "bash ", "source ", "eval ", "curl"]:
                    self.assertNotIn(banned, code, step["name"])
            # The job publishes from an empty working folder, with the commit from the workflow's environment.
            self.assertEqual(workflow("proof-run.yml")["env"]["COMMIT"],
                             "${{ github.event.pull_request.head.sha || github.sha }}")
            # With only gh, jq, unzip and coreutils on the PATH, and an empty working folder, the job publishes.
            tools = self.root / "tools"
            tools.mkdir()
            os.symlink(self.world.gh, tools / "gh")
            for tool in ["jq", "unzip", "sha256sum", "cut", "sort", "tr", "cmp", "stat", "cat", "tee", "mkdir",
                         "rm", "sleep"]:
                os.symlink(shutil.which(tool), tools / tool)
            self.assert_published(self.publish(PATH=str(tools)))
            self.assertEqual(list(self.empty.iterdir()), [])

    def test_the_publish_job_hears_the_attested_decision(self):
        with self.subTest("ST-DIS-PO06-T The publish-release job hears the attest-release job of the same run, and publishes for a passed decision only"):
            jobs = workflow("proof-run.yml")["jobs"]
            self.assertIn("attest-release", jobs["publish-release"]["needs"])
            self.assertEqual(jobs["attest-release"]["if"], "${{ needs.gate-release.outputs.passed == 'true' }}")
            self.assertEqual(jobs["publish-release"]["if"], jobs["attest-release"]["if"])
            self.assert_published(self.publish())
            self.world = self.new_world("refused")
            self.world.decide(passed=False)
            self.assert_nothing_published(self.publish(), "No passed release decision is attested")

    def test_the_qualify_job_hears_the_published_release(self):
        with self.subTest("ST-DIS-PO05-T The qualify-release job hears the published release in the same run and qualifies it with its passed decision through the consumer path"):
            job = workflow("proof-run.yml")["jobs"]["qualify-release"]
            self.assertEqual(job["needs"], "publish-release")
            self.assertEqual(job["permissions"], {"contents": "read"})
            self.assertEqual(job["steps"][0]["with"], {"ref": "${{ env.COMMIT }}", "persist-credentials": False})
            self.assertRegex(job["steps"][0]["uses"], r"^actions/checkout@[0-9a-f]{40}$")
            qualify = job["steps"][-1]
            self.assertEqual(qualify["env"], {"RELEASE": "${{ needs.publish-release.outputs.release }}"})
            self.assertIn("scripts/qualify-compiler-release.py", qualify["run"])
            self.assertIn("--revision \"$COMMIT\"", qualify["run"])
            self.assertIn("--verify-release", qualify["run"])
            published = self.publish()
            self.assert_published(published)
            run = run_job(self.world, "qualify-release", ROOT, needs={"publish-release": published.outputs})
            self.assertEqual(run.status, 0, run.log)
            record = json.loads(self.qualification_folder().joinpath("qualification.json").read_text())
            self.assertEqual(record["release_decision"]["commit"], self.world.commit)
            self.assertEqual(record["release"]["tag_name"], self.world.tag)
            # A published release with no passed decision fails qualification, and writes no record.
            self.world = self.new_world("revoked")
            published = self.publish()
            self.assert_published(published)
            self.world.load()
            self.world.decide()
            self.world.save()
            run = run_job(self.world, "qualify-release", ROOT, needs={"publish-release": published.outputs})
            self.assertNotEqual(run.status, 0)
            self.assertIn("No release decision attested at " + self.world.commit, run.log)
            self.assertFalse(self.qualification_folder().exists())
            # A release output that names another release qualifies nothing.
            self.world = self.new_world("other-release")
            published = self.publish()
            self.assert_published(published)
            self.world.clear_calls()
            run = run_job(self.world, "qualify-release", ROOT,
                          needs={"publish-release": {"release": "compiler-" + "b" * 40}})
            self.assertNotEqual(run.status, 0)
            self.assertIn("The published release compiler-" + "b" * 40 + " is not the release of " + self.world.commit,
                          run.log)
            self.assertFalse(self.qualification_folder().exists())
            self.assertFalse(any(DECISION_PREDICATE in call for call in self.world.calls()))
            # No checks run at the commit: the job says so at once and qualifies nothing.
            checks = ".github/workflows/checks.yml"
            artifact = ".github/workflows/compiler-artifact.yml"
            runs = "repos/jsell-rh/stego/actions/runs?head_sha={}&per_page=100"
            self.world = self.new_world("no-checks")
            published = self.publish()
            self.world.load()
            self.world.state["api"][runs.format(self.world.commit)] = self.world.runs(
                self.world.run(ARTIFACT_RUN, artifact))
            self.world.save()
            started = time.monotonic()
            run = run_job(self.world, "qualify-release", ROOT, needs={"publish-release": published.outputs})
            self.assertNotEqual(run.status, 0)
            self.assertIn("No checks run exists at " + self.world.commit + " on main.", run.log)
            self.assertLess(time.monotonic() - started, 20)
            self.assertFalse(self.qualification_folder().exists())
            # The checks run is in progress at the first look and completes at the second: the job waits for it.
            self.world = self.new_world("checks-wait")
            published = self.publish()
            self.world.load()
            self.world.state["api"][runs.format(self.world.commit)] = {"sequence": [
                self.world.runs(self.world.run(CHECKS_RUN, checks, status="in_progress", conclusion=None),
                                self.world.run(ARTIFACT_RUN, artifact)),
                self.world.runs(self.world.run(CHECKS_RUN, checks), self.world.run(ARTIFACT_RUN, artifact))]}
            self.world.save()
            self.world.clear_calls()
            run = run_job(self.world, "qualify-release", ROOT, needs={"publish-release": published.outputs},
                          override={"RUN_POLL_SECONDS": "0"})
            self.assertEqual(run.status, 0, run.log)
            self.assertTrue(self.qualification_folder().joinpath("qualification.json").exists())
            # The wait step looked twice, and the qualification read the runs after that.
            self.assertEqual(sum(1 for call in self.world.calls() if call[-1] == runs.format(self.world.commit)), 3)


@unittest.skipUnless(sys.platform == "linux", "qualification runs GitHub CLI as a Linux process")
class QualificationReadsTheReleaseDecision(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def qualify(self, world, name):
        world.save()
        output = world.root / name
        done = subprocess.run([sys.executable, "-B", str(ROOT / "scripts" / "qualify-compiler-release.py"),
                               "--revision", world.commit, "--output", str(output), "--gh", str(world.gh)],
                              cwd=world.root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=120,
                              env={"PATH": "/usr/bin:/bin", "HOME": str(world.root)})
        return done.returncode, done.stdout.decode(), output

    def test_qualification_verifies_a_passed_release_decision(self):
        with self.subTest("ST-DIS-UC08-R04-T01 Qualification verifies a passed release decision attested at exactly the commit by proof-run.yml at main, and fails when there is none"):
            world = World(self.root)
            status, log, output = self.qualify(world, "passed")
            self.assertEqual(status, 0, log)
            record = json.loads((output / "qualification.json").read_text())
            self.assertEqual(record["release_decision"], {
                "predicate_type": DECISION_PREDICATE,
                "signer": "https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml@refs/heads/main",
                "commit": world.commit, "kind": "release", "passed": True})
            decision = [call for call in world.calls() if DECISION_PREDICATE in call]
            self.assertEqual(len(decision), 1)
            self.assertEqual(decision[0][decision[0].index("--source-digest") + 1], world.commit)
            refused = [
                ("no decision", lambda world: world.decide(), "No release decision attested at"),
                ("a refused decision", lambda world: world.decide(passed=False), "No passed release decision"),
                ("a decision about another commit", lambda world: world.decide(commit="b" * 40),
                 "No passed release decision"),
                ("a change decision", lambda world: world.decide(kind="change"), "No passed release decision"),
                ("another signer", lambda world: world.state["signers"].update(
                    {DECISION_PREDICATE: ".github/workflows/gate.yml"}), "No release decision attested at")]
            for label, change, message in refused:
                change(world)
                status, log, output = self.qualify(world, re.sub(r"\W+", "-", label))
                self.assertNotEqual(status, 0, label)
                self.assertIn(message, log, label)
                self.assertFalse(output.exists(), label)
                world.state["signers"][DECISION_PREDICATE] = ".github/workflows/proof-run.yml"
                world.decide(passed=True)


if __name__ == "__main__":
    unittest.main()
