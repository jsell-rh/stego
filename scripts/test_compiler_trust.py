#!/usr/bin/env python3
"""Check compiler qualification and installation as an operator and CI run them: each script as a process.

GitHub stands behind the fake gh of test_release_publishing.py, which answers from fixed records: the release
decision and the signatures (the local stand-in for the provider's trusted signer), the runs, their jobs and
artifacts, the tags and the releases. The publish-release job of proof-run.yml publishes the release first, as CI
does. No test reaches GitHub. These tests run in the Python suites, not in the bounded checks of the artifact and
qualification jobs.
"""

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


def load(name, file):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(file))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


qualification = load("qualification", "qualify-compiler-release.py")
installer = load("installer", "install-compiler.py")
publishing = load("publishing", "test_release_publishing.py")
SCRIPTS = Path(__file__).resolve().parent
COMPILER_PREDICATE = "https://slsa.dev/provenance/v1"


@unittest.skipUnless(sys.platform == "linux" and shutil.which("jq") and shutil.which("unzip"),
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


# A machine that reports aarch64: Python on it answers platform.machine() with aarch64. The installer runs
# unchanged; only the host's answer differs.
AARCH64 = "import platform\nplatform.machine = lambda: 'aarch64'\n"


@unittest.skipUnless(sys.platform == "linux" and shutil.which("jq") and shutil.which("unzip"),
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
